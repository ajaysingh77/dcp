package exerunners

import (
	"context"
	"io"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type runState struct {
	name              types.NamespacedName
	runID             controllers.RunID
	sessionURL        string // The URL of the run session resource in the IDE
	changeHandler     controllers.RunChangeHandler
	handlerWG         *sync.WaitGroup
	pid               process.Pid_t
	finished          bool
	exitCode          *int32
	exitCh            chan struct{}
	stdOut            *usvc_io.BufferedWrappingWriter
	stdErr            *usvc_io.BufferedWrappingWriter
	changeNotifyQueue *resiliency.WorkQueue
}

func NewRunState(lifetimeCtx context.Context) *runState {
	rs := &runState{
		runID:             "",
		changeHandler:     nil,
		handlerWG:         &sync.WaitGroup{},
		finished:          false,
		exitCode:          apiv1.UnknownExitCode,
		exitCh:            make(chan struct{}),
		stdOut:            usvc_io.NewBufferedWrappingWriter(),
		stdErr:            usvc_io.NewBufferedWrappingWriter(),
		changeNotifyQueue: resiliency.NewWorkQueue(lifetimeCtx, 1), // For a particular run, change notifications are serialized
	}

	// Two things need to happen before the completion handler is called:
	// 1. The StartRun() method of the IDE runner must get a chance to set the completion handler.
	// 2. The IDE runner consumer must call startWaitForRunCompletion() for the run.
	rs.handlerWG.Add(2)

	return rs
}

func (rs *runState) notifyRunChanged(locker sync.Locker, notifyType notificationType) {
	rs.handlerWG.Wait()

	// Make sure the run state is not changed while we are gathering data for the change handler by locking the run state.
	// The notification always uses latest data stored in the run state.

	locker.Lock()
	runID := rs.runID
	pid := rs.pid
	exitCode := apiv1.UnknownExitCode
	if rs.exitCode != apiv1.UnknownExitCode {
		exitCode = new(int32)
		*exitCode = *rs.exitCode
	}
	locker.Unlock()

	switch notifyType {
	case notificationTypeProcessRestarted:
		rs.changeHandler.OnMainProcessChanged(runID, pid)
	case notificationTypeSessionTerminated:
		rs.changeHandler.OnRunCompleted(runID, exitCode, nil)
	}
}

func (rs *runState) NotifyRunChangedAsync(locker sync.Locker) {
	if !rs.finished {
		// Errors only if lifetime context is cancelled
		_ = rs.changeNotifyQueue.Enqueue(func(_ context.Context) {
			rs.notifyRunChanged(locker, notificationTypeProcessRestarted)
		})
	}
}

func (rs *runState) NotifyRunCompletedAsync(locker sync.Locker) {
	// Errors only if lifetime context is cancelled
	_ = rs.changeNotifyQueue.Enqueue(func(_ context.Context) {
		rs.notifyRunChanged(locker, notificationTypeSessionTerminated)
	})
}

func (rs *runState) IncreaseCompletionCallReadiness() {
	rs.handlerWG.Done()
}

func (rs *runState) SetOutputWriters(stdOut, stdErr io.WriteCloser) error {
	var err error
	if stdOut != nil {
		err = rs.stdOut.SetTarget(usvc_io.NewTimestampWriter(stdOut))
	} else {
		err = rs.stdOut.SetTarget(io.Discard)
	}
	if err != nil {
		return err
	}

	if stdErr != nil {
		err = rs.stdErr.SetTarget(usvc_io.NewTimestampWriter(stdErr))
	} else {
		err = rs.stdErr.SetTarget(io.Discard)
	}
	if err != nil {
		return err
	}

	return nil
}

func (rs *runState) CloseOutputWriters() {
	rs.stdOut.Close()
	rs.stdErr.Close()
}
