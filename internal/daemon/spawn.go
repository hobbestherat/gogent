package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Spawn re-executes the current binary as a detached background process and
// returns the child's pid. The child is given its own session via setsid so it
// has no controlling terminal and survives the parent terminal disconnecting
// (the whole point of the daemon). stdin is wired to /dev/null and stdout/stderr
// are redirected to the daemon log, so the detached process never writes to the
// (now absent) terminal.
//
// args is the full argument vector passed after the program name — the cmd layer
// supplies e.g. ["daemon", "start", "--foreground", ...] so the child runs the
// daemon in-process. The parent does not Wait: it returns once the child has
// started, leaving it running independently.
func Spawn(p Paths, args []string) (int, error) {
	if err := p.ensureDir(); err != nil {
		return 0, err
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve executable: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open daemon log %s: %w", p.Log, err)
	}
	defer func() { _ = logf.Close() }()

	cmd := exec.Command(exe, args...) //nolint:gosec // re-executes this same binary; args are daemon-internal, not user-tainted
	cmd.Stdin = devNull
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Detach: a new session with no controlling terminal so the daemon outlives
	// the terminal that launched it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// Release the child so it is not left as a zombie when it exits; we manage its
	// lifetime through the pidfile and signals, not the parent/child relationship.
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("release daemon process: %w", err)
	}
	return pid, nil
}
