//go:build !windows

package shell

import (
	"context"
	"os/exec"
	"syscall"
)

// newShellCommand builds the command used to run a shell snippet. On Unix it
// runs `sh -c` in its own process group so that, on timeout/cancel, we can kill
// the whole group (the shell and all of its children) rather than orphaning the
// grandchildren the shell spawned.
func newShellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative PID targets the process group led by the child.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}

