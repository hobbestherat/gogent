// Package diag routes diagnostic messages (warnings, errors) to a configurable
// sink so they never corrupt the TUI's alternate screen (issue #17). In TUI mode
// the sink is a log file; in headless mode it is standard error. It is the thin
// seed the structured-logging/audit stream (issue #51) will grow from, so the
// surface here is deliberately small: a few leveled methods over a single writer.
package diag

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level classifies a diagnostic message.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

func (l Level) tag() string {
	switch l {
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Logger writes timestamped, leveled diagnostic lines to a single sink.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New returns a logger that writes to w. A nil sink is silently discarded (via
// io.Discard) so callers can disable diagnostics without nil checks at every
// call site.
func New(w io.Writer) *Logger {
	if w == nil {
		w = io.Discard
	}
	return &Logger{w: w}
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
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return New(f), nil
}

// logf formats and writes a single diagnostic line. A nil logger is a no-op so
// embedders that never configure one are unaffected.
func (l *Logger) logf(level Level, format string, args ...any) {
	if l == nil {
		return
	}
	line := time.Now().UTC().Format(time.RFC3339) + " " + level.tag() + " " + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, line)
}

// Infof logs an informational message.
func (l *Logger) Infof(format string, args ...any) { l.logf(LevelInfo, format, args...) }

// Warnf logs a warning.
func (l *Logger) Warnf(format string, args ...any) { l.logf(LevelWarn, format, args...) }

// Errorf logs an error.
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }
