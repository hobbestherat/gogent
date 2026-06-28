package ui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tui "github.com/hobbestherat/turbotui"

	"gogent/internal/diag"
)

// These tests cover the persistent Logs window (issue #562): it is a read-only
// SessionWindow (winLogs) that inherits tiling/cycle/raise/layout-exclusion,
// surfaces diag records through the transcript model (so search/filter/fold and
// the amortised line cap work), colours by level, tags [local]/[daemon] only in
// remote mode, and focuses its transcript on open.

// logsKindRecords returns the kindLog records in the open Logs window's
// transcript, in append order.
func logsKindRecords(t *testing.T, w *Workbench) []*transcriptRecord {
	t.Helper()
	sw := w.sessions[logsWindowID]
	if sw == nil {
		t.Fatalf("logs window not open")
	}
	var out []*transcriptRecord
	for _, r := range sw.transcript.records {
		if r.kind == kindLog {
			out = append(out, r)
		}
	}
	return out
}

func TestShowLogsWindow_OpensReadOnlyLogsWindow(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	defer w.CloseSession(logsWindowID)

	w.showLogsWindow()

	sw := w.sessions[logsWindowID]
	if sw == nil {
		t.Fatal("logs window not registered in w.sessions")
	}
	if !sw.readOnly {
		t.Error("sw.readOnly = false, want true (so it inherits layout-exclusion etc.)")
	}
	if sw.kind != winLogs {
		t.Errorf("sw.kind = %v, want winLogs", sw.kind)
	}
	if sw.window.Title != logsWindowTitle {
		t.Errorf("window title = %q, want %q (no \"(analysis)\" suffix)", sw.window.Title, logsWindowTitle)
	}
	// A logs window shows diagnostic lines, not a conversational "[System] ready"
	// banner. Every existing kind keeps that banner; winLogs must skip it.
	for _, r := range sw.transcript.records {
		if r.kind == kindSystem {
			t.Fatalf("logs window carries a [System] banner: %+v", r)
		}
	}
}

func TestShowLogsWindow_RaiseNotDuplicate(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	defer w.CloseSession(logsWindowID)

	w.showLogsWindow()
	first := w.sessions[logsWindowID]
	w.showLogsWindow() // reopen → must raise, not duplicate

	// Exactly one logs entry, same window object.
	count := 0
	for id := range w.sessions {
		if id == logsWindowID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("logs window duplicated: %d entries", count)
	}
	if w.sessions[logsWindowID] != first {
		t.Fatal("reopen created a different window object instead of raising")
	}
	// Reopening raised it to the top of the z-stack.
	if top := w.desktop.TopLayer(); top == nil || top != first.layer {
		t.Fatal("logs window not raised to the top on reopen")
	}
}

// Defect-A regression guard (issue #562 design round 2): log lines are populated
// through the transcript model, so a search/filter/fold re-render — which does
// view.Clear() then rebuilds from m.records — must NOT wipe them. A direct
// sw.history.AddColored would be erased here.
func TestAppendLogLine_SurvivesTranscriptRender(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	defer w.CloseSession(logsWindowID)
	w.showLogsWindow()
	sw := w.sessions[logsWindowID]

	w.appendLogLine(logLocal, time.Unix(1, 0), "INFO", "alpha line")
	w.appendLogLine(logLocal, time.Unix(2, 0), "ERROR", "beta line")

	if got := len(logsKindRecords(t, w)); got != 2 {
		t.Fatalf("kindLog records = %d, want 2", got)
	}
	view := sw.history.AllText()
	if !strings.Contains(view, "alpha line") || !strings.Contains(view, "beta line") {
		t.Fatalf("view missing appended lines: %q", view)
	}

	// A search re-renders from records. The matching line must survive; clearing
	// the search must restore the full set.
	sw.transcript.setQuery("beta")
	if got := sw.history.AllText(); !strings.Contains(got, "beta line") {
		t.Fatalf("beta line wiped by search re-render: %q", got)
	}
	sw.transcript.setQuery("")
	if got := sw.history.AllText(); !strings.Contains(got, "alpha line") || !strings.Contains(got, "beta line") {
		t.Fatalf("lines lost after clearing search: %q", got)
	}
}

func TestAppendLogLine_LevelColourMapping(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	defer w.CloseSession(logsWindowID)
	w.showLogsWindow()

	t0 := time.Unix(0, 0)
	w.appendLogLine(logLocal, t0, "INFO", "i")
	w.appendLogLine(logLocal, t0, "WARN", "wa")
	w.appendLogLine(logLocal, t0, "ERROR", "er")

	recs := logsKindRecords(t, w)
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3", len(recs))
	}
	want := []struct {
		role  colorRole
		color tui.Color // resolved via headerColor()
	}{
		{roleInfo, colorInfo},
		{roleWarn, colorTool}, // no dedicated warn colour; yellow
		{roleError, colorError},
	}
	for i, wc := range want {
		if recs[i].role != wc.role {
			t.Errorf("record %d role = %v, want %v", i, recs[i].role, wc.role)
		}
		if got := recs[i].headerColor(); got != wc.color {
			t.Errorf("record %d headerColor = %v, want %v", i, got, wc.color)
		}
	}
}

func TestRoleForLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]colorRole{
		"INFO":  roleInfo,
		"WARN":  roleWarn,
		"ERROR": roleError,
		"":      roleInfo, // unknown levels default to info
		"DEBUG": roleInfo, // not a wire level; defaults to info
	}
	for lvl, want := range cases {
		if got := roleForLevel(lvl); got != want {
			t.Errorf("roleForLevel(%q) = %v, want %v", lvl, got, want)
		}
	}
}

// Source tags appear only in remote mode (a daemon stream is wired). Embedded
// mode keeps the local view clean.
func TestAppendLogLine_SourceTagOnlyInRemoteMode(t *testing.T) {
	// Embedded mode: no daemon stream → no [local]/[daemon] tag.
	we := newTestWorkbench(t)
	we.SetLogRing(diag.NewRing(8))
	defer we.CloseSession(logsWindowID)
	we.showLogsWindow()
	we.appendLogLine(logLocal, time.Unix(0, 0), "INFO", "embedded-rec")
	if h := logsKindRecords(t, we)[0].header; strings.Contains(h, "[local]") || strings.Contains(h, "[daemon]") {
		t.Fatalf("embedded header has a source tag: %q", h)
	}

	// Remote mode: a daemon stream is wired → tags shown.
	wr := newTestWorkbench(t)
	wr.SetLogRing(diag.NewRing(8))
	defer wr.CloseSession(logsWindowID)
	wr.SetDaemonLogStream(func(context.Context, func(LogRecordDTO)) {}) // non-nil ⇒ remote
	wr.showLogsWindow()
	wr.appendLogLine(logLocal, time.Unix(0, 0), "INFO", "local-rec")
	wr.appendLogLine(logDaemon, time.Unix(0, 0), "INFO", "daemon-rec")
	recs := logsKindRecords(t, wr)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if !strings.Contains(recs[0].header, "[local]") {
		t.Errorf("local header missing [local] tag: %q", recs[0].header)
	}
	if !strings.Contains(recs[1].header, "[daemon]") {
		t.Errorf("daemon header missing [daemon] tag: %q", recs[1].header)
	}
}

func TestAppendLogLine_NoopWhenClosed(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	w.showLogsWindow()
	w.CloseSession(logsWindowID)

	// Must not panic and must not resurrect the window.
	w.appendLogLine(logLocal, time.Unix(0, 0), "INFO", "after-close")
	if w.sessions[logsWindowID] != nil {
		t.Fatal("appendLogLine re-opened the closed logs window")
	}
}

// The display cap reuses the transcript model's amortised trim(): records never
// exceed the limit, the newest are always kept and the oldest dropped.
func TestLogsWindow_CappedAtDisplayLimit(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	defer w.CloseSession(logsWindowID)
	w.showLogsWindow()
	sw := w.sessions[logsWindowID]
	if sw.transcript.limit != logsDisplayLimit {
		t.Fatalf("transcript limit = %d, want %d", sw.transcript.limit, logsDisplayLimit)
	}

	over := logsDisplayLimit + 150
	for i := 0; i < over; i++ {
		w.appendLogLine(logLocal, time.Unix(int64(i), 0), "INFO", fmt.Sprintf("line-%d", i))
	}

	if got := len(sw.transcript.records); got > logsDisplayLimit {
		t.Fatalf("records grew to %d, want <= %d (cap not enforced)", got, logsDisplayLimit)
	}
	view := sw.history.AllText()
	if !strings.Contains(view, fmt.Sprintf("line-%d", over-1)) {
		t.Fatal("newest log line was trimmed")
	}
	if strings.Contains(view, "line-0") {
		t.Fatal("oldest log line was not trimmed")
	}
}

// Focus target fix (issue #562 design round 2): an input-less logs window must
// focus its transcript on open, not leave focus on a nil input box.
func TestShowLogsWindow_FocusesHistory(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	defer w.CloseSession(logsWindowID)
	w.showLogsWindow()
	sw := w.sessions[logsWindowID]

	if sw.input != nil {
		t.Fatal("logs window should have no input box")
	}
	if !sw.history.Component.Focused() {
		t.Fatal("logs transcript not focused after open (keyboard scroll / q-Esc would be dead)")
	}
}

func TestCloseLogsWindow_CancelsSubscriptions(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetLogRing(diag.NewRing(8))
	w.showLogsWindow()
	if w.logs.cancel == nil {
		t.Fatal("logs cancel not set on open")
	}
	w.CloseSession(logsWindowID)
	if w.logs.cancel != nil {
		t.Fatal("logs cancel not cleared on close (goroutine/daemon stream leaked)")
	}
	if w.sessions[logsWindowID] != nil {
		t.Fatal("logs session not removed on close")
	}
}

// Reopening after close re-primes history from the ring, so the view shows recent
// logs again (acceptance: "reopening shows history again").
func TestLogsWindow_RepriResHistoryOnReopen(t *testing.T) {
	w := newTestWorkbench(t)
	ring := diag.NewRing(8)
	w.SetLogRing(ring)
	lg := diag.NewWithRing(io.Discard, ring)
	lg.Info("persisted-line-up")
	lg.Warn("persisted-lines-up")

	w.showLogsWindow()
	if got := len(logsKindRecords(t, w)); got != 2 {
		t.Fatalf("first open primed %d, want 2", got)
	}
	w.CloseSession(logsWindowID)

	// A record arrives while the window is closed; it sits in ring history.
	lg.Error("arrived-while-closed")

	w.showLogsWindow()
	recs := logsKindRecords(t, w)
	if len(recs) != 3 {
		t.Fatalf("reopen primed %d, want 3 (ring history refills the view)", len(recs))
	}
	view := w.sessions[logsWindowID].history.AllText()
	for _, want := range []string{"persisted-lines-up", "arrived-while-closed"} {
		if !strings.Contains(view, want) {
			t.Errorf("reopen view missing %q: %q", want, view)
		}
	}
	w.CloseSession(logsWindowID)
}

func TestParseLogTime(t *testing.T) {
	t.Parallel()
	got := parseLogTime("2026-06-28T12:34:56.123456789Z")
	want := time.Date(2026, 6, 28, 12, 34, 56, 123456789, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parseLogTime = %v, want %v", got, want)
	}
	// A malformed timestamp falls back to "now" (non-zero) rather than panicking.
	if fb := parseLogTime("not-a-time"); fb.IsZero() {
		t.Fatal("malformed timestamp fell back to zero time")
	}
}

// The menu item is "always available" — with neither a ring nor a daemon stream
// wired, opening must still succeed (empty window) and close cleanly, never panic.
func TestShowLogsWindow_OpensEmptyWhenFullyUnwired(t *testing.T) {
	w := newTestWorkbench(t)
	// No SetLogRing, no SetDaemonLogStream.
	w.showLogsWindow()

	sw := w.sessions[logsWindowID]
	if sw == nil {
		t.Fatal("window not opened when fully unwired")
	}
	if len(logsKindRecords(t, w)) != 0 {
		t.Fatalf("unwired window should be empty, got %+v", logsKindRecords(t, w))
	}
	// A nil ring must not panic when the live subscriber goroutine starts.
	w.CloseSession(logsWindowID)
	if w.sessions[logsWindowID] != nil {
		t.Fatal("unwired window not removed on close")
	}
}
