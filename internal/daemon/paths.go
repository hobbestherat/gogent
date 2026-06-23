// Package daemon provides the process-lifecycle primitives for gogent's
// userspace daemon (issue #358, Phase 1): on-disk pidfile + Unix listening
// socket management, liveness detection, detached spawn and graceful/forced
// stop. It is stdlib-only and deliberately decoupled from *gogent.Gogent and
// the HTTP server: the cmd layer wires those onto the listener this package
// hands back. Everything here is unit-testable against a t.TempDir root.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// Lifecycle file names, all rooted under the daemon's directory (normally
// ~/.gogent). They are siblings of the existing session/config state so a
// single directory describes a daemon instance.
const (
	pidFile  = "daemon.pid"
	sockFile = "daemon.sock"
	addrFile = "daemon.addr"
	logFile  = "daemon.log"
	lockFile = "daemon.lock"
)

// Paths holds the absolute lifecycle-file locations for a daemon rooted at Dir.
// Production code uses PathsFor(DefaultDir()); tests point Dir at a t.TempDir so
// the whole lifecycle exercises real files without touching ~/.gogent.
type Paths struct {
	Dir  string // root directory (normally ~/.gogent)
	Pid  string // pidfile: <Dir>/daemon.pid
	Sock string // Unix listening socket: <Dir>/daemon.sock
	Addr string // chosen listen address, for discovery: <Dir>/daemon.addr
	Log  string // detached stdout/stderr log: <Dir>/daemon.log
	Lock string // single-instance flock file: <Dir>/daemon.lock
}

// PathsFor derives the lifecycle-file set for a daemon rooted at dir.
func PathsFor(dir string) Paths {
	return Paths{
		Dir:  dir,
		Pid:  filepath.Join(dir, pidFile),
		Sock: filepath.Join(dir, sockFile),
		Addr: filepath.Join(dir, addrFile),
		Log:  filepath.Join(dir, logFile),
		Lock: filepath.Join(dir, lockFile),
	}
}

// DefaultDir returns the standard daemon directory (~/.gogent), the same root
// the rest of gogent uses for its on-disk state.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".gogent"), nil
}

// ensureDir creates the daemon directory if absent, so a first-ever start does
// not fail merely because ~/.gogent has not been created yet.
func (p Paths) ensureDir() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create daemon dir %s: %w", p.Dir, err)
	}
	return nil
}
