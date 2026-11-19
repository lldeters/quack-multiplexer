//go:build !unix && !windows

package process

import "os/exec"

// setProcAttr has nothing to set on this platform.
func setProcAttr(*exec.Cmd) {}

// kill stops the engine process immediately.
func kill(cmd *exec.Cmd) error { return cmd.Process.Kill() }
