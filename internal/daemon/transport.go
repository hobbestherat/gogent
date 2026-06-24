package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ErrAlreadyRunning is returned when a live daemon already holds the instance,
// so the caller refuses a double-start instead of clobbering a running one. The
// guard is OS-specific (flock on Unix, pidfile + TCP liveness on Windows) but
// the sentinel is shared.
var ErrAlreadyRunning = errors.New("daemon already running")

// ErrNotRunning is returned by operations (e.g. Stop) that need a live daemon
// when none is found.
var ErrNotRunning = errors.New("daemon not running")

// healthPath is the unauthenticated endpoint the daemon's HTTP handler serves;
// a successful response over the transport proves a live daemon (not merely a
// leftover socket/addr file). exitPath asks a live daemon to shut itself down.
const (
	healthPath = "/health"
	exitPath   = "/exit"
)

// probeTimeout bounds a single liveness dial+request so a probe never blocks a
// status/start/stop command on a wedged peer. Shared by the Unix socket and the
// Windows TCP transports.
const probeTimeout = 2 * time.Second

// httpHealthOK issues GET <base>/health with the supplied client and reports a
// 2xx. base is the request origin the platform transport understands —
// "http://unix" for the Unix-socket dialer, or "http://host:port" for TCP. It
// is the single liveness primitive both transports share; only the client and
// base differ.
func httpHealthOK(c *http.Client, base string) bool {
	req, err := http.NewRequest(http.MethodGet, base+healthPath, nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// httpExitOK issues POST <base>/exit with the supplied client and reports
// whether the daemon accepted the shutdown request (2xx). It targets the real
// daemon process through the transport it owns, so it works even when the
// pidfile is missing, stale or corrupt — the basis for Stop's safe graceful
// path. Shared by both transports.
func httpExitOK(c *http.Client, base string) bool {
	req, err := http.NewRequest(http.MethodPost, base+exitPath, nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// removeSocket deletes the Unix socket file, ignoring absence, for graceful-
// shutdown and stale-reclamation cleanup. It is shared (called by cleanStale on
// every platform); on Windows no socket file is created, so the os.Remove is a
// harmless no-op.
func removeSocket(sockPath string) error {
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove socket %s: %w", sockPath, err)
	}
	return nil
}
