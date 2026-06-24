package daemon

import (
	"fmt"
	"time"
)

// stopPollInterval is how often Stop re-checks whether the signalled process has
// exited (or the transport has gone dead) while waiting out the grace period.
const stopPollInterval = 50 * time.Millisecond

// Stop terminates a running daemon and reclaims its lifecycle files. The control
// flow is OS-independent; only the mechanisms it calls are per-platform —
// liveness (probeLive), graceful shutdown (exitLive = the daemon's own /exit),
// and process signalling (gracefulSignal/forceKill). On Unix gracefulSignal is
// SIGTERM and forceKill is SIGKILL; on Windows there is no graceful signal, so
// gracefulSignal is a no-op (the graceful path is /exit) and forceKill is
// TerminateProcess — but the policy below is identical on both.
//
// When a daemon is answering, Stop prefers its own /exit endpoint over signalling
// a pid: /exit targets the real daemon process through the transport it owns, so
// a missing, stale or corrupt pidfile can neither make us signal an unrelated
// process nor unlink a live peer. The lifecycle files are reclaimed only after
// the transport has gone dead (graceful) or the process has exited (signalled).
// Behaviour:
//
//   - Nothing live: any stale residue is cleaned and ErrNotRunning is returned.
//   - Transport live: ask it to /exit; on success and a clean teardown the files
//     are reclaimed. If /exit is unavailable, fall back to signalling the
//     recorded pid — but if that pid is unusable, refuse rather than orphan a
//     live daemon.
//   - The graceful signal is sent and Stop waits up to timeout; if the process
//     has not exited, force-kills it (with --force) or returns an error.
func Stop(p Paths, timeout time.Duration, force bool) error {
	live := probeLive(p)

	// Nothing is answering on the transport: there is no live daemon to stop, so
	// just reclaim any crash residue. Stop never signals a pid behind a dead
	// transport.
	if DecideStopMode(live) == StopModeReclaim {
		_ = cleanStale(p)
		return ErrNotRunning
	}

	st := Query(p)

	// A daemon is answering. Prefer its own /exit endpoint so a bad pidfile cannot
	// cause us to signal the wrong process or remove a live transport.
	if exitLive(p) && waitLiveDead(p, timeout) {
		_ = cleanStale(p)
		return nil
	}
	// /exit was unavailable or did not stop it in time. We may only fall back to
	// signalling if we have a live pid to signal; otherwise refuse rather than
	// risk reclaiming files a daemon is still serving.
	if st.PID <= 0 || !ProcessAlive(st.PID) {
		return fmt.Errorf("daemon is live on %s but its pidfile is missing or invalid and /exit did not stop it", primaryAddr(p))
	}

	pid := st.PID
	if err := gracefulSignal(pid); err != nil {
		return fmt.Errorf("signal daemon pid %d: %w", pid, err)
	}
	if waitExit(pid, timeout) {
		_ = cleanStale(p)
		return nil
	}

	if !force {
		return fmt.Errorf("daemon pid %d did not exit within %s (use --force to force-kill)", pid, timeout)
	}

	if err := forceKill(pid); err != nil {
		return fmt.Errorf("force-kill daemon pid %d: %w", pid, err)
	}
	// Give the OS a moment to reap, then reclaim the files the killed process
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

// waitLiveDead polls until the daemon stops answering on its transport or the
// deadline elapses, returning true if it went dead in time. Used to confirm a
// graceful /exit actually tore the listener down before reclaiming files.
func waitLiveDead(p Paths, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !probeLive(p) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(stopPollInterval)
	}
}
