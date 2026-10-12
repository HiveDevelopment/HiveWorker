//go:build windows

package process

import "os/exec"

func configureProcessGroup(cmd *exec.Cmd) {}
func signalProcess(cmd *exec.Cmd, name string, force bool) error {
	// Windows does not support Unix process signals. Console shutdown commands
	// are attempted first; the OS termination API is the final fallback.
	return cmd.Process.Kill()
}
