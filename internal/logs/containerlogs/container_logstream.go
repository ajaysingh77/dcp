// Copyright (c) Microsoft Corporation. All rights reserved.

package containerlogs

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/contextdata"
	"github.com/microsoft/usvc-apiserver/internal/logs"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

var (
	// A map of Container resource UID to the log descriptor for the container.
	// Used for streaming logs from the container.
	containerLogs = syncmap.Map[types.UID, *logs.LogDescriptor]{}

	containerOrchestrator containers.ContainerOrchestrator = nil
	containerWatcher      watch.Interface                  = nil
	lock                                                   = &sync.Mutex{}
)

const (
	logCaptureShutdownTimeout = 2 * time.Second
)

func CreateContainerLogStream(
	ctx context.Context,
	obj apiserver_resource.Object,
	opts *apiv1.LogOptions,
	parentKindStorage registry_rest.StandardStorage,
) (io.ReadCloser, error) {
	ctr, isContainer := obj.(*apiv1.Container)
	if !isContainer {
		return nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type (not Container): %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := ctr.DeletionTimestamp != nil && !ctr.DeletionTimestamp.IsZero()
	if deletionRequested {
		return nil, apierrors.NewBadRequest("Container is being deleted")
	}

	switch ctr.Status.State {

	case apiv1.ContainerStateUnknown, apiv1.ContainerStateNotFound, apiv1.ContainerStateRemoved:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("logs are not available for Container in state %s", ctr.Status.State))

	case apiv1.ContainerStatePending, apiv1.ContainerStateStarting:
		// TODO: need to wait for the logs to be availabe if opts.Follow is true
		return io.NopCloser(strings.NewReader("")), nil
	}

	if ctr.Status.ContainerID == "" {
		// TODO: need to wait for the logs to be availabe if opts.Follow is true
		return io.NopCloser(strings.NewReader("")), nil
	}

	if opts.Source != "" && opts.Source != string(apiv1.LogStreamSourceStdout) && opts.Source != string(apiv1.LogStreamSourceStderr) {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("Invalid log source '%s'. Supported log sources are '%s' and '%s'", opts.Source, apiv1.LogStreamSourceStdout, apiv1.LogStreamSourceStderr))
	}

	hostLifetimeCtx := contextdata.GetHostLifetimeContext(ctx)
	log := contextdata.GetContextLogger(ctx)
	logFolder := os.TempDir()
	if dcpSessionDir, found := os.LookupEnv(logger.DCP_SESSION_FOLDER); found {
		logFolder = dcpSessionDir
	}

	containerWatcherErr := ensureContainerWatcher(hostLifetimeCtx, parentKindStorage, log)
	if containerWatcherErr != nil {
		log.Error(containerWatcherErr, "failed to create Container watcher")
		return nil, apierrors.NewInternalError(containerWatcherErr)
	}

	ld, ldExisted := containerLogs.LoadOrStoreNew(ctr.UID, func() *logs.LogDescriptor {
		logDescriptorCtx, cancel := context.WithCancel(hostLifetimeCtx)
		return logs.NewLogDescriptor(logDescriptorCtx, cancel, ctr.NamespacedName(), ctr.UID)
	})

	if !ldExisted {
		// Need to start log capturing for the container

		co, coErr := ensureContainerOrchestrator(ctx, log)
		if coErr != nil {
			log.Error(coErr, "failed to get Container orchestrator")
			containerLogs.Delete(ctr.UID)
			return nil, apierrors.NewInternalError(coErr)
		}

		stdOutWriter, stdErrWriter, logCaptureErr := ld.EnableLogCapturing(logFolder)
		if logCaptureErr != nil {
			log.Error(logCaptureErr, "Failed to enable log capturing for Container", "Container", ctr.NamespacedName())
			containerLogs.Delete(ctr.UID)
			return nil, apierrors.NewInternalError(logCaptureErr)
		}

		logCaptureErr = co.CaptureContainerLogs(ld.Context, ctr.Status.ContainerID, stdOutWriter, stdErrWriter, containers.StreamContainerLogsOptions{
			Follow: true,
		})
		if logCaptureErr != nil {
			log.Error(logCaptureErr, "Failed to start capturing logs for Container", "Container", ctr.NamespacedName())
			// Deliberately using the request context here for the timeout
			shutdownCtx, cancel := context.WithTimeout(ctx, logCaptureShutdownTimeout)
			defer cancel()
			disposeErr := ld.Dispose(shutdownCtx)
			if disposeErr != nil {
				log.Error(disposeErr, "Failed to dispose log descriptor after failed log capture", "Container", ctr.NamespacedName())
			}
			containerLogs.Delete(ctr.UID)
			return nil, apierrors.NewInternalError(logCaptureErr)
		}
	}

	stdOutPath, stdErrPath, err := ld.LogConsumerStarting()
	if err != nil {
		// This can happen if the log descriptor is being disposed because the Container is being deleted
		// We just report not found in this case
		return nil, apierrors.NewNotFound(ctr.GetGroupVersionResource().GroupResource(), ctr.NamespacedName().Name)
	}

	var logFilePath string
	if opts.Source == string(apiv1.LogStreamSourceStdout) {
		logFilePath = stdOutPath
	} else {
		logFilePath = stdErrPath
	}

	reader, writer := io.Pipe()
	go func() {
		logWatchErr := logs.WatchLogs(ld.Context, logFilePath, writer, logs.WatchLogOptions{Follow: opts.Follow})
		if logWatchErr != nil {
			log.Error(logWatchErr, "Failed to watch Container logs",
				"Container", ctr.NamespacedName(),
				"LogFilePath", logFilePath,
			)
		}
		ld.LogConsumerStopped()
	}()
	// We also need a singleton watcher for the Containers, to handle Container deletion.
	//
	// When a Container is removed (as reported by the watcher),
	// we want to stop the log stream and any log watchers by cancelling the context,
	// and then remove the stdout/stderr file objects. To track the watchers and log stream,
	// we will have an atomic count of the number of watchers that includes the log streamer
	// (part of the log descriptor). We wait (with a timeout) for the count to go to zero
	// and then delete the file objects.

	return reader, nil
}

func ensureContainerOrchestrator(ctx context.Context, log logr.Logger) (containers.ContainerOrchestrator, error) {
	lock.Lock()
	defer lock.Unlock()
	if containerOrchestrator != nil {
		return containerOrchestrator, nil
	}

	pe := contextdata.GetProcessExecutor(ctx)
	co, err := container_flags.GetContainerOrchestrator(ctx, log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagArg()), pe)
	if err != nil {
		return nil, err
	}

	containerOrchestrator = co
	return co, nil
}

func ensureContainerWatcher(hostLifetimeCtx context.Context, containerStorage registry_rest.StandardStorage, log logr.Logger) error {
	lock.Lock()
	defer lock.Unlock()
	if containerWatcher != nil {
		return nil
	}

	opts := metainternalversion.ListOptions{
		Watch: true,
	}
	w, err := containerStorage.Watch(hostLifetimeCtx, &opts)
	if err != nil {
		return err
	}
	containerWatcher = w
	go processContainerEvents(hostLifetimeCtx, containerWatcher, log)

	return nil
}

func processContainerEvents(hostLifetimeCtx context.Context, containerWatcher watch.Interface, log logr.Logger) {
	for {
		select {

		case <-hostLifetimeCtx.Done():
			containerWatcher.Stop()
			return

		case event, isChanOpen := <-containerWatcher.ResultChan():
			if !isChanOpen {
				return
			}

			if event.Type != watch.Deleted {
				continue
			}

			ctr, isContainer := event.Object.(*apiv1.Container)
			if !isContainer {
				log.V(1).Info("container watcher received a Deleted event for an object that is not a Container", "ObjectKind", event.Object.GetObjectKind().GroupVersionKind().String())
				continue
			}

			ld, deleted := containerLogs.LoadAndDelete(ctr.UID)
			if !deleted {
				// We weren't watching logs of this container, so there is nothing to do
				continue
			}

			// Need to stop the log streamer and any log watchers for this container as it is being deleted
			// Run the disposal of the log descriptor in a separate goroutine to avoid blocking the container watcher

			go func(ld *logs.LogDescriptor, containerName types.NamespacedName) {
				shutdownCtx, cancel := context.WithTimeout(hostLifetimeCtx, logCaptureShutdownTimeout)
				defer cancel()
				disposeErr := ld.Dispose(shutdownCtx)
				if disposeErr != nil {
					log.Error(disposeErr, "Failed to dispose log descriptor after container was deleted", "Container", containerName.String())
				}
			}(ld, ctr.NamespacedName())
		}
	}
}
