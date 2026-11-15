//go:build linux

package airplay

import (
	"os/exec"
	"syscall"
)

// setChildProcAttr makes the kernel stop the capture child when the thread
// that started it exits.
func setChildProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func afterChildStart(*exec.Cmd) {}
