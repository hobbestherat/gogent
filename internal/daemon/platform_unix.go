//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// This file supplies the Unix implementations of the per-platform hooks the
// shared daemon orchestration depends on. The Unix transport is a Unix-domain
// socket with flock single-instance and POSIX-signal shutdown — the original,
// unchanged daemon behaviour. The Windows equivalents live in
// platform_windows.go.

// ProcessAlive reports whether a process with the given pid currently exists. On
// Unix it sends signal 0, which performs the permission/existence check without
// delivering a signal: nil or EPERM (exists, not ours) ⇒ alive; ESRCH ⇒ gone. A
// non-positive pid is never alive.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// detachSysProcAttr returns the attributes that detach the spawned daemon from
// the launching terminal: a new session (setsid) with no controlling terminal,
// so the daemon outlives the terminal that started it.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// gracefulSignal asks the daemon to terminate cleanly. On Unix this is SIGTERM,
// which the daemon's signal handler turns into the graceful-shutdown sequence.
func gracefulSignal(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM: %w", err)
	}
	return nil
}

// forceKill terminates the daemon immediately (SIGKILL) when a graceful stop did
// not complete within the grace period and --force was given.
func forceKill(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("SIGKILL: %w", err)
	}
	return nil
}

// probeLive reports whether a live daemon answers a health probe on the Unix
// transport — the socket at p.Sock.
func probeLive(p Paths) bool {
	return Probe(p.Sock)
}

// exitLive asks a live daemon to shut down via its own /exit endpoint over the
// Unix socket.
func exitLive(p Paths) bool {
	return exitViaSocket(p.Sock)
}

// transportResidue reports whether on-disk transport residue exists — the Unix
// socket file — used by Query to detect a crashed/half-torn-down instance.
func transportResidue(p Paths) bool {
	return socketFilePresent(p.Sock)
}

// primaryAddr is the human-facing identifier of the primary local transport,
// used in Stop diagnostics. On Unix it is the socket path.
func primaryAddr(p Paths) string {
	return p.Sock
}

// ListenLocal binds the daemon's primary local transport and returns the
// listener together with the scheme-qualified discovery address to record in
// daemon.addr. On Unix this is the flock-guarded Unix socket; the address is
// "unix://<sock>". Closing the listener releases the single-instance lock.
func ListenLocal(p Paths) (net.Listener, string, error) {
	ln, err := Listen(p.Sock)
	if err != nil {
		return nil, "", err
	}
	return ln, "unix://" + p.Sock, nil
}

// ProbeLocal reports whether a live daemon answers on the primary local
// transport, for the default-invocation auto-attach decision in cmd.
func ProbeLocal(p Paths) bool {
	return Probe(p.Sock)
}

// LocalDiscoveryAddr returns the address a local TUI would attach to: on Unix the
// well-known socket path, "unix://<sock>" (deterministic, independent of whether
// a daemon is currently running).
func LocalDiscoveryAddr(p Paths) string {
	return "unix://" + p.Sock
}
