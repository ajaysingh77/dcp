// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

const (
	pollImmediately = true // Don't wait before polling for the first time
)

var (
	waitPollInterval = 200 * time.Millisecond
)

// LogDescriptor is a struct that holds information about logs beloning to a DCP resource
// (e.g. Container, Executable etc).
type LogDescriptor struct {
	ResourceName types.NamespacedName
	ResourceUID  types.UID

	// The context for resource logs, to be used by log watchers.
	// When the resource is deleted, the context is cancelled and all log watchers terminate.
	Context       context.Context
	CancelContext context.CancelFunc

	lock          *sync.Mutex // Protects the fields below
	stdOut        *os.File
	stdErr        *os.File
	consumerCount uint32 // Number of active log watchers. Includes the log capturing goroutine.
	disposed      bool
}

// Creates new LogDescriptor.
// Note: this is separated from log file creation so make sure that NewLogDescriptor() never fails,
// and as a result, the LogDescriptor is easy to use as a value type in a syncmap.
func NewLogDescriptor(ctx context.Context, cancel context.CancelFunc, resourceName types.NamespacedName, resourceUID types.UID) *LogDescriptor {
	return &LogDescriptor{
		ResourceName:  resourceName,
		ResourceUID:   resourceUID,
		Context:       ctx,
		CancelContext: cancel,
		lock:          &sync.Mutex{},
	}
}

// Creates destination files for capturing resource logs.
// The LogDescriptor assumes there will be only one writer for both files,
// so only one call to this method is allowed.
func (l *LogDescriptor) EnableLogCapturing(logsFolder string) (io.Writer, io.Writer, error) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.disposed || l.Context.Err() != nil {
		return nil, nil, fmt.Errorf("this LogDescriptor (for %s) has been disposed", l.ResourceName.String())
	}

	if l.stdOut != nil || l.stdErr != nil {
		return nil, nil, fmt.Errorf("log files already exist for resource %s", l.ResourceName.String())
	}

	// To avoid file name conflicts when log watching is stopped and quickly started again for the same resource,
	// we include a random suffix in the file names.
	suffix, randErr := randdata.MakeRandomString(4)
	if randErr != nil {
		// Should never happen
		return nil, nil, fmt.Errorf("could not generate random suffix for log file names for resource %s: %w", l.ResourceName.String(), randErr)
	}

	stdOutFileName := fmt.Sprintf("%s_out_%s_%s", l.ResourceName.Name, l.ResourceUID, string(suffix))
	stdOutPath := filepath.Join(logsFolder, stdOutFileName)
	stdOut, stdOutErr := usvc_io.OpenFile(stdOutPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdOutErr != nil {
		// If we cannot create stdout or stderr file, this descriptor is pretty much unusable.
		// We consider is disposed.
		l.disposed = true
		return nil, nil, fmt.Errorf("could not create stdout log file for resource %s: %w", l.ResourceName.String(), stdOutErr)
	}

	stdErrFileName := fmt.Sprintf("%s_err_%s_%s", l.ResourceName.Name, l.ResourceUID, string(suffix))
	stdErrPath := filepath.Join(logsFolder, stdErrFileName)
	stdErr, stdErrErr := usvc_io.OpenFile(stdErrPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdErrErr != nil {
		l.disposed = true
		stdOutCloseErr := stdOut.Close()
		stdOutRemoveErr := os.Remove(stdOutPath)
		return nil, nil, errors.Join(
			fmt.Errorf("could not create stderr log file for resource %s: %w", l.ResourceName.String(), stdErrErr),
			stdOutCloseErr,
			stdOutRemoveErr,
		)
	}

	l.stdOut = stdOut
	l.stdErr = stdErr
	return stdOut, stdErr, nil
}

// Notifies the log descriptor that another log watcher (or the log streamer) has started to use the log files.
// Returns the paths to the log files and an error if the log descriptor was disposed or the context was cancelled.
func (l *LogDescriptor) LogConsumerStarting() (string, string, error) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if !l.disposed {
		l.consumerCount++
		return l.stdOut.Name(), l.stdErr.Name(), nil
	} else {
		return "", "", fmt.Errorf("this LogDescriptor (for %s) has been disposed", l.ResourceName.String())
	}
}

// Notifies the log descriptor that a log watcher has stopped using the log files.
// Should only be called if corresponding LogConsumerStarting() returned true.
func (l *LogDescriptor) LogConsumerStopped() {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.consumerCount > 0 {
		l.consumerCount--
	}
}

// Waits for all log watchers to stop (with a passed deadline) and then deletes the log files (best effort).
func (l *LogDescriptor) Dispose(deadline context.Context) error {
	// Zero out the paths to log files immediately so that no new log watchers can be started.
	l.lock.Lock()
	if l.disposed {
		l.lock.Unlock()
		return nil
	}
	l.disposed = true
	l.CancelContext()
	l.lock.Unlock()

	_ = wait.PollUntilContextCancel(deadline, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		l.lock.Lock()
		defer l.lock.Unlock()

		if l.consumerCount > 0 {
			return false, nil
		} else {
			return true, nil
		}
	})

	stdOutPath := l.stdOut.Name()
	stdErrPath := l.stdErr.Name()
	stdOutCloseErr := l.stdOut.Close()
	stdErrCloseErr := l.stdErr.Close()
	stdOutRemoveErr := os.Remove(stdOutPath)
	stdErrRemoveErr := os.Remove(stdErrPath)
	return errors.Join(stdOutCloseErr, stdOutRemoveErr, stdErrCloseErr, stdErrRemoveErr)
}
