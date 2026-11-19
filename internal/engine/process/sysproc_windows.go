//go:build windows

package process

import (
	"os/exec"
	"syscall"
)

// setProcAttr starts the engine in a process group of its own, so that a
// Ctrl-C in the gateway's console does not reach it (the gateway stops its
// engines itself, in order).
func setProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// kill stops the engine process immediately (only the process itself:
// Windows has no process groups to signal).
func kill(cmd *exec.Cmd) error { return cmd.Process.Kill() }
