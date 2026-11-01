//go:build !windows

package engine

import (
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return syscall.Kill(-process.Pid, syscall.SIGTERM)
}

func forceKillProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return syscall.Kill(-process.Pid, syscall.SIGKILL)
}
