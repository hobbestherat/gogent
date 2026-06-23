package daemon

import (
	"fmt"
	"syscall"
	"time"
)

// stopPollInterval is how often Stop re-checks whether the signalled process has
// exited while waiting out the grace period.
const stopPollInterval = 50 * time.Millisecond

// Stop terminates a running daemon and reclaims its lifecycle files.
//
//   - If no live daemon is found, any stale files are cleaned and ErrNotRunning
//     is returned (the caller can treat that as "already stopped").
//   - Otherwise SIGTERM is sent and Stop waits up to timeout for a graceful
//     exit, during which the daemon persists state and removes its own files.
//   - If the daemon has not exited within timeout: when force is set, SIGKILL is
//     sent and the files are reclaimed; otherwise an error is returned and the
//     daemon is left running so the caller can decide.
//
// A successful graceful or forced stop returns nil with the files removed.
func Stop(p Paths, timeout time.Duration, force bool) error {
	st := Query(p)
	if !st.Running {
		// Nothing live: clear any crash residue so the next start is clean.
		_ = cleanStale(p)
		return ErrNotRunning
	}

	pid := st.PID
	if pid <= 0 {
		return fmt.Errorf("daemon socket is live but pidfile is missing or invalid; cannot signal")
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon pid %d: %w", pid, err)
	}

	if waitExit(pid, timeout) {
		// The daemon should have removed its own files on graceful shutdown; sweep
		// any that remain so the directory is left clean regardless.
		_ = cleanStale(p)
		return nil
	}

	if !force {
		return fmt.Errorf("daemon pid %d did not exit within %s (use --force to SIGKILL)", pid, timeout)
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("force-kill daemon pid %d: %w", pid, err)
	}
	// Give the kernel a moment to reap, then reclaim the files the killed process
	// never got to remove.
	waitExit(pid, time.Second)
	_ = cleanStale(p)
	return nil
}

// waitExit polls until the process is gone or the deadline elapses, returning
// true if it exited in time.
func waitExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !ProcessAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(stopPollInterval)
	}
}
