package ui

import (
	"strconv"
	"testing"
	"time"

	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/stats"
)

// These tests cover issue #521: the connect/restore/open hot path must request
// COALESCED redraws (RequestRedraw) instead of synchronous ones (Redraw), and the
// expensive Overall recompute (refreshOverall -> GetStatistics + lifetime fold)
// must be coalesced during a burst rather than run once per window. They also pin
// the inverse contract: paths that must paint within the same turn (a user-driven
// raise) keep their synchronous Redraw, and the cheap focus/TODO tracking is not
// dropped when the expensive Overall recompute is deferred.
//
// All tests are headless: newTestWorkbench builds a Workbench (desktop + sidebar)
// without starting the event loop, and App.Apply is a no-op until Run. In that
// setting a synchronous Redraw() still runs compose() (so it is observable),
// while a coalesced RequestRedraw() only sets the dirty flag and composes NOTHING
// (there is no flushDirty() without a running loop). That asymmetry is exactly
// what these tests exploit to count real composes without touching the app writer
// or faking the event loop. The spy mirrors the flashSpy pattern in
// close_session_flash_test.go.

// composeSpy is a passive bottom desktop layer whose DrawFn fires exactly once
// per compose() — i.e. once per synchronous frame paint (an AddLayer or an
// explicit Redraw). A coalesced RequestRedraw() ticks it zero times, because
// without a running event loop nothing turns the dirty flag into a compose. It
// does not accept input, so it never claims focus or the top spot.
type composeSpy struct {
	composes int
}

// installComposeSpy adds a recording layer to the bottom of w's desktop and
// returns the spy. The AddLayer itself composes once (so the spy records 1 on
// return); callers reset() before the operation under test.
func installComposeSpy(w *Workbench) *composeSpy {
	sp := &composeSpy{}
	comp := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: w.app.Width(), H: w.app.Height()})
	comp.DrawFn = func(*tv.VisualComponent, tv.Surface) { sp.composes++ }
	// FullScreen so compose restretches (and therefore always draws) the spy every
	// frame even if its bounds were empty; AcceptInput=false so it never claims
	// focus or the top spot while any session remains.
	w.desktop.AddLayer(tv.NewLayer("compose-spy", comp, false, true))
	return sp
}

func (sp *composeSpy) reset() { sp.composes = 0 }

// stopStatsTimer stops the coalesced Overall-refresh timer so its AfterFunc
// cannot fire (and Post) after the test ends. The Post is benign on a stopped
// desktop, but stopping keeps the test tidy and avoids cross-test timer noise.
func stopStatsTimer(t *testing.T, w *Workbench) {
	t.Helper()
	t.Cleanup(func() {
		if w.statsRefresh != nil {
			w.statsRefresh.Stop()
		}
	})
}

// TestRebuildMenuRequestsCoalescedRedraw pins change (A): rebuildMenu, which
// runs once per restored window on the connect/restore hot path
// (AdoptSession/OpenAnalysisSession both call it), must no longer end in a
// synchronous Redraw(). It rebuilds only the menu-bar model and never precedes a
// blocking read, so deferring to RequestRedraw is correct. With the fix a
// rebuildMenu in isolation composes zero frames.
func TestRebuildMenuRequestsCoalescedRedraw(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installComposeSpy(w)
	w.openWindow("a", "A") // give the menu a session so it has real content
	sp.reset()

	w.rebuildMenu()

	if sp.composes != 0 {
		t.Errorf("rebuildMenu composed %d frame(s), want 0 (must RequestRedraw, not Redraw; issue #521)", sp.composes)
	}

	// Spy sanity: a direct synchronous Redraw must tick exactly once, so the 0
	// above is a real "deferred" result and not a broken/insensitive spy. This is
	// also the load-bearing fact every other test in this file relies on.
	w.desktop.Redraw()
	if sp.composes != 1 {
		t.Errorf("direct Redraw composed %d frame(s), want 1 (spy must observe synchronous paints)", sp.composes)
	}
}

// TestConnectBurstCoalescesRedraws is the headline repaint-coalescing test for
// the connect/restore hot path. Restoring/opening N windows must compose ~N frames
// (the one unavoidable synchronous compose per AddLayer — structural turbotui
// behaviour we cannot and do not change), not the ~2N it cost before #521 where
// each AdoptSession additionally paid a synchronous rebuildMenu Redraw. restore
// (#519 batched addAll), model-preselect and rebuildEffortOptions redraw
// nothing, so per window the ONLY synchronous compose is AddLayer.
func TestConnectBurstCoalescesRedraws(t *testing.T) {
	w := newTestWorkbench(t)
	sp := installComposeSpy(w)
	sp.reset()

	const n = 6
	for i := 0; i < n; i++ {
		id := "s" + strconv.Itoa(i)
		w.AdoptSession(RestoredSession{ID: id, Title: id})
	}

	// Exactly one compose per window (AddLayer). If rebuildMenu regressed to a
	// synchronous Redraw this would be 2n; the equality also catches any other
	// stray synchronous paint added to the open path.
	if sp.composes != n {
		t.Errorf("connect/restore burst of %d windows composed %d frame(s), want %d (one per AddLayer; rebuildMenu must not compose synchronously, issue #521)", n, sp.composes, n)
	}
	if got := len(w.orderIDs()); got != n {
		t.Errorf("post-burst window count = %d, want %d (the windows themselves must still open)", got, n)
	}
}

// TestOverallFocusPreservedWithoutStatsHandler is the critical regression guard
// for change (C)'s split. refreshOverall runs sidebar.focusSession BEFORE its
// GetStatistics==nil guard, so the TODO region follows focus even with no
// statistics handler; scheduleOverallRefresh returns AT that guard (a no-op when
// GetStatistics is nil). A blind refreshOverall->scheduleOverallRefresh swap
// would therefore drop the focus/TODO highlight on window-open when no stats
// handler is wired. The fix keeps focusSession inline, which this test pins:
// with no statistics handler at all, the sidebar highlight must still land on
// every opened window.
func TestOverallFocusPreservedWithoutStatsHandler(t *testing.T) {
	w := newTestWorkbench(t)
	if w.handlers.GetStatistics != nil {
		t.Fatal("test precondition: newTestWorkbench must ship without a GetStatistics handler")
	}

	w.openWindow("a", "A")
	if w.sidebar.focused != "a" {
		t.Errorf("sidebar.focused = %q, want a (focusSession must run on window-open even with no statistics handler)", w.sidebar.focused)
	}
	w.openWindow("b", "B")
	if w.sidebar.focused != "b" {
		t.Errorf("sidebar.focused = %q, want b (focus must follow each newly opened window, not just the first)", w.sidebar.focused)
	}
}

// TestScheduleOverallRefreshDefersAndDoesNotCompose pins the coalescer itself:
// scheduleOverallRefresh arms (or resets) the single coalesced timer and composes
// nothing and fetches no statistics — it neither paints synchronously nor runs
// the recompute. Calling it repeatedly (as a burst of window-opens does) must not
// fetch statistics per call and must not compose.
func TestScheduleOverallRefreshDefersAndDoesNotCompose(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	calls := 0
	w.handlers.GetStatistics = func() stats.Report {
		calls++
		return stats.Report{}
	}
	sp := installComposeSpy(w)
	sp.reset()

	for i := 0; i < 5; i++ {
		w.scheduleOverallRefresh()
	}

	if calls != 0 {
		t.Errorf("scheduleOverallRefresh fetched statistics %d time(s), want 0 (it only arms the timer)", calls)
	}
	if sp.composes != 0 {
		t.Errorf("scheduleOverallRefresh composed %d frame(s), want 0 (it defers, never paints)", sp.composes)
	}
	if w.statsRefresh == nil {
		t.Error("scheduleOverallRefresh left w.statsRefresh nil; the coalesced timer was not armed")
	}
}

// TestOverallCountDeferredUntilCoalescedRefresh covers the deferral end-to-end.
// With the recompute fully deferred on the open path (no synchronous
// refreshOverall during a burst), the Overall count must NOT be recomputed per
// window: it stays at its pre-burst value synchronously, then ONE coalesced
// refresh fetches statistics exactly once and reflects every restored window.
// This is the "final frame after a burst must be correct" edge case from #521,
// plus the "one recompute per burst, not per window" coalescing contract.
func TestOverallCountDeferredUntilCoalescedRefresh(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	calls := 0
	w.handlers.GetStatistics = func() stats.Report {
		calls++
		return stats.Report{}
	}

	const n = 4
	for i := 0; i < n; i++ {
		id := "s" + strconv.Itoa(i)
		w.AdoptSession(RestoredSession{ID: id, Title: id})
	}

	// Nothing recomputed synchronously: zero stats fetches and the count stays 0
	// (its pre-burst value) — the recompute is deferred, not run per window.
	if calls != 0 {
		t.Errorf("burst fetched GetStatistics %d time(s) synchronously; want 0 (recompute must be deferred)", calls)
	}
	if got := w.sidebar.overall.Sessions; got != 0 {
		t.Errorf("Overall session count synchronously after burst = %d, want 0 (the recompute must not run per window)", got)
	}
	// The coalesced terminus (the 250ms AfterFunc Posts refreshOverall; no loop
	// here, so invoke the same call) fetches exactly once and yields the correct
	// final count.
	w.refreshOverall()
	if calls != 1 {
		t.Errorf("coalesced refresh fetched GetStatistics %d time(s); want 1 (one recompute per burst)", calls)
	}
	if got := w.sidebar.overall.Sessions; got != n {
		t.Errorf("Overall session count after coalesced refresh = %d, want %d (final frame must reflect every restored window)", got, n)
	}
}

// TestFocusKeepsSynchronousRedraw is the over-conversion guard. Focus is a
// discrete user-driven raise (Ctrl+] / window click) that must paint the raised
// window synchronously within the same turn, so it must KEEP its trailing
// Redraw(). The test is robust to how many composes the toolkit's RemoveLayer /
// AddLayer emit: it measures the bare raise (RemoveLayer + AddLayer, i.e. Focus
// MINUS its trailing Redraw) as a baseline, then asserts full Focus composes
// strictly MORE. If a future blanket-convert dropped Focus's Redraw in favour of
// RequestRedraw, Focus would equal the baseline and this assertion would fail.
func TestFocusKeepsSynchronousRedraw(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	sw := w.sessions["a"]
	sp := installComposeSpy(w)

	// Baseline: the bare z-stack raise Focus performs, without its trailing
	// synchronous Redraw (RemoveLayer redraws nothing; AddLayer composes once).
	sp.reset()
	w.desktop.RemoveLayer(sw.layer)
	w.desktop.AddLayer(sw.layer)
	baseline := sp.composes
	if baseline == 0 {
		t.Fatal("setup invariant failed: bare raise composed 0 frames — AddLayer must compose, else the guard is vacuous")
	}

	// Full Focus = the same raise + refreshOverall (no compose) + synchronous
	// Redraw. It must compose strictly more than the bare raise.
	sp.reset()
	w.Focus("a")
	if sp.composes <= baseline {
		t.Errorf("Focus composed %d frame(s) (bare-raise baseline %d); want strictly more — Focus must keep its synchronous Redraw (issue #521 over-conversion guard)", sp.composes, baseline)
	}
	if w.ActiveID() != "a" {
		t.Errorf("post-Focus ActiveID = %q, want a", w.ActiveID())
	}
}

// TestTickBusyStatusesRequestsCoalescedRedraw pins the change-(B) conversions in
// tickBusyStatuses (the live-status ticker). It runs inside desktop.Post, which
// already requests a coalesced redraw when the callback returns, so the two
// trailing Redraw() calls (the marker-transition redraw and the busy-tick
// Overall redraw) were redundant and are now RequestRedraws. With a busy session
// both converted sites execute, yet — with no event loop — a tick composes zero
// frames.
func TestTickBusyStatusesRequestsCoalescedRedraw(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	w.handlers.GetStatistics = func() stats.Report { return stats.Report{} }
	w.openWindow("a", "A")
	// Busy so the active-refresh path runs (refreshOverall + the tail redraw); on
	// this first tick syncBusy also sees a new busy marker, exercising the
	// marker-transition redraw path too.
	w.sessions["a"].busy = true

	sp := installComposeSpy(w)
	sp.reset()
	w.tickBusyStatuses()

	if sp.composes != 0 {
		t.Errorf("tickBusyStatuses composed %d frame(s), want 0 (both internal redraws are coalesced RequestRedraws; the enclosing Post already repaints, issue #521)", sp.composes)
	}

	// Spy still detects a real synchronous paint, so the 0 is meaningful.
	w.desktop.Redraw()
	if sp.composes != 1 {
		t.Errorf("direct Redraw after tick composed %d frame(s), want 1 (spy sanity)", sp.composes)
	}
}

// TestTickBusyStatusesIdleDrawsNothing documents that the conversion did not
// accidentally make an idle workbench compose: with no busy session and no
// marker/fold/watcher transition, tickBusyStatuses must still compose zero
// frames (the function's own redraw guards are false, and the enclosing Post's
// RequestRedraw composes nothing without a loop).
func TestTickBusyStatusesIdleDrawsNothing(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	w.openWindow("a", "A")

	sp := installComposeSpy(w)
	sp.reset()
	w.tickBusyStatuses()

	if sp.composes != 0 {
		t.Errorf("idle tickBusyStatuses composed %d frame(s), want 0 (idle workbench must not repaint)", sp.composes)
	}
}

// TestOverallRefreshCoalesceWindow pins the documented 250ms Overall-refresh
// coalesce window (issue #53 / #521). Silently changing it would alter the
// burst-coalescing latency the fix relies on and that openWindowAny now routes
// window-open through.
func TestOverallRefreshCoalesceWindow(t *testing.T) {
	if overallRefreshCoalesce != 250*time.Millisecond {
		t.Errorf("overallRefreshCoalesce = %v, want 250ms", overallRefreshCoalesce)
	}
}

// ---------------------------------------------------------------------------
// REGRESSION GUARDS for the OnActiveLayerChange hook (NewWorkbench). An earlier
// revision of the #521 fix converted openWindowAny's DIRECT refreshOverall to the
// coalescer but missed this hook, which AddLayer fires synchronously — so a
// connect/restore burst still fetched GetStatistics once per window. The hook now
// keeps only the cheap focusSession immediate and defers the recompute via
// scheduleOverallRefresh (mirroring openWindowAny). The tests below pin that.
// ---------------------------------------------------------------------------

// TestActiveLayerHookDefersOverallRecompute verifies the corrected hook: opening
// a window fires the OnActiveLayerChange hook synchronously (AddLayer ->
// notifyActiveLayerChange, turbotui desktop.go:289), and the hook must DEFER the
// expensive Overall recompute (GetStatistics + fold) to the 250ms coalescer —
// fetching ZERO statistics synchronously — while still landing the cheap sidebar
// focus highlight on the new window immediately.
func TestActiveLayerHookDefersOverallRecompute(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	calls := 0
	w.handlers.GetStatistics = func() stats.Report {
		calls++
		return stats.Report{}
	}

	w.openWindow("a", "A")

	if calls != 0 {
		t.Errorf("openWindow fetched GetStatistics %d time(s) synchronously; want 0 (OnActiveLayerChange hook must defer the recompute to the coalescer, issue #521)", calls)
	}
	if w.sidebar.focused != "a" {
		t.Errorf("sidebar.focused = %q, want a (the hook's cheap focusSession must still run immediately so the highlight tracks the opened window)", w.sidebar.focused)
	}
	if w.statsRefresh == nil {
		t.Error("the coalesced Overall refresh timer was not armed (the hook must defer via scheduleOverallRefresh)")
	}
}

// TestActiveLayerHookSkipsUnchangedActiveSession covers the hook's dedup guard,
// which existed before #521 and must survive the fix. Opening a non-session layer
// (a dialog/overlay) above the active window changes TopLayer but NOT ActiveID
// (activeIDLocked falls back to the w.order tail, the session beneath), so
// ActiveID == sidebar.focused and the hook must skip — firing no Overall
// recompute and no GetStatistics fetch. This is what keeps a dialog open from
// triggering a redundant stats fetch.
func TestActiveLayerHookSkipsUnchangedActiveSession(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	calls := 0
	w.handlers.GetStatistics = func() stats.Report {
		calls++
		return stats.Report{}
	}
	w.openWindow("a", "A")
	if w.ActiveID() != "a" || w.sidebar.focused != "a" {
		t.Fatalf("setup: ActiveID=%q focused=%q, want a/a", w.ActiveID(), w.sidebar.focused)
	}
	calls = 0

	// A non-session overlay on top: TopLayer becomes it, but ActiveID still
	// resolves to the session beneath (the w.order tail).
	comp := tv.NewComponent(tv.Rect{X: 10, Y: 5, W: 40, H: 10})
	w.desktop.AddLayer(tv.NewLayer("overlay", comp, false, false))

	if w.ActiveID() != "a" {
		t.Fatalf("precondition: overlay on top changed ActiveID to %q, want a (test premise — activeIDLocked must skip non-session layers)", w.ActiveID())
	}
	if calls != 0 {
		t.Errorf("a non-session overlay over the active session fetched GetStatistics %d time(s); want 0 (hook guard must dedupe an unchanged ActiveID)", calls)
	}
}

// TestConnectBurstCoalescesOverallRecompute is the regression guard for the #521
// coalescing contract: GetStatistics (a backend round-trip) plus the lifetime fold
// must run at most once for a burst of N window-opens, never once per window. It
// passed only after the OnActiveLayerChange hook was converted to defer via
// scheduleOverallRefresh (mirroring openWindowAny); a regression that re-introduces
// a synchronous refreshOverall on the open path would fetch N times and fail this.
func TestConnectBurstCoalescesOverallRecompute(t *testing.T) {
	w := newTestWorkbench(t)
	stopStatsTimer(t, w)
	calls := 0
	w.handlers.GetStatistics = func() stats.Report {
		calls++
		return stats.Report{}
	}

	const n = 5
	for i := 0; i < n; i++ {
		id := "s" + strconv.Itoa(i)
		w.AdoptSession(RestoredSession{ID: id, Title: id})
	}

	if calls > 1 {
		t.Errorf("connect/restore burst of %d windows fetched GetStatistics %d time(s); want coalesced (<=1, ideally 0 deferred). "+
			"The NewWorkbench OnActiveLayerChange hook still calls refreshOverall synchronously per AddLayer — issue #521's Overall-recompute coalescing is not achieved.",
			n, calls)
	}
}
