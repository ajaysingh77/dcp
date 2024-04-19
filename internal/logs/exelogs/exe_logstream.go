// Copyright (c) Microsoft Corporation. All rights reserved.

package exelogs

import (
	"context"
	"fmt"
	"io"

	"github.com/go-logr/logr"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/logs"
)

var exeStreamer = &executableLogStreamer{}

type executableLogStreamer struct{}

func LogStreamer() *executableLogStreamer {
	return exeStreamer
}

// StreamLogs implements v1.ResourceLogStreamer.
func (*executableLogStreamer) StreamLogs(
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

	if opts.Timestamps {
		// Timestamps are not implemented yet
		return status, nil, apierrors.NewBadRequest("timestamps are not supported yet for Executable logs")
	}

	var logFilePath string
	switch opts.Source {
	case "", string(apiv1.LogStreamSourceStdout):
		logFilePath = exe.Status.StdOutFile
	case string(apiv1.LogStreamSourceStderr):
		logFilePath = exe.Status.StdErrFile
	case string(apiv1.LogStreamSourceStartupStdout):
		return status, nil, apierrors.NewBadRequest("Startup logs are not supported yet for Executable logs")
	case string(apiv1.LogStreamSourceStartupStderr):
		return status, nil, apierrors.NewBadRequest("Startup logs are not supported yet for Executable logs")
	default:
		return status, nil, apierrors.NewBadRequest(fmt.Sprintf("Invalid log source '%s'. Supported log sources are '%s' and '%s'", opts.Source, apiv1.LogStreamSourceStdout, apiv1.LogStreamSourceStderr))
	}

	if logFilePath != "" {
		doneCh := make(chan struct{})
		status = apiv1.ResourceStreamStatusStreaming

		go func() {
			defer close(doneCh)

			logWatchErr := logs.WatchLogs(requestCtx, logFilePath, dest, logs.WatchLogOptions{Follow: opts.Follow})
			if logWatchErr != nil {
				log.Error(logWatchErr, "failed to watch Executable logs", "LogFilePath", logFilePath)
			}
		}()

		return status, doneCh, nil
	}

	return status, nil, nil
}

// OnResourceDeleted implements v1.ResourceLogStreamer.
func (*executableLogStreamer) OnResourceDeleted(obj apiserver_resource.Object, log logr.Logger) {
	// Nothing to cleanup for executable logs
}

var _ apiv1.ResourceLogStreamer = (*executableLogStreamer)(nil)
