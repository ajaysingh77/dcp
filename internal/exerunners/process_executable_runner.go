package exerunners

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DCP_SKIP_MONITOR_PROCESSES = "DCP_SKIP_MONITOR_PROCESSES"
)

type processRunState struct {
	stdOutFile *os.File
	stdErrFile *os.File
}

func newProcessRunState(stdOutFile *os.File, stdErrFile *os.File) *processRunState {
	return &processRunState{
		stdOutFile: stdOutFile,
		stdErrFile: stdErrFile,
	}
}

type ProcessExecutableRunner struct {
	pe               process.Executor
	runningProcesses *syncmap.Map[controllers.RunID, *processRunState]
}

func NewProcessExecutableRunner(pe process.Executor) *ProcessExecutableRunner {
	return &ProcessExecutableRunner{
		pe:               pe,
		runningProcesses: &syncmap.Map[controllers.RunID, *processRunState]{},
	}
}

func (r *ProcessExecutableRunner) StartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *controllers.ExecutableRunInfo,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) error {
	cmd := makeCommand(exe)
	log.Info("starting process...", "executable", cmd.Path)
	log.V(1).Info("process settings",
		"executable", cmd.Path,
		"args", cmd.Args[1:],
		"env", cmd.Env,
		"cwd", cmd.Dir)

	stdOutFile, err := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard output data")
	} else {
		cmd.Stdout = usvc_io.NewTimestampWriter(stdOutFile)
		runInfo.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, err := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard error data")
	} else {
		cmd.Stderr = usvc_io.NewTimestampWriter(stdErrFile)
		runInfo.StdErrFile = stdErrFile.Name()
	}

	var processExitHandler process.ProcessExitHandler = nil
	if runChangeHandler != nil {
		processExitHandler = process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
			ec := new(int32)
			*ec = exitCode
			runChangeHandler.OnRunCompleted(pidToRunID(pid), ec, err)
		})
	}

	pid, startWaitForProcessExit, err := r.pe.StartProcess(ctx, cmd, processExitHandler)

	if err != nil {
		log.Error(err, "failed to start a process")
		runInfo.FinishTimestamp = metav1.NowMicro()
		runInfo.ExeState = apiv1.ExecutableStateFailedToStart
	} else {
		r.runningProcesses.Store(pidToRunID(pid), newProcessRunState(stdOutFile, stdErrFile))
		log.Info("process started", "executable", cmd.Path, "PID", pid)
		runInfo.ExecutionID = pidToExecutionID(pid)
		if runInfo.Pid == apiv1.UnknownPID {
			runInfo.Pid = new(int64)
		}
		*runInfo.Pid = int64(pid)
		runInfo.ExeState = apiv1.ExecutableStateRunning
		runInfo.StartupTimestamp = metav1.NowMicro()

		r.runWatcher(ctx, pid, log)

		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), pidToRunID(pid), runInfo, startWaitForProcessExit)
	}

	return err
}

func (r *ProcessExecutableRunner) StopRun(_ context.Context, runID controllers.RunID, log logr.Logger) error {
	log.V(1).Info("stopping process...", "runID", runID)

	// We want to make progress eventually, so we don't want to wait indefinitely for the process to stop.
	const ProcessStopTimeout = 15 * time.Second

	timer := time.NewTimer(ProcessStopTimeout)
	defer timer.Stop()
	errCh := make(chan error, 1)

	go func() {
		errCh <- r.pe.StopProcess(runIdToPID(runID))
	}()

	var stopErr error = nil
	select {
	case stopErr = <-errCh:
		// (no falltrough in Go)
	case <-timer.C:
		stopErr = fmt.Errorf("timed out waiting for process associated with run %s to stop", runID)
	}

	if runState, found := r.runningProcesses.LoadAndDelete(runID); found {
		var stdOutErr error
		if runState.stdOutFile != nil {
			stdOutErr = runState.stdOutFile.Close()
		}

		var stdErrErr error
		if runState.stdErrFile != nil {
			stdErrErr = runState.stdErrFile.Close()
		}

		stopErr = errors.Join(stopErr, stdOutErr, stdErrErr)
	}

	return stopErr
}

func (r *ProcessExecutableRunner) runWatcher(ctx context.Context, pid process.Pid_t, log logr.Logger) {
	if _, found := os.LookupEnv(DCP_SKIP_MONITOR_PROCESSES); found {
		return
	}

	// This is a best effort and will only log errors if the process watcher can't be started
	binPath, binPathErr := dcppaths.GetDcpBinDir()
	if binPathErr != nil {
		log.Error(binPathErr, "could not resolve path to process monitor", "PID", pid)
	} else {
		procMonitorPath := filepath.Join(binPath, "dcpproc")
		if runtime.GOOS == "windows" {
			procMonitorPath += ".exe"
		}

		// DCP doesn't shut down if DCPCTRL goes away, but DCPCTRL will shut down if DCP goes away. For now, watching DCPCTRL is the safer bet.
		monitorPid := os.Getpid()

		// Monitor the parent process and the service process
		monitorCmd := exec.Command(procMonitorPath, "--monitor", strconv.Itoa(monitorPid), "--proc", strconv.FormatInt(int64(pid), 10))
		_, _, monitorErr := r.pe.StartProcess(ctx, monitorCmd, nil)
		if monitorErr != nil {
			log.Error(monitorErr, "failed to start process monitor", "executable", procMonitorPath, "PID", pid)
		}
	}
}

func makeCommand(exe *apiv1.Executable) *exec.Cmd {
	cmd := exec.Command(exe.Spec.ExecutablePath)
	cmd.Args = append([]string{exe.Spec.ExecutablePath}, exe.Status.EffectiveArgs...)

	cmd.Env = slices.Map[apiv1.EnvVar, string](exe.Status.EffectiveEnv, func(e apiv1.EnvVar) string { return fmt.Sprintf("%s=%s", e.Name, e.Value) })

	cmd.Dir = exe.Spec.WorkingDirectory

	return cmd
}

func pidToRunID(pid process.Pid_t) controllers.RunID {
	return controllers.RunID(strconv.FormatInt(int64(pid), 10))
}

func runIdToPID(runID controllers.RunID) process.Pid_t {
	pid64, err := strconv.ParseInt(string(runID), 10, 64)
	if err != nil {
		return process.UnknownPID
	}
	pid, err := process.Int64ToPidT(pid64)
	if err != nil {
		return process.UnknownPID
	}
	return pid
}

func pidToExecutionID(pid process.Pid_t) string {
	return strconv.FormatInt(int64(pid), 10)
}

var _ controllers.ExecutableRunner = (*ProcessExecutableRunner)(nil)
