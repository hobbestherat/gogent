//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
)

// This file supplies the Windows implementations of the per-platform hooks the
// shared daemon orchestration depends on. Windows has no Unix-domain sockets and
// no true double-fork/setsid daemonization, so the transport is loopback TCP and
// detachment is a background process spawned with the DETACHED_PROCESS /
// CREATE_NEW_PROCESS_GROUP creation flags.
//
// This is deliberately NOT true service-style daemonization: the spawned process
// survives the launching console closing, but it is an ordinary user process,
// not a managed service. For survival across logout/reboot, run the daemon under
// a service supervisor (NSSM, a Windows Service wrapper) or `gogent daemon start
// --foreground` under Task Scheduler. Single-instance is enforced by an
// OS-exclusive handle on daemon.lock — the Windows analog of Unix flock, so a
// second start is refused race-free even on a concurrent cold start; the TCP
// /health probe is used only for status/liveness reporting. The CLI subcommands
// (start/stop/status/restart) and pidfile/addr discovery behave the same as on
// Unix.

// Windows process-creation flags (winbase.h). Defined locally so we depend only
// on the syscall stdlib, not golang.org/x/sys/windows.
const (
	createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	detachedProcess       = 0x00000008 // DETACHED_PROCESS
)

// stillActive is STILL_ACTIVE (winbase.h): the exit code a process reports while
// it is still running. GetExitCodeProcess returns it for a live process.
const stillActive = 259

// ProcessAlive reports whether a process with the given pid currently exists. On
// Windows os.FindProcess always succeeds and signal 0 is unavailable, so it opens
// the process and checks its exit code: a live process reports STILL_ACTIVE; a
// pid that cannot be opened (gone, or never existed) is not alive. A non-positive
// pid is never alive.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// detachSysProcAttr returns the attributes that detach the spawned daemon from
// the launching console so it survives the console closing: DETACHED_PROCESS
// gives the child no inherited console, and CREATE_NEW_PROCESS_GROUP isolates it
// from console Ctrl+C/Ctrl+Break sent to the parent's group. (DETACHED_PROCESS,
// CREATE_NEW_CONSOLE and CREATE_NO_WINDOW are mutually exclusive console options;
// DETACHED_PROCESS is the right one for a background daemon.)
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

// gracefulSignal is a no-op on Windows: there is no SIGTERM equivalent for a
// console-less detached process, so the graceful path is the daemon's own /exit
// endpoint (handled by Stop before it reaches here). Returning nil means Stop
// proceeds to wait out the grace period and then force-kills under --force,
// preserving the same --force gating as Unix.
func gracefulSignal(pid int) error {
	_ = pid
	return nil
}

// forceKill terminates the daemon immediately (TerminateProcess, via
// os.Process.Kill) when a graceful stop did not complete and --force was given.
func forceKill(pid int) error {
	pr, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	if err := pr.Kill(); err != nil {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	return nil
}

// probeLive reports whether a live daemon answers a health probe on the Windows
// transport — the loopback TCP address recorded in daemon.addr.
func probeLive(p Paths) bool {
	base := tcpBase(p)
	if base == "" {
		return false
	}
	return httpHealthOK(tcpClient(), base)
}

// exitLive asks a live daemon to shut down via its own /exit endpoint over the
// recorded TCP address.
func exitLive(p Paths) bool {
	base := tcpBase(p)
	if base == "" {
		return false
	}
	return httpExitOK(tcpClient(), base)
}

// transportResidue reports whether on-disk transport residue exists. Windows has
// no socket file, so the discovery file is daemon.addr; its presence (with no
// live daemon) marks a crashed/half-torn-down instance.
func transportResidue(p Paths) bool {
	_, err := os.Stat(p.Addr)
	return err == nil
}

// primaryAddr is the human-facing identifier of the primary local transport,
// used in Stop diagnostics. On Windows it is the recorded loopback TCP address.
func primaryAddr(p Paths) string {
	if b := tcpBase(p); b != "" {
		return b
	}
	return readAddr(p)
}

// ListenLocal binds the daemon's primary local transport and returns the listener
// together with the scheme-qualified discovery address to record in daemon.addr.
// On Windows this is a loopback TCP listener on an ephemeral port; the address is
// "http://127.0.0.1:<port>" so a local client (and curl) can attach over HTTP.
//
// Single-instance is enforced by holding an OS-exclusive handle on daemon.lock
// for the daemon's lifetime — the Windows analog of Unix flock. This is taken
// BEFORE binding, so two concurrent cold starts cannot both proceed: exactly one
// wins the exclusive handle and the loser gets ErrAlreadyRunning. The lock is
// released (and the next start freed) when the returned listener is Closed.
func ListenLocal(p Paths) (net.Listener, string, error) {
	if err := p.ensureDir(); err != nil {
		return nil, "", err
	}

	lock, err := acquireWindowsLock(p.Lock)
	if err != nil {
		return nil, "", err // ErrAlreadyRunning, or a wrapped open error
	}

	// We hold the exclusive lock: no other daemon is live, so any pidfile/addr
	// here is stale crash residue, safe to reclaim before binding.
	_ = cleanStale(p)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = lock.Close()
		return nil, "", fmt.Errorf("listen on loopback tcp: %w", err)
	}
	return &lockedListener{Listener: ln, lock: lock}, "http://" + ln.Addr().String(), nil
}

// lockedListener couples the loopback TCP listener with the single-instance lock
// handle so the lock lives exactly as long as the listener: closing the listener
// releases the lock, freeing the next start to bind.
type lockedListener struct {
	net.Listener
	lock *os.File
}

// Close closes the underlying listener and releases the single-instance lock.
func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	if l.lock != nil {
		_ = l.lock.Close()
		l.lock = nil
	}
	if err != nil {
		return fmt.Errorf("close listener: %w", err)
	}
	return nil
}

// errSharingViolation is ERROR_SHARING_VIOLATION (winerror.h): the error a
// share-mode-0 CreateFile returns when another process already holds the file
// open — i.e. another live daemon holds the single-instance lock.
const errSharingViolation = syscall.Errno(32)

// acquireWindowsLock opens (creating if needed) the lock file with a zero share
// mode — exclusive access, the Windows analog of flock — and returns the holding
// *os.File. The handle must be kept open for the lock to persist and Closed to
// release it; the OS releases it automatically if the holder dies (even on a hard
// kill), so it never goes stale. A live holder maps to ErrAlreadyRunning; a stale
// lock file from a crashed daemon opens cleanly because the dead process's handle
// was already released. Any other failure is wrapped.
func acquireWindowsLock(path string) (*os.File, error) {
	namep, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("lock path %s: %w", path, err)
	}
	h, err := syscall.CreateFile(
		namep,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // zero share mode: exclusive
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	return os.NewFile(uintptr(h), path), nil
}

// ProbeLocal reports whether a live daemon answers on the primary local transport,
// for the default-invocation auto-attach decision in cmd.
func ProbeLocal(p Paths) bool {
	return probeLive(p)
}

// LocalDiscoveryAddr returns the address a local TUI would attach to: on Windows
// the loopback TCP address recorded by a running daemon ("http://127.0.0.1:port"),
// or "" when no daemon has recorded one. cmd only uses it when ProbeLocal reports
// a live daemon, so the empty case never drives an attach.
func LocalDiscoveryAddr(p Paths) string {
	return tcpBase(p)
}

// tcpBase returns the primary HTTP origin from the recorded discovery address
// (the first space-separated field of daemon.addr), or "" when none is recorded
// or it is not an http(s) address. The daemon may record a second, optional
// --tcp endpoint after the primary; the probe targets the primary.
func tcpBase(p Paths) string {
	fields := strings.Fields(readAddr(p))
	if len(fields) == 0 {
		return ""
	}
	addr := fields[0]
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return ""
}

// tcpClient builds an HTTP/1.1 client bounded by probeTimeout for the loopback
// TCP liveness/exit requests.
func tcpClient() *http.Client {
	return &http.Client{Timeout: probeTimeout}
}
