//go:build windows

package process

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Use separate process group so this process exit will not affect the children.
func DecoupleFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Use separate console group to force the child process completely outside its parent's process group
func ForkFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
}

func GetBuiltInSid(domainAliasRid uint32) (*windows.SID, error) {
	var sid *windows.SID
	if err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		domainAliasRid,
		0, 0, 0, 0, 0, 0,
		&sid,
	); err != nil {
		return nil, err
	}

	return sid, nil
}

func FindProcess(pid Pid_t) (*os.Process, error) {
	osPid, err := PidT_ToInt(pid)
	if err != nil {
		return nil, err
	}

	process, err := os.FindProcess(osPid)
	if err != nil {
		return nil, err
	}

	return process, nil
}
