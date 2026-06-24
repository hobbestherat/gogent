package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WritePidfile atomically writes pid to path. It writes a temp sibling and
// renames so a concurrent reader never observes a half-written file, and a
// crash mid-write leaves the previous pidfile (or none) intact rather than a
// truncated one. The parent directory is created if missing.
func WritePidfile(path string, pid int) error {
	if err := PathsFor(dirOf(path)).ensureDir(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write pidfile %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename pidfile %s: %w", path, err)
	}
	return nil
}

// ReadPidfile reads and parses the pid stored at path. A missing file returns a
// wrapped os.ErrNotExist (test with errors.Is); a present-but-garbage file
// returns a parse error so a corrupt pidfile is distinguishable from absence.
func ReadPidfile(path string) (int, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is a daemon-owned lifecycle file, not user input
	if err != nil {
		return 0, fmt.Errorf("read pidfile %s: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %s: %w", path, err)
	}
	return pid, nil
}

// RemovePidfile deletes the pidfile. A missing file is not an error, so cleanup
// is idempotent across graceful shutdown, forced stop and stale reclamation.
func RemovePidfile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pidfile %s: %w", path, err)
	}
	return nil
}

// ProcessAlive reports whether a process with the given pid currently exists.
// Its implementation is OS-specific (signal 0 on Unix, OpenProcess on Windows)
// and lives in the per-platform files; the contract is identical: a non-positive
// pid is never alive.

// dirOf returns the directory component of a lifecycle-file path, used to derive
// the daemon root so WritePidfile can create it on demand. It uses filepath.Dir
// so it honours the platform separator (e.g. backslash paths on Windows) rather
// than assuming "/"; on Unix the result is unchanged.
func dirOf(path string) string {
	return filepath.Dir(path)
}
