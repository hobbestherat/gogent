package daemon

// This file holds the daemon's OS-independent policy decisions, factored out of
// Query and Stop so they can be unit-tested on any platform without real
// sockets, processes or TCP listeners. The OS-specific mechanisms (Unix-socket
// vs TCP liveness, signals vs /exit) are supplied to Query/Stop by the
// per-platform files; the decisions below take their results as plain booleans.

// ClassifyStatus derives a daemon's running/stale state from raw observations,
// independent of OS and side effects:
//
//   - pidPresent — a readable pidfile exists.
//   - pidAlive   — that pid is an existing process (already AND'd with
//     pidPresent by the caller).
//   - residue    — transport residue exists on disk (Unix socket file / Windows
//     addr file) regardless of whether anything is listening.
//   - live       — the transport answers a health probe.
//
// Running requires both a live process and a live transport — a daemon that is
// actually serving, not merely a leftover pidfile. Anything less, with some
// residue still on disk, is the Stale remains of a crash, safe to reclaim on the
// next start. Query wraps this with the real probes; tests drive it directly.
func ClassifyStatus(pidPresent, pidAlive, residue, live bool) (running, stale bool) {
	running = pidAlive && live
	stale = !running && (pidPresent || residue)
	return running, stale
}

// StopMode is the OS-independent strategy Stop follows, chosen from a single
// liveness observation. It isolates the policy (which mechanism to use) from the
// OS-specific mechanism and the side effects.
type StopMode int

const (
	// StopModeReclaim: nothing is answering on the transport, so there is no live
	// daemon to stop — just reclaim any crash residue and report ErrNotRunning.
	StopModeReclaim StopMode = iota
	// StopModeGraceful: a daemon is answering, so ask it to shut down via its own
	// /exit endpoint (with a signal/force-kill fallback if a usable pid exists).
	StopModeGraceful
)

// DecideStopMode chooses the stop strategy from whether the daemon answers a
// health probe on its transport. Liveness is the sole gate: Stop never signals a
// pid behind a dead transport, so a bad pidfile can neither make us signal an
// unrelated process nor unlink a live peer.
func DecideStopMode(live bool) StopMode {
	if live {
		return StopModeGraceful
	}
	return StopModeReclaim
}
