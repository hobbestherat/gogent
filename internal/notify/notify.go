// Package notify emits desktop/terminal notifications when a long task finishes
// or a session needs attention, so a user can step away from the terminal
// (issue #59).
//
// Three delivery channels are supported and independently toggleable:
//
//   - Bell — the terminal bell (\a), which most terminals map to an audible
//     alert, a visual flash, or a urgency hint on the window.
//   - Desktop — an OSC 9 (iTerm2/Ghostty) and OSC 777 (rxvt-unicode and others)
//     desktop-notification escape sequence. These are written to the terminal
//     and processed by capable terminal emulators; unsupported terminals ignore
//     them, so emitting them is always harmless.
//   - Native — shells out to the platform's notifier binary (notify-send on
//     Linux/BSDs, terminal-notifier on macOS) when one is on $PATH.
//
// The package is UI-agnostic: callers decide which events are worth a
// notification (see Reason) and whether the originating session is focused.
package notify

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"gogent/internal/config"
)

// Reason categorizes why a notification is being emitted, so the per-event
// toggles in config.NotifyConfig can gate it.
type Reason string

const (
	// ReasonComplete: a task finished (final assistant answer).
	ReasonComplete Reason = "complete"
	// ReasonError: a task errored.
	ReasonError Reason = "error"
	// ReasonApproval: a permission prompt is waiting for an answer.
	ReasonApproval Reason = "approval"
	// ReasonClarify: a sub-agent asked a question (interactive CLARIFY).
	ReasonClarify Reason = "clarify"
)

// bell is the ASCII BEL control character.
const bell = "\x07"

// esc formats an OSC (Operating System Command) sequence, terminating it with
// BEL. Params are joined by ';'.
func esc(params ...string) string {
	return "\x1b]" + strings.Join(params, ";") + bell
}

// sanitize strips characters that would prematurely terminate an OSC sequence
// (BEL, the ST escape) or otherwise confuse the terminal (other control
// characters and newlines). It keeps the payload on a single line.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\x07', '\x1b', '\n', '\r', '\t': // BEL, ESC, newlines, tabs -> space
			b.WriteByte(' ')
		default:
			if r < 0x20 || r == 0x7f {
				continue // drop other control bytes
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// desktopSequence returns the OSC desktop-notification escape sequences for
// title/body. OSC 9 (iTerm2/Ghostty) takes a single message string, so title and
// body are combined; OSC 777 (rxvt-unicode and others) takes "notify;title;body".
// Both are emitted so the union of capable terminals picks one up.
func desktopSequence(title, body string) string {
	title = sanitize(title)
	body = sanitize(body)
	message := body
	if title != "" {
		message = title + " — " + body
	}
	return esc("9", message) + esc("777", "notify", title, body)
}

// lookPathFunc resolves a binary name to an executable path (or an error). It
// mirrors exec.LookPath and is a seam so nativeCommand can be unit tested without
// touching the real $PATH.
type lookPathFunc func(file string) (string, error)

// nativeCommand selects the native notifier binary and its arguments for the
// given GOOS, using look to find it on $PATH. It returns ("", nil) when the
// platform has no supported notifier or it is not installed.
func nativeCommand(goos string, look lookPathFunc, title, body string) (string, []string) {
	title = sanitize(title)
	body = sanitize(body)
	switch goos {
	case "darwin":
		if p, err := look("terminal-notifier"); err == nil {
			return p, []string{"-title", "Gogent", "-subtitle", title, "-message", body}
		}
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		if p, err := look("notify-send"); err == nil {
			return p, []string{"--app-name=Gogent", title, body}
		}
	}
	return "", nil
}

// nativeRunner shells out to the OS notifier for one notification. It is a seam
// so Notifier can be tested without spawning real processes.
type nativeRunner func(title, body string) error

// defaultNative runs the platform notifier (a no-op when none is installed).
func defaultNative(title, body string) error {
	name, args := nativeCommand(runtime.GOOS, exec.LookPath, title, body)
	if name == "" {
		return nil
	}
	cmd := exec.Command(name, args...) //nolint:gosec // launches configured/trusted local notifier binary
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run notifier: %w", err)
	}
	return nil
}

// Notifier emits user-facing notifications for session attention events. It is
// safe for concurrent use: the live config is guarded by a read/write lock and
// terminal writes are serialized so two notifications can't interleave their
// escape sequences.
type Notifier struct {
	mu      sync.RWMutex
	cfg     config.NotifyConfig
	writeMu sync.Mutex
	out     io.Writer
	native  nativeRunner
}

// New builds a Notifier that writes terminal escapes to out (os.Stdout in
// production) and, when the native channel is enabled, shells out to the
// platform notifier.
func New(cfg config.NotifyConfig, out io.Writer) *Notifier {
	return &Notifier{cfg: cfg, out: out, native: defaultNative}
}

// SetConfig updates the live notification configuration.
func (n *Notifier) SetConfig(cfg config.NotifyConfig) {
	n.mu.Lock()
	n.cfg = cfg
	n.mu.Unlock()
}

// Config returns a snapshot of the current configuration.
func (n *Notifier) Config() config.NotifyConfig {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.cfg
}

// reasonOn reports whether the per-event toggle for r is on.
func reasonOn(cfg config.NotifyConfig, r Reason) bool {
	switch r {
	case ReasonComplete:
		return cfg.OnComplete
	case ReasonError:
		return cfg.OnError
	case ReasonApproval:
		return cfg.OnApproval
	case ReasonClarify:
		return cfg.OnClarify
	}
	return false
}

// ReasonEnabled reports whether notifications of r are turned on at all — the
// master switch plus the per-event toggle. It ignores focus.
func (n *Notifier) ReasonEnabled(r Reason) bool {
	cfg := n.Config()
	return cfg.Enabled && reasonOn(cfg, r)
}

// ShouldNotify reports whether a notification for r should fire, considering the
// per-event toggle and the optional suppression of the focused session.
func (n *Notifier) ShouldNotify(r Reason, focused bool) bool {
	cfg := n.Config()
	if !cfg.Enabled || !reasonOn(cfg, r) {
		return false
	}
	if focused && cfg.SuppressWhenFocused {
		return false
	}
	return true
}

// Notify emits every configured channel. Callers should gate it with ShouldNotify
// (Notify itself does not check per-event toggles, so the same Notifier can be
// reused to force-emit when desired). The bell and desktop sequences are written
// synchronously (they are tiny); the native notifier runs in its own goroutine so
// a slow external binary never stalls the caller.
//
// title/body are sanitized once here so every channel — including a custom
// native runner — receives clean, single-line text (the helpers sanitize again
// defensively, which is idempotent).
func (n *Notifier) Notify(title, body string) {
	cfg := n.Config()
	title = sanitize(title)
	body = sanitize(body)
	var b strings.Builder
	if cfg.Bell {
		b.WriteString(bell)
	}
	if cfg.Desktop {
		b.WriteString(desktopSequence(title, body))
	}
	if b.Len() > 0 {
		n.writeMu.Lock()
		_, _ = io.WriteString(n.out, b.String())
		n.writeMu.Unlock()
	}
	if cfg.Native && n.native != nil {
		runner := n.native
		go func() { _ = runner(title, body) }()
	}
}
