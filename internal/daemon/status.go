package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Status is a point-in-time view of a daemon instance, assembled by Query from
// the pidfile and a socket liveness probe.
type Status struct {
	// Running is true only when the pid is alive AND the socket answers a health
	// probe — i.e. a daemon is actually serving, not merely a leftover pidfile.
	Running bool
	// PID is the pid recorded in the pidfile, or 0 when no (readable) pidfile
	// exists.
	PID int
	// Addr is the discovery address from daemon.addr (e.g. "unix:///…/daemon.sock"),
	// or the socket path when daemon.addr is absent.
	Addr string
	// Stale is true when a pidfile (or socket file) is present but no live daemon
	// backs it — the residue of a crash, safe to reclaim on the next start.
	Stale bool
}

// Query inspects the lifecycle files and reports whether a daemon is live. It
// never mutates state, so it is safe to call from status, start (pre-flight) and
// stop. Liveness is the conjunction of pid-alive and transport-answers; either
// one alone with the other dead marks the instance Stale. The OS-specific
// liveness probe (Unix socket / Windows TCP) and residue check are supplied by
// probeLive/transportResidue; the running/stale decision itself is the
// OS-independent ClassifyStatus.
func Query(p Paths) Status {
	st := Status{Addr: readAddr(p)}

	pid, err := ReadPidfile(p.Pid)
	pidPresent := err == nil
	if pidPresent {
		st.PID = pid
	}
	pidAlive := pidPresent && ProcessAlive(pid)

	st.Running, st.Stale = ClassifyStatus(pidPresent, pidAlive, transportResidue(p), probeLive(p))
	return st
}

// cleanStale removes the lifecycle files of a dead instance so the next start
// gets a clean slate. It is only called once Query has determined no live daemon
// holds them.
func cleanStale(p Paths) error {
	return errors.Join(
		RemovePidfile(p.Pid),
		removeSocket(p.Sock),
		removeAddr(p),
	)
}

// writeAddr records the chosen listen address for local discovery. addr is a
// scheme-qualified string such as "unix:///home/u/.gogent/daemon.sock".
func writeAddr(p Paths, addr string) error {
	if err := os.WriteFile(p.Addr, []byte(addr+"\n"), 0o600); err != nil {
		return fmt.Errorf("write addr file %s: %w", p.Addr, err)
	}
	return nil
}

// readAddr returns the recorded discovery address, falling back to the socket
// path when daemon.addr is absent or unreadable.
func readAddr(p Paths) string {
	b, err := os.ReadFile(p.Addr) //nolint:gosec // daemon-owned lifecycle file, not user input
	if err != nil {
		return "unix://" + p.Sock
	}
	return strings.TrimSpace(string(b))
}

// removeAddr deletes the addr file, ignoring absence.
func removeAddr(p Paths) error {
	if err := os.Remove(p.Addr); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove addr file %s: %w", p.Addr, err)
	}
	return nil
}

// socketFilePresent reports whether a socket file exists on disk regardless of
// whether anything is listening, used to detect stale residue.
func socketFilePresent(sockPath string) bool {
	_, err := os.Stat(sockPath)
	return err == nil
}
