//go:build windows

package engine

import (
	"os"
	"os/exec"
)

func configureProcess(cmd *exec.Cmd) {
}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}

func forceKillProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}
