//go:build windows

package process

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	CREATE_NEW_CONSOLE = 0x00000010
)

// Use separate process group so this process exit will not affect the children.
func DecoupleFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Use separate console group to force the child process completely outside its parent's process group
func ForkFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: CREATE_NEW_CONSOLE, HideWindow: true}
}

func FindProcess(pid int32) (*os.Process, error) {
	process, err := os.FindProcess(int(pid))
	if err != nil {
		return nil, err
	}

	return process, nil
}
