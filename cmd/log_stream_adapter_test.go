package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gogent/internal/diag"
)

// These tests cover the daemon-side adapter that bridges *diag.Ring to
// server.LogStreamer (issue #562) — the single point where internal/diag and
// internal/server meet — plus the slog-level → wire-string mapping the client
// colours by.

func TestLevelString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "INFO"}, // below Info collapses up to INFO
		{slog.LevelInfo, "INFO"},
		{slog.Level(2), "INFO"}, // Notice sits between Info and Warn → INFO
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
		{slog.Level(12), "ERROR"}, // above Error → ERROR
	}
	for _, c := range cases {
		if got := levelString(c.level); got != c.want {
			t.Fatalf("levelString(%v) = %q, want %q", c.level, got, c.want)
		}
	}
}

func TestRingLogStreamer_SnapshotMapsRecords(t *testing.T) {
	t.Parallel()
	ring := diag.NewRing(8)
	lg := diag.NewWithRing(io.Discard, ring)
	lg.Info("hello", "k", "v")
	lg.Warn("careful")
	lg.Error("nope")

	got := ringLogStreamer{ring: ring}.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(got))
	}
	want := []string{"INFO", "WARN", "ERROR"}
	for i, lvl := range want {
		if got[i].Level != lvl {
			t.Fatalf("record %d level = %q, want %q", i, got[i].Level, lvl)
		}
		if got[i].Time.IsZero() {
			t.Fatalf("record %d has zero time", i)
		}
	}
	if !strings.Contains(got[0].Text, "hello") || !strings.Contains(got[0].Text, "k=v") {
		t.Fatalf("first record text = %q", got[0].Text)
	}
}

func TestRingLogStreamer_SnapshotEmptyWhenNoLogs(t *testing.T) {
	t.Parallel()
	got := ringLogStreamer{ring: diag.NewRing(4)}.Snapshot()
	if len(got) != 0 {
		t.Fatalf("empty ring Snapshot = %+v, want none", got)
	}
}

func TestRingLogStreamer_SubscribeBridgesAndClosesOnUnsub(t *testing.T) {
	t.Parallel()
	ring := diag.NewRing(8)
	out, unsub := ringLogStreamer{ring: ring}.Subscribe()

	lg := diag.NewWithRing(io.Discard, ring)
	lg.Error("streamed")

	select {
	case rec := <-out:
		if rec.Level != "ERROR" || !strings.Contains(rec.Text, "streamed") {
			t.Fatalf("bridged record = %+v", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter did not bridge the live record onto its channel")
	}

	unsub()
	// The converter goroutine must close `out` once unsubscribed.
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("adapter output channel still open after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("adapter output channel not closed after unsubscribe")
	}
}

// Subscribe must also deliver records appended to the ring's rolling history
// before a subscriber attaches only via Snapshot, and live records thereafter —
// mirroring how the server primes then streams.
func TestRingLogStreamer_SnapshotSeesHistoryLiveSeesNew(t *testing.T) {
	t.Parallel()
	ring := diag.NewRing(8)
	lg := diag.NewWithRing(io.Discard, ring)
	lg.Info("hist")

	snap := ringLogStreamer{ring: ring}.Snapshot()
	if len(snap) != 1 || !strings.Contains(snap[0].Text, "hist") {
		t.Fatalf("snapshot = %+v, want [hist]", snap)
	}

	out, unsub := ringLogStreamer{ring: ring}.Subscribe()
	defer unsub()
	lg.Warn("live")
	select {
	case rec := <-out:
		if rec.Level != "WARN" || !strings.Contains(rec.Text, "live") {
			t.Fatalf("live = %+v", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive the post-subscribe live record")
	}
}
