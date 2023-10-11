package testutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/stretchr/testify/require"
)

type ProcessExecution struct {
	PID                process.Pid_t
	Cmd                *exec.Cmd
	StartWaitingCalled bool
	StartedAt          time.Time
	EndedAt            time.Time
	ExitHandler        process.ProcessExitHandler
	ExitCode           int32
}

func (pe *ProcessExecution) Running() bool {
	return pe.EndedAt.IsZero()
}
func (pe *ProcessExecution) Finished() bool {
	return !pe.EndedAt.IsZero()
}

type TestProcessExecutor struct {
	nextPID    int64
	Executions []ProcessExecution
	m          *sync.RWMutex
}

const (
	NotFound              = -1
	KilledProcessExitCode = 137 // 128 + SIGKILL (9)
)

func NewTestProcessExecutor() *TestProcessExecutor {
	return &TestProcessExecutor{
		Executions: make([]ProcessExecution, 0),
		m:          &sync.RWMutex{},
	}
}

func (e *TestProcessExecutor) StartProcess(ctx context.Context, cmd *exec.Cmd, handler process.ProcessExitHandler) (process.Pid_t, func(), error) {
	pid64 := atomic.AddInt64(&e.nextPID, 1)
	pid, err := process.Int64ToPidT(pid64)
	if err != nil {
		return process.UnknownPID, nil, err
	}

	e.m.Lock()
	defer e.m.Unlock()

	pe := ProcessExecution{
		Cmd:         cmd,
		PID:         pid,
		StartedAt:   time.Now(),
		ExitHandler: handler,
	}

	// For testing purposes make sure that stdout and stderr are always captured.
	if cmd.Stdout == nil {
		cmd.Stdout = new(bytes.Buffer)
	}
	if cmd.Stderr == nil {
		cmd.Stderr = new(bytes.Buffer)
	}

	e.Executions = append(e.Executions, pe)

	startWaitingForExit := func() {
		e.m.Lock()
		defer e.m.Unlock()
		i := e.findByPid(pid)
		pe := e.Executions[i]
		pe.StartWaitingCalled = true
		e.Executions[i] = pe
	}

	return pid, startWaitingForExit, nil
}

// Called by the controller (via Executor interface)
func (e *TestProcessExecutor) StopProcess(pid process.Pid_t) error {
	return e.stopProcessImpl(pid, KilledProcessExitCode)
}

// Called by tests to simulate a process exit with specific exit code.
func (e *TestProcessExecutor) SimulateProcessExit(t *testing.T, pid process.Pid_t, exitCode int32) {
	err := e.stopProcessImpl(pid, exitCode)
	if err != nil {
		require.Failf(t, "invalid PID (test issue)", err.Error())
	}
}

// Finds all executions of a specific command.
// The command is identified by path to the executable and a subset of its arguments (the first N arguments).
// If lastArg parameter is not empty, it is matched against the last argument of the command.
// Last parameter is a function that is called to verify that the command is the one we are waiting for.
func (e *TestProcessExecutor) FindAll(
	command []string,
	lastArg string,
	cond func(pe *ProcessExecution) bool,
) []ProcessExecution {
	e.m.RLock()
	defer e.m.RUnlock()

	retval := make([]ProcessExecution, 0)
	if len(command) == 0 {
		return retval
	}

	cmdPath := command[0]
	usingAbsPath := path.IsAbs(cmdPath)

	for _, pe := range e.Executions {
		if usingAbsPath {
			if pe.Cmd.Path != cmdPath {
				continue // Path to executable does not match
			}
		} else {
			if !strings.Contains(pe.Cmd.Path, cmdPath) {
				continue // Path to executable does not contain the expected command name
			}
		}

		args := pe.Cmd.Args

		if len(args) < len(command) {
			continue // Not enough arguments
		}

		if len(command) > 1 && !slices.StartsWith(args[1:], command[1:]) {
			continue // First N arguments don't match
		}

		if lastArg != "" && args[len(args)-1] != lastArg {
			continue // Last argument doesn't match
		}

		if cond != nil && !cond(&pe) {
			continue // Condition doesn't match
		}

		// All criteria match
		retval = append(retval, pe)
	}

	return retval
}

func (e *TestProcessExecutor) FindByPid(pid process.Pid_t) (ProcessExecution, bool) {
	e.m.RLock()
	defer e.m.RUnlock()

	i := e.findByPid(pid)
	if i == NotFound {
		return ProcessExecution{}, false
	}
	return e.Executions[i], true
}

// Clears all execution history
func (e *TestProcessExecutor) ClearHistory() {
	e.m.Lock()
	defer e.m.Unlock()

	e.Executions = make([]ProcessExecution, 0)
	// The PID counter is not reset so that the clients continue to receive unique PIDs.
}

func (e *TestProcessExecutor) findByPid(pid process.Pid_t) int {
	for i, pe := range e.Executions {
		if pe.PID == pid {
			return i
		}
	}

	return NotFound
}

func (e *TestProcessExecutor) stopProcessImpl(pid process.Pid_t, exitCode int32) error {
	e.m.Lock()
	defer e.m.Unlock()

	i := e.findByPid(pid)
	if i == NotFound {
		return fmt.Errorf("no process with PID %d found", pid)
	}
	pe := e.Executions[i]
	pe.ExitCode = exitCode
	pe.EndedAt = time.Now()
	e.Executions[i] = pe
	if pe.ExitHandler != nil {
		go pe.ExitHandler.OnProcessExited(pid, exitCode, nil)
	}
	return nil
}

var _ process.Executor = (*TestProcessExecutor)(nil)
