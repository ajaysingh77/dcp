package process

import (
	"context"
	"os/exec"

	ps "github.com/mitchellh/go-ps"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Returns the list of ID for a given process and its children
// The list is ordered starting with the root of the hierarchy, then the children, then the grandchildren etc.
func GetProcessTree(pid int32) ([]int32, error) {
	procs, err := ps.Processes()
	if err != nil {
		return nil, err
	}

	tree := []int32{}
	next := []int{int(pid)}

	for len(next) > 0 {
		current := next[0]
		next = next[1:]
		tree = append(tree, int32(current))
		children := slices.Select(procs, func(p ps.Process) bool {
			return p.PPid() == current
		})
		next = append(next, slices.Map[ps.Process, int](children, func(p ps.Process) int {
			return p.Pid()
		})...)
	}

	return tree, nil
}

// Runs the command as a child process to completion.
// Returns exit code, or error if the process could not be started/tracked for some reason.
//
// The context parameter is used to request cancellation of the process, but the call to RunToCompletion() will not return
// until the process exits and all its output is captured.
// Do not assume the call will end quickly if the context is cancelled.
func RunToCompletion(ctx context.Context, executor Executor, cmd *exec.Cmd) (int32, error) {
	pic := make(chan ProcessExitInfo, 1)
	peh := NewChannelProcessExitHandler(pic)

	_, startWaitForProcessExit, err := executor.StartProcess(ctx, cmd, peh)
	if err != nil {
		return UnknownExitCode, err
	}

	startWaitForProcessExit()

	// Only exit when the process exit--do not exit merely because the context is cancelled.
	exitInfo := <-pic
	return exitInfo.ExitCode, exitInfo.Err
}

type resultOrError[T any] struct {
	result T
	err    error
}

// Runs the command as a child process to completion, unless the passed context is cancelled,
// or its deadline is exceeded.
func RunWithTimeout(ctx context.Context, executor Executor, cmd *exec.Cmd) (int32, error) {
	resultCh := make(chan resultOrError[int32], 1)
	go func() {
		exitCode, err := RunToCompletion(ctx, executor, cmd)
		resultCh <- resultOrError[int32]{exitCode, err}
	}()

	select {
	case <-ctx.Done():
		return UnknownExitCode, ctx.Err()
	case runResult := <-resultCh:
		return runResult.result, runResult.err
	}
}
