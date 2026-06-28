package ui

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gogent/internal/diag"
)

// The persistent Logs window (issue #562) surfaces gogent's structured
// diagnostics in-app. It is a read-only SessionWindow (winLogs) so it joins
// w.sessions/w.order and inherits tiling, cycle, raise, focus, minimize/move,
// activeIDLocked and layout-exclusion exactly like an analysis window — no
// special-casing in those paths. Its body is the shell's transcriptModel: each
// log line is a kindLog record, so the search/filter/fold/yank toolkit and the
// amortised line cap (transcriptModel.trim at limit) work unchanged, and the
// auto-follow-only-at-bottom / no-yank-when-scrolled-up behaviour comes for free
// from add()→renderOne respecting the current scroll position.
//
// In remote/attach mode the window interlaces the client's own logs ([local])
// with the daemon's logs ([daemon]) streamed over GET /api/logs/stream, ordered
// by arrival (each line shows its own host's clock; the two hosts' clocks are not
// merged on an absolute timeline, which would be meaningless under skew).

const (
	logsWindowID    = "logs"
	logsWindowTitle = "Logs"
	// logsDisplayLimit caps the records kept live in the window's transcript. The
	// shell's transcriptModel.trim() drops the oldest ~10% past this, amortised —
	// the same mechanism every live session window uses. The diag ring keeps a
	// larger history (logRingSize) so a reopen can refill the view.
	logsDisplayLimit = 1000
)

// logSource tags a record's origin for the [local]/[daemon] prefix shown in
// remote mode.
type logSource int

const (
	logLocal logSource = iota
	logDaemon
)

// logsState holds the Logs window's live wiring (issue #562). ring and
// startDaemon are configured at startup (SetLogRing / SetDaemonLogStream) and
// persist across open/close; cancel and showSource are per-open and reset on
// close. All fields are read/written on the UI thread except the ring/stream
// channels, which marshal back via w.Post.
type logsState struct {
	// ring is the local diagnostic-log ring the diag logger tees into — the
	// [local] stream source. nil disables local capture (the window then shows
	// only [daemon] records, or nothing).
	ring *diag.Ring
	// startDaemon, when set (attach mode only), streams the daemon's logs into the
	// given sink until ctx is cancelled. nil in embedded mode, where there is no
	// [daemon] stream and source tags are omitted.
	startDaemon func(ctx context.Context, sink func(LogRecordDTO))
	// cancel stops the live subscription goroutines; nil while the window is closed.
	cancel context.CancelFunc
	// showSource tags each line [local]/[daemon]; true only in remote mode.
	showSource bool
}

// SetLogRing wires the in-memory diagnostic-log ring the Logs window reads as its
// [local] stream (issue #562). Call once at startup, before the UI loop. A nil
// ring is safe — the window simply has no local capture.
func (w *Workbench) SetLogRing(r *diag.Ring) { w.logs.ring = r }

// SetDaemonLogStream wires the daemon-log stream starter used to interlace
// [daemon] logs in attach mode (issue #562). start streams records into sink
// until ctx is cancelled; the attach path passes RemoteClient.StreamLogsTo. A nil
// starter (embedded mode) leaves the window local-only and omits source tags.
func (w *Workbench) SetDaemonLogStream(start func(ctx context.Context, sink func(LogRecordDTO))) {
	w.logs.startDaemon = start
}

// showLogsWindow opens the persistent Logs window, or raises it if already open
// (issue #562). It is wired to the Settings ▸ Logs… menu item and the optional
// chord. On first open it primes from the ring's recent history, then follows the
// live tail; in attach mode it also starts the [daemon] stream. Runs on the UI
// thread.
func (w *Workbench) showLogsWindow() {
	w.mu.Lock()
	existing := w.sessions[logsWindowID]
	w.mu.Unlock()
	if existing != nil {
		// Already open: raise + focus, never duplicate (the openWindowKind id guard
		// would also catch this, but raising is the intended UX).
		w.Focus(logsWindowID)
		return
	}

	sw := w.openLogsWindow(logsWindowID, logsWindowTitle)
	sw.transcript.limit = logsDisplayLimit

	remote := w.logs.startDaemon != nil
	w.logs.showSource = remote

	// Prime from the local ring's recent history (oldest first), capped to the
	// display limit so priming does not immediately trigger a trim rebuild.
	hist := w.logs.ring.Snapshot()
	if len(hist) > logsDisplayLimit {
		hist = hist[len(hist)-logsDisplayLimit:]
	}
	for _, rec := range hist {
		w.appendLogLine(logLocal, rec.Time, levelName(rec.Level), rec.Text)
	}

	// Live subscriptions, bounded by a cancel stored for close (and parented on the
	// workbench shutdown context so app-quit also stops them). A brief gap between
	// the snapshot above and the subscribe below can drop a record — acceptable for
	// a best-effort diagnostic tail.
	ctx, cancel := context.WithCancel(w.shutdown)
	w.logs.cancel = cancel

	sub, unsub := w.logs.ring.Subscribe()
	go func() {
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case rec, ok := <-sub:
				if !ok {
					return
				}
				r := rec
				w.Post(func() { w.appendLogLine(logLocal, r.Time, levelName(r.Level), r.Text) })
			}
		}
	}()

	if remote {
		go w.logs.startDaemon(ctx, func(rec LogRecordDTO) {
			r := rec
			w.Post(func() { w.appendLogLine(logDaemon, parseLogTime(r.Time), r.Level, r.Text) })
		})
	}

	w.Focus(logsWindowID)
}

// closeLogsWindow tears down the live subscriptions when the Logs window closes.
// It is invoked from CloseSession for the logs id, so a closed window holds no
// ring subscription or daemon stream. Runs on the UI thread.
func (w *Workbench) closeLogsWindow() {
	if w.logs.cancel != nil {
		w.logs.cancel()
		w.logs.cancel = nil
	}
	w.logs.showSource = false
}

// appendLogLine renders one diagnostic record into the open Logs window. It must
// run on the UI thread (callers marshal via w.Post). Each record becomes a
// kindLog transcript record — going through the transcript model (not the raw
// view) so it survives a search/filter/fold re-render and participates in the
// line cap. Level drives the colour role; the source tag is shown only in remote
// mode. A no-op if the window is closed.
func (w *Workbench) appendLogLine(src logSource, t time.Time, level, text string) {
	w.mu.Lock()
	sw := w.sessions[logsWindowID]
	w.mu.Unlock()
	if sw == nil {
		return
	}
	var tag string
	if w.logs.showSource {
		switch src {
		case logLocal:
			tag = "[local] "
		case logDaemon:
			tag = "[daemon] "
		}
	}
	header := fmt.Sprintf("%s %s%-5s %s", t.Format("15:04:05.000"), tag, level, text)
	sw.transcript.add(&transcriptRecord{
		kind:   kindLog,
		header: header,
		role:   roleForLevel(level),
	})
	w.desktop.RequestRedraw()
}

// roleForLevel maps a level name to its transcript colour role: info=cyan,
// warn=yellow, error=red (issue #562).
func roleForLevel(level string) colorRole {
	switch level {
	case "WARN":
		return roleWarn
	case "ERROR":
		return roleError
	default:
		return roleInfo
	}
}

// levelName renders an slog level as the canonical uppercase name used for the
// [local] stream's colouring, matching the daemon's wire encoding.
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	default:
		return "INFO"
	}
}

// parseLogTime parses a daemon record's RFC3339Nano timestamp, falling back to
// the current time if it is malformed so the line still carries a sensible clock.
func parseLogTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Now()
}
