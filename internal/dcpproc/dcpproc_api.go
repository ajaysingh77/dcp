package dcpproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

const (
	DCP_DISABLE_MONITOR_PROCESS = "DCP_DISABLE_MONITOR_PROCESS"
)

// Starts the process monitor for the given child process, using current process as the "monitored", or watched, process.
// The caller should ensure that the current process is the correct process to monitor.
// Errors are logged, but no error is returned if the process monitor fails to start--
// process monitor is considered a "best-effort reliability enhancement".
// Note: DCP process doesn't shut down if DCPCTRL goes away, but DCPCTRL will shut down if DCP process goes away,
// so monitoring DCPCTRL is a safe bet.
func RunProcessWatcher(
	pe process.Executor,
	childPid process.Pid_t,
	childStartTime time.Time,
	log logr.Logger,
) {
	if _, found := os.LookupEnv(DCP_DISABLE_MONITOR_PROCESS); found {
		return
	}

	log = log.WithValues("ChildPID", childPid)

	dcpProcPath, dcpprocPathErr := geDcpProcPath()
	if dcpprocPathErr != nil {
		log.Error(dcpprocPathErr, "Could not resolve path to dcpproc executable")
		return
	}

	cmdArgs := []string{
		"process",
		"--child", strconv.FormatInt(int64(childPid), 10),
	}
	if !childStartTime.IsZero() {
		cmdArgs = append(cmdArgs, "--child-start-time", childStartTime.Format(osutil.RFC3339MiliTimestampFormat))
	}
	cmdArgs = append(cmdArgs, getMonitorCmdArgs()...)

	startErr := startDcpProc(pe, dcpProcPath, cmdArgs)
	if startErr != nil {
		log.Error(startErr, "Failed to start process monitor", "DcpProcPath", dcpProcPath)
	}
}

// Starts the container monitor for the given container ID, using current process as the "monitored", or watched, process.
// The caller should ensure that the current process is the correct process to monitor.
// Errors are logged, but no error is returned if the container monitor fails to start--
// container monitor is considered a "best-effort reliability enhancement".
func RunContainerWatcher(
	pe process.Executor,
	containerID string,
	log logr.Logger,
) {
	if _, found := os.LookupEnv(DCP_DISABLE_MONITOR_PROCESS); found {
		return
	}

	log = log.WithValues("ContainerID", containerID)

	dcpProcPath, dcpprocPathErr := geDcpProcPath()
	if dcpprocPathErr != nil {
		log.Error(dcpprocPathErr, "Could not resolve path to dcpproc executable")
		return
	}

	cmdArgs := []string{
		"container",
		"--containerID", containerID,
	}
	cmdArgs = append(cmdArgs, getMonitorCmdArgs()...)

	startErr := startDcpProc(pe, dcpProcPath, cmdArgs)
	if startErr != nil {
		log.Error(startErr, "Failed to start process monitor", "DcpProcPath", dcpProcPath)
	}
}

func geDcpProcPath() (string, error) {
	binPath, binPathErr := dcppaths.GetDcpBinDir()
	if binPathErr != nil {
		return "", binPathErr
	}

	dcpProcPath := filepath.Join(binPath, "dcpproc")
	if runtime.GOOS == "windows" {
		dcpProcPath += ".exe"
	}

	return dcpProcPath, nil
}

func getMonitorCmdArgs() []string {
	monitorPid := os.Getpid()

	// Add monitor PID to the command args
	cmdArgs := []string{"--monitor", strconv.Itoa(monitorPid)}

	// Add monitor start time if available
	pid := process.Uint32_ToPidT(uint32(monitorPid))
	monitorTime := process.StartTimeForProcess(pid)
	if !monitorTime.IsZero() {
		cmdArgs = append(cmdArgs, "--monitor-start-time", monitorTime.Format(osutil.RFC3339MiliTimestampFormat))
	}

	return cmdArgs
}

func startDcpProc(pe process.Executor, dcpProcPath string, cmdArgs []string) error {
	dcpProcCmd := exec.Command(dcpProcPath, cmdArgs...)
	dcpProcCmd.Env = os.Environ()    // Use DCP CLI environment
	logger.WithSessionId(dcpProcCmd) // Ensure the session ID is passed to the monitor command
	_, _, monitorErr := pe.StartAndForget(dcpProcCmd, process.CreationFlagsNone)
	return monitorErr
}
