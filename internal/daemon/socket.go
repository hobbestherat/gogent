//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

// Listen binds the daemon's Unix-domain socket and returns a listener that also
// holds the single-instance lock for the daemon's lifetime. It is the Unix
// transport; the Windows build binds loopback TCP instead (see
// platform_windows.go) and enforces single-instance via the pidfile + a TCP
// liveness probe rather than flock.
//
// Single-instance is enforced by an exclusive, non-blocking flock on a sibling
// daemon.lock file — a kernel-level guard that is race-free across concurrent
// starts and is released automatically if the holder dies (even on SIGKILL),
// so it never goes stale. Only once the lock is held — proving we are the sole
// instance — does Listen clear any leftover socket file and bind fresh; this is
// why a concurrent start can never unlink a live peer's socket. The lock is
// released when the returned listener is Closed.
//
// A second caller while a daemon is live gets ErrAlreadyRunning. The daemon
// directory is created if absent and the socket is chmod'd 0600 (filesystem
// permissions are the only access gate for the local transport).
func Listen(sockPath string) (net.Listener, error) {
	if err := PathsFor(filepath.Dir(sockPath)).ensureDir(); err != nil {
		return nil, err
	}

	lock, err := acquireLock(lockPathFor(sockPath))
	if err != nil {
		return nil, err // ErrAlreadyRunning, or a wrapped open/flock error
	}

	// We hold the exclusive lock: no other daemon is live, so a socket file here
	// is stale residue from a crash and is safe to remove before binding.
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = releaseLock(lock)
		return nil, fmt.Errorf("remove stale socket %s: %w", sockPath, err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = releaseLock(lock)
		return nil, fmt.Errorf("listen on unix socket %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = l.Close()
		_ = releaseLock(lock)
		return nil, fmt.Errorf("chmod socket %s: %w", sockPath, err)
	}
	return &lockedListener{Listener: l, lock: lock}, nil
}

// lockedListener couples a net.Listener with the single-instance lock so the
// lock lives exactly as long as the listener: closing the listener releases the
// lock, freeing the next start to bind.
type lockedListener struct {
	net.Listener
	lock *os.File
}

// Close closes the underlying listener and releases the single-instance lock.
func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	if l.lock != nil {
		_ = releaseLock(l.lock)
		l.lock = nil
	}
	if err != nil {
		return fmt.Errorf("close listener: %w", err)
	}
	return nil
}

// acquireLock opens (creating if needed) the lock file and takes an exclusive,
// non-blocking flock. A contended lock — another live daemon — maps to
// ErrAlreadyRunning; any other failure is wrapped. The returned *os.File must be
// kept open for the lock to persist and closed (releaseLock) to drop it.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is a daemon-owned lifecycle file, not user input
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return f, nil
}

// releaseLock drops the flock by closing the holding file descriptor (closing an
// flock'd fd releases the lock). The lock file itself is intentionally left in
// place: unlinking it would let a concurrent start create a fresh inode and lock
// that instead, defeating mutual exclusion.
func releaseLock(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}

// lockPathFor returns the single-instance lock path that is a sibling of the
// socket (e.g. <dir>/daemon.lock for <dir>/daemon.sock).
func lockPathFor(sockPath string) string {
	return filepath.Join(filepath.Dir(sockPath), lockFile)
}

// Probe reports whether a live daemon is answering on the Unix socket at
// sockPath. It dials the socket and issues GET /health over HTTP/1.1; any
// successful 2xx response means live. A missing socket, refused connection,
// timeout or non-2xx status all mean not-live (a stale file or no daemon).
func Probe(sockPath string) bool {
	if _, err := os.Stat(sockPath); err != nil {
		return false
	}
	// The host in the URL is ignored by the unix DialContext but required for a
	// well-formed request; "unix" is a conventional placeholder.
	return httpHealthOK(unixHTTPClient(sockPath), "http://unix")
}

// exitViaSocket asks a live daemon to shut itself down through its own /exit
// endpoint over the Unix socket. This targets the actual daemon process via the
// socket it owns, so it works correctly even when the pidfile is missing, stale
// or corrupt — the basis for Stop's safe graceful path. It reports whether the
// daemon accepted the request (2xx); any transport error or non-2xx is false.
func exitViaSocket(sockPath string) bool {
	return httpExitOK(unixHTTPClient(sockPath), "http://unix")
}

// unixHTTPClient builds an HTTP/1.1 client that dials the given Unix socket,
// bounded by probeTimeout. Shared by the health probe and the /exit request.
func unixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{}
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}
