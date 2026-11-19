//go:build unix

package process

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcAttr starts the engine in a process group of its own: a Ctrl-C in
// the gateway's terminal does not reach it (the gateway stops its engines
// itself, in order), and kill can stop helpers that a wrapper script started.
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// kill stops the engine's process group immediately.
func kill(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return cmd.Process.Kill()
	}
	return nil
}
