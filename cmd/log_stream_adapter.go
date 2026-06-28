package main

import (
	"log/slog"

	"gogent/internal/diag"
	"gogent/internal/server"
)

// logRingSize bounds the in-memory diagnostic-log ring (issue #562). It is the
// rolling history the Logs window primes from on open and the daemon streams to
// remote clients; sized well above the 50-line notification ring since logs are
// chattier and the window's own display cap (~1000) trims what is shown.
const logRingSize = 2000

// ringLogStreamer adapts a *diag.Ring to server.LogStreamer (issue #562). It is
// the single point where internal/diag and internal/server meet, keeping diag a
// leaf: the server consumes only this interface and never imports diag.
type ringLogStreamer struct{ ring *diag.Ring }

// Snapshot returns the ring's recent history as server records (oldest first).
func (a ringLogStreamer) Snapshot() []server.LogRecord {
	recs := a.ring.Snapshot()
	out := make([]server.LogRecord, len(recs))
	for i, r := range recs {
		out[i] = toServerLogRecord(r)
	}
	return out
}

// Subscribe bridges a diag-ring subscription to a server.LogRecord channel. A
// converter goroutine maps records until the caller's unsubscribe (or the ring's
// closure) stops it; the channel buffer plus the ring's own non-blocking send
// keep a slow client from ever stalling the logger.
func (a ringLogStreamer) Subscribe() (<-chan server.LogRecord, func()) {
	src, unsub := a.ring.Subscribe()
	out := make(chan server.LogRecord, logStreamBuffer)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case rec, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- toServerLogRecord(rec):
				case <-done:
					return
				}
			}
		}
	}()
	return out, func() {
		close(done)
		unsub()
	}
}

// logStreamBuffer is the converter channel depth; matches the ring's per-
// subscriber buffer.
const logStreamBuffer = 256

// toServerLogRecord maps a diag record to its server-facing form, rendering the
// slog level as the canonical INFO/WARN/ERROR string.
func toServerLogRecord(r diag.Record) server.LogRecord {
	return server.LogRecord{Time: r.Time, Level: levelString(r.Level), Text: r.Text}
}

// levelString renders an slog level as the canonical uppercase name used on the
// wire and for the client's level colouring.
func levelString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	default:
		return "INFO"
	}
}
