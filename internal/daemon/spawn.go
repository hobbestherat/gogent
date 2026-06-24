package daemon

import (
	"fmt"
	"os"
	"os/exec"
)

// Spawn re-executes the current binary as a detached background process and
// returns the child's pid. The child is detached from the launching terminal so
// it survives that terminal disconnecting (the whole point of the daemon): on
// Unix via a new session (setsid); on Windows via the DETACHED_PROCESS /
// CREATE_NEW_PROCESS_GROUP creation flags (a background process that outlives the
// console — not a true Windows service; see detachSysProcAttr). The detach
// attributes are supplied by the per-platform detachSysProcAttr. stdin is wired
// to the null device and stdout/stderr are redirected to the daemon log, so the
// detached process never writes to the (now absent) terminal.
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
	// Detach from the launching terminal/console so the daemon outlives it. The
	// concrete attributes are OS-specific (setsid on Unix, creation flags on
	// Windows); see detachSysProcAttr.
	cmd.SysProcAttr = detachSysProcAttr()

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
