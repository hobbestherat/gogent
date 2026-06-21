//go:build windows

package shell

import (
	"context"
	"os/exec"
)

// newShellCommand builds the command used to run a shell snippet. On Windows
// the snippet is executed through `cmd /C`. Windows has no POSIX process
// groups, so cancellation relies on the default exec.Cmd cancel behaviour,
// which kills the cmd.exe process when the context is cancelled or times out.
func newShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd", "/C", command)
}

