package daemon

import "os"

// Acquire records the running daemon's lifecycle files: its own pid and the
// chosen discovery address. The foreground daemon calls this immediately after
// Listen succeeds, so a status/stop command can find and signal it. addr is the
// scheme-qualified listen address (e.g. "unix:///…/daemon.sock").
func Acquire(p Paths, addr string) error {
	if err := WritePidfile(p.Pid, os.Getpid()); err != nil {
		return err
	}
	if err := writeAddr(p, addr); err != nil {
		// Roll back the pidfile so we never leave a pidfile without an addr.
		_ = RemovePidfile(p.Pid)
		return err
	}
	return nil
}

// Release removes all of this daemon's lifecycle files (pidfile, socket, addr).
// The foreground daemon calls it on graceful shutdown so the next start finds a
// clean directory. It is idempotent: missing files are not errors.
func Release(p Paths) error {
	return cleanStale(p)
}

// CleanStale removes the lifecycle files of a crashed instance. The cmd layer
// calls it during pre-start when Query reports Stale residue but no live daemon.
func CleanStale(p Paths) error {
	return cleanStale(p)
}
