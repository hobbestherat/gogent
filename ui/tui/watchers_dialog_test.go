package ui

import (
	"fmt"
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// --- pure formatter / helper coverage --------------------------------------

// TestWatcherTarget covers the Target column / field that makes the
// attached-vs-free distinction explicit (issue #329): a free-running watcher
// reads "free", an attached one with a target reads its session id, and an
// attached one missing its target falls back to "(attached)".
func TestWatcherTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		info WatcherInfo
		want string
	}{
		{"free", WatcherInfo{Free: true, TargetSession: "ignored"}, "free"},
		{"attached with session", WatcherInfo{Free: false, TargetSession: "sess-7"}, "sess-7"},
		{"attached missing session", WatcherInfo{Free: false, TargetSession: ""}, "(attached)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := watcherTarget(tc.info); got != tc.want {
				t.Errorf("watcherTarget(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}
}

// TestLoadWatcherItemsNilGetter is the nil-handler guard: with no ListWatchers
// wired, loadWatcherItems yields no items (the dialog then shows its empty note).
func TestLoadWatcherItemsNilGetter(t *testing.T) {
	if got := loadWatcherItems(nil, "s"); got != nil {
		t.Errorf("loadWatcherItems(nil) = %v, want nil", got)
	}
}

// TestLoadWatcherItemsOrder pins the stable order the dialog renders: free-running
// watchers first, then by name, with the id as the final tie-break — so a status
// flip (which never changes name/kind/id) can never reshuffle the rows.
func TestLoadWatcherItemsOrder(t *testing.T) {
	in := []WatcherInfo{
		{ID: "z", Name: "zebra", Free: false, TargetSession: "s1"},
		{ID: "a2", Name: "alpha", Free: true},
		{ID: "a1", Name: "alpha", Free: true}, // same name as a2 -> id tie-break
		{ID: "m", Name: "mid", Free: false, TargetSession: "s1"},
		{ID: "f", Name: "beta", Free: true},
	}
	got := loadWatcherItems(func(string) []WatcherInfo { return in }, "s1")
	var gotIDs []string
	for _, w := range got {
		gotIDs = append(gotIDs, w.ID)
	}
	// free alpha(a1), free alpha(a2), free beta(f), attached mid(m), attached zebra(z)
	want := []string{"a1", "a2", "f", "m", "z"}
	if strings.Join(gotIDs, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", gotIDs, want)
	}
}

// TestLoadWatcherItemsPassesSessionID verifies the active session id is forwarded
// to the getter — the dialog must list the active session's attached watchers, not
// some other session's.
func TestLoadWatcherItemsPassesSessionID(t *testing.T) {
	var seen string
	loadWatcherItems(func(id string) []WatcherInfo { seen = id; return nil }, "sess-42")
	if seen != "sess-42" {
		t.Errorf("getter got session id %q, want %q", seen, "sess-42")
	}
}

// TestFormatWatcherRow covers a row's six columns: the live busy marker, the
// padded name, the schedule, the next-fire, the status token (with the /off
// suffix when disabled) and the Target column.
func TestFormatWatcherRow(t *testing.T) {
	// Free-running, running, enabled.
	row := formatWatcherRow(WatcherInfo{
		Name: "poller", Free: true, Enabled: true, Running: true,
		Status: "running", Schedule: "every 5m", NextFire: "12:05",
	})
	if !strings.HasPrefix(row, sessionStatusIcon(true)) {
		t.Errorf("running row should lead with the ● busy marker: %q", row)
	}
	for _, want := range []string{"every 5m", "next:12:05", "running", "[free]"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q missing %q", row, want)
		}
	}
	if !strings.Contains(row, padName("poller", watcherRowNameWidth)) {
		t.Errorf("row %q does not pad the name to %d cols", row, watcherRowNameWidth)
	}

	// Disabled attached watcher: status carries the /off suffix and the Target is
	// the owning session id; an idle watcher must NOT show the ● marker.
	off := formatWatcherRow(WatcherInfo{
		Name: "gh", Free: false, TargetSession: "sess-1", Enabled: false,
		Status: "idle", Schedule: "every 1m", NextFire: "13:00",
	})
	if strings.Contains(off, sessionStatusIcon(true)) {
		t.Errorf("idle watcher row should not show the ● marker: %q", off)
	}
	if !strings.Contains(off, "idle/off") {
		t.Errorf("disabled row %q should carry the /off status suffix", off)
	}
	if !strings.Contains(off, "[sess-1]") {
		t.Errorf("attached row %q should show the owning session in the Target column", off)
	}
}

// TestFormatWatcherRowEmptyFields covers the em-dash fallbacks: a watcher with no
// schedule and no next-fire still renders both columns (never a blank gap).
func TestFormatWatcherRowEmptyFields(t *testing.T) {
	row := formatWatcherRow(WatcherInfo{Name: "x", Free: true, Enabled: true, Status: "idle"})
	if !strings.Contains(row, "—") {
		t.Errorf("missing schedule/next should fall back to em-dash: %q", row)
	}
	if !strings.Contains(row, "next:—") {
		t.Errorf("missing next-fire should render next:— : %q", row)
	}
}

// TestFormatWatcherDetail covers the detail pane: metadata lines plus the full
// task text. The disabled marker, the optional last-run/result/error lines and
// the "(no task configured)" placeholder are all exercised.
func TestFormatWatcherDetail(t *testing.T) {
	full := formatWatcherDetail(WatcherInfo{
		Name: "poller", Free: false, TargetSession: "sess-9", Enabled: false,
		Status: "failed", Schedule: "every 5m", NextFire: "12:05",
		LastRun: "12:00", LastResult: "ok", LastError: "boom",
		Task: "do the thing\nover two lines",
	})
	for _, want := range []string{
		"Name: poller",
		"Target: sess-9",
		"Schedule: every 5m",
		"Status: failed (disabled)",
		"Next fire: 12:05",
		"Last run: 12:00",
		"Last result: ok",
		"Last error: boom",
		"Task:\ndo the thing\nover two lines",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("detail missing %q in:\n%s", want, full)
		}
	}

	// An enabled watcher with no task shows neither the disabled marker nor the
	// optional lines, and falls back to the no-task placeholder.
	bare := formatWatcherDetail(WatcherInfo{Name: "x", Free: true, Enabled: true, Status: "idle"})
	if strings.Contains(bare, "(disabled)") {
		t.Errorf("enabled watcher should not show the disabled marker:\n%s", bare)
	}
	if strings.Contains(bare, "Last run:") || strings.Contains(bare, "Last result:") || strings.Contains(bare, "Last error:") {
		t.Errorf("empty last-run fields should be omitted:\n%s", bare)
	}
	if !strings.Contains(bare, "(no task configured)") {
		t.Errorf("missing task should show the placeholder:\n%s", bare)
	}
	if !strings.Contains(bare, "Target: free") {
		t.Errorf("free watcher detail should report Target: free:\n%s", bare)
	}
}

// TestEmptyWatchersDetail covers the placeholder: the no-watchers invitation when
// the list is empty, and the "select one" hint when there are rows but none is
// highlighted.
func TestEmptyWatchersDetail(t *testing.T) {
	if got := emptyWatchersDetail(0); !strings.Contains(got, "No watchers") {
		t.Errorf("empty list should invite, got %q", got)
	}
	if got := emptyWatchersDetail(3); !strings.Contains(got, "Select a watcher") {
		t.Errorf("non-empty list should hint to select, got %q", got)
	}
}

// --- dialog open + footer-button dispatch ----------------------------------

// wiredWatcherWorkbench builds a workbench with a single window and a full set of
// recording watcher handlers, seeded with the given watchers. Each control records
// the id it was called with so a test can assert the footer buttons dispatch to the
// right handler with the selected watcher's id.
type watcherCalls struct {
	enable, disable, run, stop, delete string
}

func wiredWatcherWorkbench(t *testing.T, sessionID string, items ...WatcherInfo) (*Workbench, *watcherCalls) {
	t.Helper()
	w := newTestWorkbench(t)
	w.app.Resize(200, 50)
	w.openWindow(sessionID, "S")
	calls := &watcherCalls{}
	w.SetHandlers(Handlers{
		ListWatchers:   func(string) []WatcherInfo { return append([]WatcherInfo(nil), items...) },
		EnableWatcher:  func(id string) error { calls.enable = id; return nil },
		DisableWatcher: func(id string) error { calls.disable = id; return nil },
		RunWatcher:     func(id string) error { calls.run = id; return nil },
		StopWatcher:    func(id string) error { calls.stop = id; return nil },
		DeleteWatcher:  func(id string) error { calls.delete = id; return nil },
	})
	return w, calls
}

// pressDialogButton finds the footer button whose laid-out bounds equal want and
// drives a full press (down then up at its centre) through its OnClickFn — the
// exact path a real mouse click takes, so the button's OnPress (and the act
// closure behind it) runs.
func pressDialogButton(t *testing.T, w *Workbench, want tv.Rect) {
	t.Helper()
	for _, c := range dialogDescendants(w) {
		if c.Bounds != want || !c.DrawOutside || c.OnClickFn == nil {
			continue
		}
		abs := c.AbsoluteBounds()
		cx, cy := abs.X+abs.W/2, abs.Y+abs.H/2
		c.OnClickFn(c, tui.ClickEvent{X: cx, Y: cy, Down: true})
		c.OnClickFn(c, tui.ClickEvent{X: cx, Y: cy, Down: false})
		return
	}
	t.Fatalf("no footer button found at %+v", want)
}

// footerRectFor returns the laid-out rect of the footer button at index i, using
// the same footerButtonRects call the dialog makes.
func footerRectFor(w *Workbench, i int) tv.Rect {
	b := dialogBounds(w)
	return footerButtonRects(watchersFooterLabels, 2, b.W-3, b.H-3, tv.DefaultButtonGap)[i]
}

// TestWatchersDialogUnavailableWithoutHandler covers the guard: with no
// ListWatchers wired, the dialog must not open the two-pane browser — it shows a
// compact "unavailable" confirm instead.
func TestWatchersDialogUnavailableWithoutHandler(t *testing.T) {
	w := newTestWorkbench(t)
	w.app.Resize(200, 50)
	w.showWatchersDialog()
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		t.Fatal("expected a confirmation dialog, got none")
	}
	if b := top.Root.Bounds; b.W >= 60 {
		t.Errorf("a %dx%d dialog opened — the watchers browser should be hidden without ListWatchers", b.W, b.H)
	}
}

// TestWatchersDialogListsWatchers opens the dialog with a free-running and an
// attached watcher and checks both rows are present with the right Target column,
// and that the detail pane shows the selected (first) watcher's task text.
func TestWatchersDialogListsWatchers(t *testing.T) {
	free := WatcherInfo{ID: "w-free", Name: "emailer", Free: true, SessionID: "watcher:emailer",
		Enabled: true, Status: "idle", Schedule: "daily 07:00 UTC", Task: "send the digest"}
	att := WatcherInfo{ID: "w-att", Name: "gh", Free: false, TargetSession: "sess-1", SessionID: "sess-1",
		Enabled: true, Status: "idle", Schedule: "every 5m", Task: "poll issues"}
	w, _ := wiredWatcherWorkbench(t, "sess-1", free, att)
	w.showWatchersDialog()

	// The dialog must actually open as the two-pane browser (not the unavailable
	// confirm), at the content-driven size.
	if b := dialogBounds(w); b.W < 60 {
		t.Fatalf("watchers browser did not open (dialog %dx%d)", b.W, b.H)
	}

	// The list rows are rendered by formatWatcherRow over the same snapshot the
	// dialog builds; assert free-first ordering and both Target columns.
	items := loadWatcherItems(w.handlers.ListWatchers, w.ActiveID())
	if len(items) != 2 || !items[0].Free {
		t.Fatalf("expected free-first ordering, got %+v", items)
	}
	var rows []string
	for _, it := range items {
		rows = append(rows, formatWatcherRow(it))
	}
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "[free]") {
		t.Errorf("free-running watcher row missing [free] target:\n%s", joined)
	}
	if !strings.Contains(joined, "[sess-1]") {
		t.Errorf("attached watcher row missing [sess-1] target:\n%s", joined)
	}
	// The detail pane shows the selected (first = free) watcher's task.
	detailText := formatWatcherDetail(items[0])
	if !strings.Contains(detailText, "send the digest") {
		t.Errorf("detail pane should show the selected watcher's task:\n%s", detailText)
	}
}

// TestWatchersDialogFooterDispatch is the core behaviour test: pressing each
// footer control invokes the matching handler with the selected watcher's id.
// A single watcher makes the selection deterministic (index 0).
func TestWatchersDialogFooterDispatch(t *testing.T) {
	att := WatcherInfo{ID: "w-1", Name: "poll", Free: false, TargetSession: "sess-1",
		SessionID: "sess-1", Enabled: true, Status: "idle", Task: "poll"}
	w, calls := wiredWatcherWorkbench(t, "sess-1", att)
	w.showWatchersDialog()

	// Footer order matches watchersFooterLabels:
	// 0 Open Session, 1 Enable, 2 Disable, 3 Run, 4 Stop, 5 Delete, 6 Close.
	pressDialogButton(t, w, footerRectFor(w, 1))
	if calls.enable != "w-1" {
		t.Errorf("Enable button called EnableWatcher(%q), want w-1", calls.enable)
	}
	pressDialogButton(t, w, footerRectFor(w, 2))
	if calls.disable != "w-1" {
		t.Errorf("Disable button called DisableWatcher(%q), want w-1", calls.disable)
	}
	pressDialogButton(t, w, footerRectFor(w, 3))
	if calls.run != "w-1" {
		t.Errorf("Run button called RunWatcher(%q), want w-1", calls.run)
	}
	pressDialogButton(t, w, footerRectFor(w, 4))
	if calls.stop != "w-1" {
		t.Errorf("Stop button called StopWatcher(%q), want w-1", calls.stop)
	}
	pressDialogButton(t, w, footerRectFor(w, 5))
	if calls.delete != "w-1" {
		t.Errorf("Delete button called DeleteWatcher(%q), want w-1", calls.delete)
	}
}

// TestWatchersDialogOpenSessionFocusesTarget verifies the Open Session button
// raises the selected watcher's reporting session (its SessionID): Focus re-adds
// that session's layer to the top of the z-stack.
func TestWatchersDialogOpenSessionFocusesTarget(t *testing.T) {
	att := WatcherInfo{ID: "w-1", Name: "poll", Free: false, TargetSession: "sess-1",
		SessionID: "sess-1", Enabled: true, Status: "idle"}
	w, _ := wiredWatcherWorkbench(t, "sess-1", att)
	// Open a second window and raise it, so "sess-1" is not already on top.
	w.openWindow("sess-2", "S2")
	w.Focus("sess-2")
	w.showWatchersDialog()

	pressDialogButton(t, w, footerRectFor(w, 0)) // Open Session
	// Focus("sess-1") re-adds that session's layer above the dialog, so it is now the
	// top layer.
	if top := w.desktop.TopLayer(); top != w.sessions["sess-1"].layer {
		t.Errorf("Open Session did not raise sess-1 to the top layer")
	}
}

// TestWatchersDialogOpenSessionNoTargetIsNoop covers the guard: a watcher with an
// empty SessionID (nothing to open) must not crash or move focus.
func TestWatchersDialogOpenSessionNoTargetIsNoop(t *testing.T) {
	att := WatcherInfo{ID: "w-1", Name: "poll", Free: false, TargetSession: "sess-1",
		SessionID: "", Enabled: true, Status: "idle"}
	w, _ := wiredWatcherWorkbench(t, "sess-1", att)
	w.showWatchersDialog()
	dialogLayer := w.desktop.TopLayer()
	pressDialogButton(t, w, footerRectFor(w, 0)) // Open Session
	if top := w.desktop.TopLayer(); top != dialogLayer {
		t.Errorf("Open Session with empty SessionID changed the top layer")
	}
}

// TestWatchersFooterButtonRectsCleanAtPreferredWidth is the #321-style footer
// guard: at and above the dialog's preferred open width, every footer button is
// sized to its caption (never the old hardcoded widths), separated by exactly the
// gap, in-bounds and non-overlapping.
func TestWatchersFooterButtonRectsCleanAtPreferredWidth(t *testing.T) {
	const leftX, gap = 2, tv.DefaultButtonGap
	for _, width := range []int{104, 120, 130} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			rightX := width - 3
			rects := footerButtonRects(watchersFooterLabels, leftX, rightX, width-3, gap)
			assertFooterInvariants(t, watchersFooterLabels, rects, leftX, rightX, width-3, gap)
			for i, r := range rects {
				if min := tv.ButtonLabelWidth(watchersFooterLabels[i]); r.W < min {
					t.Errorf("button %q W=%d < ButtonLabelWidth %d (caption would clip)",
						watchersFooterLabels[i], r.W, min)
				}
			}
		})
	}
}

// TestWatchersDialogFooterCleanOnStandardTerminal opens the dialog the way a user
// would on the most common terminal sizes and asserts the footer lays out cleanly
// at the size the dialog actually resolved to — the same guarantee the Saved
// Sessions dialog makes in TestSessionsDialogFloorsOnTinyTerminal.
//
// This is the real-world version of the #321 footer guarantee: the seven watcher
// controls ("Open Session"/Enable/Disable/Run/Stop/Delete/Close) need ~89 columns
// of footer, but on an 80-column terminal the dialog's 80% width cap floors it to
// ~64 columns, so the buttons overlap. The dialog's content spec (MinW 60,
// PreferredW 104) is not wide enough for its own footer on a standard terminal.
func TestWatchersDialogFooterCleanOnStandardTerminal(t *testing.T) {
	for _, sz := range []struct{ w, h int }{
		{80, 24}, // the single most common terminal width
		{100, 30},
	} {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			att := WatcherInfo{ID: "w-1", Name: "poll", Free: false, TargetSession: "s", SessionID: "s", Enabled: true, Status: "idle"}
			w, _ := wiredWatcherWorkbench(t, "s", att)
			w.app.Resize(sz.w, sz.h)
			w.showWatchersDialog()
			b := dialogBounds(w)
			rects := footerButtonRects(watchersFooterLabels, 2, b.W-3, b.H-3, tv.DefaultButtonGap)
			// Buttons must not overlap at the size the dialog actually opened to.
			for i := 0; i < len(rects); i++ {
				for j := i + 1; j < len(rects); j++ {
					if rectsOverlap(rects[i], rects[j]) {
						t.Errorf("on a %dx%d terminal the dialog opened %d wide and footer buttons %q/%q overlap (%+v / %+v)",
							sz.w, sz.h, b.W, watchersFooterLabels[i], watchersFooterLabels[j], rects[i], rects[j])
					}
				}
			}
		})
	}
}

// TestWatchersDialogSizeIsContentDriven checks the dedicated watchersDialogSpec
// sizes to its content footprint and does not balloon to the browser percentage
// box on a roomy terminal.
func TestWatchersDialogSizeIsContentDriven(t *testing.T) {
	w := newTestWorkbench(t)
	spec := w.watchersDialogSpec()
	_, _, gw, gh := tv.ResolveDialogRect(spec, 200, 50)
	if gw != 104 || gh != 24 {
		t.Errorf("watchers size on 200x50 = %dx%d, want content-driven 104x24", gw, gh)
	}
	if gw >= 160 || gh >= 42 {
		t.Errorf("watchers dialog ballooned to the browser box (%dx%d)", gw, gh)
	}
	// Floors hold on a tiny terminal.
	_, _, fw, fh := tv.ResolveDialogRect(spec, 50, 10)
	if fw != 60 || fh != 16 {
		t.Errorf("watchers floor = %dx%d, want 60x16", fw, fh)
	}
}
