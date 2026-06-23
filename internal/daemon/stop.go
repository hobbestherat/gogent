package daemon

import (
	"fmt"
	"syscall"
	"time"
)

// stopPollInterval is how often Stop re-checks whether the signalled process has
// exited (or the socket has gone dead) while waiting out the grace period.
const stopPollInterval = 50 * time.Millisecond

// Stop terminates a running daemon and reclaims its lifecycle files.
//
// When a daemon is answering on the socket, Stop prefers the daemon's own /exit
// endpoint over signalling a pid: /exit targets the real daemon process via the
// socket it owns, so a missing, stale or corrupt pidfile can neither make us
// signal an unrelated process nor unlink a live socket. The lifecycle files are
// reclaimed only after the socket has gone dead (graceful) or the process has
// exited (signalled). Behaviour:
//
//   - Nothing live: any stale residue is cleaned and ErrNotRunning is returned.
//   - Socket live: ask it to /exit; on success and a clean socket teardown the
//     files are reclaimed. If /exit is unavailable, fall back to signalling the
//     recorded pid — but if that pid is unusable, refuse rather than orphan a
//     live socket.
//   - SIGTERM is sent and Stop waits up to timeout; if the process has not
//     exited, force sends SIGKILL (otherwise an error is returned, daemon left
//     running).
func Stop(p Paths, timeout time.Duration, force bool) error {
	st := Query(p)
	socketLive := Probe(p.Sock)

	// Nothing is live anywhere: reclaim any crash residue.
	if !st.Running && !socketLive {
		_ = cleanStale(p)
		return ErrNotRunning
	}

	// A daemon is answering on the socket. Prefer its own /exit endpoint so a bad
	// pidfile cannot cause us to signal the wrong process or remove a live socket.
	if socketLive {
		if exitViaSocket(p.Sock) && waitSocketDead(p.Sock, timeout) {
			_ = cleanStale(p)
			return nil
		}
		// /exit was unavailable or did not stop it in time. We may only fall back
		// to signalling if we have a live pid to signal; otherwise refuse rather
		// than risk unlinking a socket a daemon is still serving.
		if st.PID <= 0 || !ProcessAlive(st.PID) {
			return fmt.Errorf("daemon is live on %s but its pidfile is missing or invalid and /exit did not stop it", p.Sock)
		}
	}

	pid := st.PID
	if pid <= 0 || !ProcessAlive(pid) {
		// Socket is not live (we would have returned above otherwise) and there is
		// no signallable process: just reclaim the residue.
		_ = cleanStale(p)
		return ErrNotRunning
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon pid %d: %w", pid, err)
	}
	if waitExit(pid, timeout) {
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

// waitSocketDead polls until the daemon stops answering on the socket or the
// deadline elapses, returning true if it went dead in time. Used to confirm a
// graceful /exit actually tore the listener down before reclaiming files.
func waitSocketDead(sockPath string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !Probe(sockPath) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(stopPollInterval)
	}
}
