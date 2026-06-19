// Package diag routes diagnostic messages (warnings, errors) to a configurable
// sink so they never corrupt the TUI's alternate screen (issue #17). In TUI mode
// the sink is a log file; in headless mode it is standard error.
//
// Diagnostics are structured: the package is a thin wrapper over the standard
// library's log/slog (issue #51), so every record carries a timestamp, a level
// and typed key/value attributes. Logger.With binds context (session/agent ids)
// that then rides along on every line, and the separate Audit stream records
// security-relevant events (permission decisions, tool calls) as an append-only
// post-incident artifact. Use Secret to keep API keys and tokens out of the logs.
package diag

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Logger writes structured, leveled diagnostic records to a single sink. The
// printf-style methods (Infof/Warnf/Errorf) bridge the many call sites that
// pre-date structured logging; Info/Warn/Error take typed slog attributes and
// With binds attributes (e.g. session/agent ids) onto every later record.
//
// A nil *Logger is a safe no-op, so embedders that never configure one — and
// callers that hold an unset field — are unaffected.
type Logger struct {
	sl *slog.Logger
}

// New returns a logger that writes text-format records to w. A nil sink is
// silently discarded (via io.Discard) so callers can disable diagnostics without
// nil checks at every call site.
func New(w io.Writer) *Logger {
	if w == nil {
		w = io.Discard
	}
	return &Logger{sl: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))}
}

// Stderr returns the default headless logger, writing to standard error. The TUI
// entry point redirects away from this to a file so diagnostics never visually
// corrupt the alternate screen.
func Stderr() *Logger { return New(os.Stderr) }

// NewFile opens (or creates) a log file at path for appending and returns a
// logger writing to it. The parent directory is created if needed. It is the
// TUI-mode sink: a file is used instead of stderr so diagnostics land somewhere
// visible without disturbing the rendered screen.
func NewFile(path string) (*Logger, error) {
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return New(f), nil
}

// With returns a child logger that adds args (alternating key/value pairs, or
// slog.Attr values) to every record it writes. Use it to thread session and
// agent ids so a model event can be correlated to the tool outcome it caused.
func (l *Logger) With(args ...any) *Logger {
	if l == nil || l.sl == nil {
		return l
	}
	return &Logger{sl: l.sl.With(args...)}
}

func (l *Logger) log(level slog.Level, msg string, args ...any) {
	if l == nil || l.sl == nil {
		return
	}
	l.sl.Log(context.Background(), level, msg, args...)
}

// Info logs an informational message with typed attributes.
func (l *Logger) Info(msg string, args ...any) { l.log(slog.LevelInfo, msg, args...) }

// Warn logs a warning with typed attributes.
func (l *Logger) Warn(msg string, args ...any) { l.log(slog.LevelWarn, msg, args...) }

// Error logs an error with typed attributes.
func (l *Logger) Error(msg string, args ...any) { l.log(slog.LevelError, msg, args...) }

// Infof logs a preformatted informational message. Prefer Info with attributes
// for new code; this exists for call sites that pre-date structured logging.
func (l *Logger) Infof(format string, args ...any) { l.log(slog.LevelInfo, fmt.Sprintf(format, args...)) }

// Warnf logs a preformatted warning.
func (l *Logger) Warnf(format string, args ...any) { l.log(slog.LevelWarn, fmt.Sprintf(format, args...)) }

// Errorf logs a preformatted error.
func (l *Logger) Errorf(format string, args ...any) {
	l.log(slog.LevelError, fmt.Sprintf(format, args...))
}

// openAppend opens path for appending, creating the parent directory and the
// file as needed. It backs both the diagnostic and audit file sinks.
func openAppend(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}
