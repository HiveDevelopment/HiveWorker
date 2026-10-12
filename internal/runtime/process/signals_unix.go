//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func signalProcess(cmd *exec.Cmd, name string, force bool) error {
	signal := syscall.SIGTERM
	switch name {
	case "SIGINT":
		signal = syscall.SIGINT
	case "SIGHUP":
		signal = syscall.SIGHUP
	case "SIGQUIT":
		signal = syscall.SIGQUIT
	}
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
