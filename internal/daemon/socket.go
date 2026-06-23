package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// ErrAlreadyRunning is returned when a live daemon already holds the socket, so
// the caller refuses a double-start instead of clobbering a running instance.
var ErrAlreadyRunning = errors.New("daemon already running")

// ErrNotRunning is returned by operations (e.g. Stop) that need a live daemon
// when none is found.
var ErrNotRunning = errors.New("daemon not running")

// healthPath is the unauthenticated endpoint the daemon's HTTP handler serves;
// a successful response over the socket proves a live daemon (not merely a
// leftover socket file).
const healthPath = "/health"

// probeTimeout bounds a single liveness dial+request so Probe never blocks a
// status/start command on a wedged peer.
const probeTimeout = 2 * time.Second

// Listen creates a Unix-domain-socket listener at sockPath with 0600 perms
// (filesystem permissions are the only access gate for the local transport).
//
// It distinguishes a *live* daemon from a *stale* socket file left by a crash:
// if Probe finds a daemon answering, it returns ErrAlreadyRunning; otherwise it
// removes the leftover file and listens fresh. The daemon directory is created
// if absent.
func Listen(sockPath string) (net.Listener, error) {
	if Probe(sockPath) {
		return nil, ErrAlreadyRunning
	}
	if err := PathsFor(dirOf(sockPath)).ensureDir(); err != nil {
		return nil, err
	}
	// A stale socket file (nothing listening) blocks bind with EADDRINUSE; remove
	// it first. Probe above already ruled out a live listener.
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", sockPath, err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on unix socket %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod socket %s: %w", sockPath, err)
	}
	return l, nil
}

// Probe reports whether a live daemon is answering on the Unix socket at
// sockPath. It dials the socket and issues GET /health over HTTP/1.1; any
// successful 2xx response means live. A missing socket, refused connection,
// timeout or non-2xx status all mean not-live (a stale file or no daemon).
func Probe(sockPath string) bool {
	if _, err := os.Stat(sockPath); err != nil {
		return false
	}
	client := http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{}
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
	// The host in the URL is ignored by the unix DialContext but required for a
	// well-formed request; "unix" is a conventional placeholder.
	req, err := http.NewRequest(http.MethodGet, "http://unix"+healthPath, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// removeSocket deletes the socket file, ignoring absence, for graceful-shutdown
// and stale-reclamation cleanup.
func removeSocket(sockPath string) error {
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove socket %s: %w", sockPath, err)
	}
	return nil
}
