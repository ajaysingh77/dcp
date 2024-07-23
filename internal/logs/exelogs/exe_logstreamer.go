// Copyright (c) Microsoft Corporation. All rights reserved.

package exelogs

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/logs"
)

var exeStreamer = &executableLogStreamer{
	lock:          &sync.Mutex{},
	activeStreams: make(map[types.UID]map[streamID_T]context.CancelFunc),
}

type streamID_T = uint32

type executableLogStreamer struct {
	lock          *sync.Mutex
	activeStreams map[types.UID]map[streamID_T]context.CancelFunc
	nextStreamID  uint32
}

func LogStreamer() *executableLogStreamer {
	return exeStreamer
}

// StreamLogs implements v1.ResourceLogStreamer.
func (els *executableLogStreamer) StreamLogs(
	requestCtx context.Context,
	dest io.Writer,
	obj apiserver_resource.Object,
	opts *apiv1.LogOptions,
	log logr.Logger,
) (apiv1.ResourceStreamStatus, <-chan struct{}, error) {
	status := apiv1.ResourceStreamStatusNotReady

	exe, isExe := obj.(*apiv1.Executable)
	if !isExe {
		return status, nil, apierrors.NewInternalError(fmt.Errorf("parent storage returned object of wrong type (not Executable): %s", obj.GetObjectKind().GroupVersionKind().String()))
	}

	deletionRequested := exe.DeletionTimestamp != nil && !exe.DeletionTimestamp.IsZero()
	if deletionRequested {
		return status, nil, apierrors.NewBadRequest("Executable is being deleted")
	}

	var logFilePath string
	switch opts.Source {
	case "", string(apiv1.LogStreamSourceStdout):
		logFilePath = exe.Status.StdOutFile
	case string(apiv1.LogStreamSourceStderr):
		logFilePath = exe.Status.StdErrFile
	default:
		return apiv1.ResourceStreamStatusDone, nil, nil
	}

	if logFilePath != "" {
		doneCh := make(chan struct{})
		status = apiv1.ResourceStreamStatusStreaming

		go func() {
			defer close(doneCh)

			els.lock.Lock()
			watchCtx, watchCtxCancel := context.WithCancel(requestCtx)
			streamId := atomic.AddUint32(&els.nextStreamID, 1)
			objStreams, found := els.activeStreams[exe.UID]
			if !found {
				objStreams = make(map[streamID_T]context.CancelFunc)
				els.activeStreams[exe.UID] = objStreams
			}
			objStreams[streamId] = watchCtxCancel
			els.lock.Unlock()

			logWatchErr := logs.WatchLogs(watchCtx, logFilePath, dest, logs.WatchLogOptions{Follow: opts.Follow, Timestamps: opts.Timestamps})
			if logWatchErr != nil {
				log.Error(logWatchErr, "failed to watch Executable logs",
					"LogFilePath", logFilePath,
					"Executable", exe.NamespacedName().String(),
					"LogOptions", opts.String(),
				)
			}

			els.lock.Lock()
			objStreams, found = els.activeStreams[exe.UID]
			if found {
				watchCtxCancel, found = objStreams[streamId]
				if found {
					delete(objStreams, streamId)
					watchCtxCancel()
				} else {
					// This should not really happen. Either we should have the cancellation function for specific stream,
					// or (if the Executable was deleted), the whole map[streamID_T]context.CancelFunc should have been deleted.
					log.V(1).Info("stream cancellation function not found in active streams for Executable",
						"StreamID", streamId,
						"Executable", exe.NamespacedName().String(),
						"LogOptions", opts.String(),
					)
				}
			}
			els.lock.Unlock()
		}()

		return status, doneCh, nil
	}

	return status, nil, nil
}

// OnResourceDeleted implements v1.ResourceLogStreamer.
func (els *executableLogStreamer) OnResourceUpdated(evt apiv1.ResourceWatcherEvent, log logr.Logger) {
	exe, isExe := evt.Object.(*apiv1.Executable)
	if !isExe {
		log.V(1).Info("Executable watcher received a resource notification for an object that is not an Executable", "ObjectKind", evt.Object.GetObjectKind().GroupVersionKind().String())
		return
	}

	els.lock.Lock()
	defer els.lock.Unlock()

	if evt.Type == watch.Deleted {
		objStreams, found := els.activeStreams[exe.UID]
		if found {
			delete(els.activeStreams, exe.UID)

			for _, cancel := range objStreams {
				cancel()
			}
		}
	}
}

func (els *executableLogStreamer) Dispose() error {
	els.lock.Lock()
	defer els.lock.Unlock()

	for _, objStreams := range els.activeStreams {
		for _, cancel := range objStreams {
			cancel()
		}
	}

	clear(els.activeStreams)
	return nil
}

var _ apiv1.ResourceLogStreamer = (*executableLogStreamer)(nil)
