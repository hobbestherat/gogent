package ui

import (
	"fmt"
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// sessionsFooterLabels are the exact action-button labels showSessionsDialog hands
// footerButtonRects. Tests reuse them so a change to the source labels is reflected
// (and the assertions track the real widths).
var sessionsFooterLabels = []string{"&Open (analysis)", "&Continue", "Close"}

// wiredSessionsWorkbench returns a workbench with the two handlers showSessionsDialog
// requires (ListSavedSessions + OpenSavedSession), seeded with `n` sessions. The
// OpenSavedSession stub always reports failure — opening the dialog never calls it
// (it fires only on a button press), so the stub just satisfies the non-nil guard.
func wiredSessionsWorkbench(t *testing.T, n int) *Workbench {
	t.Helper()
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		ListSavedSessions: func() []SessionMeta {
			out := make([]SessionMeta, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, SessionMeta{
					ID:        "id",
					Title:     "Session",
					CreatedAt: "2026-06-19T12:00:00Z",
					Turns:     i + 1,
					Messages:  i + 2,
				})
			}
			return out
		},
		OpenSavedSession: func(string, bool) (RestoredSession, bool) { return RestoredSession{}, false },
	})
	return w
}

// dialogDescendants returns every VisualComponent under the top-most dialog window
// (depth-first), so a test can find the rects the dialog actually laid out.
func dialogDescendants(w *Workbench) []*tv.VisualComponent {
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		return nil
	}
	var out []*tv.VisualComponent
	var rec func(*tv.VisualComponent)
	rec = func(n *tv.VisualComponent) {
		for _, ch := range n.Children() {
			out = append(out, ch)
			rec(ch)
		}
	}
	rec(top.Root)
	return out
}

func containsRect(rects []tv.Rect, want tv.Rect) bool {
	for _, r := range rects {
		if r == want {
			return true
		}
	}
	return false
}

// --- #321: footer buttons sized to their labels, not clipped ----------------

// TestSessionsFooterButtonRectsNotClipped is the direct #321 regression guard at
// the pure-layout level: the three Saved Sessions buttons, laid out by the same
// footerButtonRects call showSessionsDialog makes, are each sized to the FULL width
// turbotui needs to draw their caption (tv.ButtonLabelWidth) — never the old
// hand-tuned widths that clipped every caption. The full footer invariants
// (in-bounds, right-aligned, exactly DefaultButtonGap apart, non-overlapping) are
// checked across the whole width range the sessions dialog can take, from its 60
// floor to its 120 cap.
func TestSessionsFooterButtonRectsNotClipped(t *testing.T) {
	const leftX, gap = 2, tv.DefaultButtonGap
	for _, width := range []int{60, 64, 90, 120} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			rightX := width - 3
			rects := footerButtonRects(sessionsFooterLabels, leftX, rightX, width-3, gap)
			assertFooterInvariants(t, sessionsFooterLabels, rects, leftX, rightX, width-3, gap)
			// Each face is at least as wide as its caption needs — the #321 symptom
			// was W < ButtonLabelWidth, which turbotui then ellipsises.
			for i, r := range rects {
				if min := tv.ButtonLabelWidth(sessionsFooterLabels[i]); r.W < min {
					t.Errorf("button %q W=%d < ButtonLabelWidth %d (caption would be clipped)",
						sessionsFooterLabels[i], r.W, min)
				}
			}
		})
	}
}

// TestSessionsFooterWiderThanOldHardcoded documents the actual defect #321 fixed:
// the old hand-tuned widths (17 / 10 / 7) were each strictly NARROWER than the
// caption needs, so every button clipped. The helper-driven widths are wide enough.
func TestSessionsFooterWiderThanOldHardcoded(t *testing.T) {
	oldHardcoded := map[string]int{
		"&Open (analysis)": 17,
		"&Continue":        10,
		"Close":            7,
	}
	for _, label := range sessionsFooterLabels {
		need := tv.ButtonLabelWidth(label)
		if old := oldHardcoded[label]; old >= need {
			t.Errorf("old hardcoded width %d for %q was not below the needed %d — #321 premise no longer holds",
				old, label, need)
		}
	}
}

// TestSessionsFooterFitsAtFloorWidth checks the footer never has to clamp at the
// dialog's MINIMUM width (60): the whole right-aligned group fits inside [2,57]
// with room to spare, so no button is pushed in or trimmed (clampDialogRect is a
// no-op here). This is the "correct at the small end" half of #321.
func TestSessionsFooterFitsAtFloorWidth(t *testing.T) {
	const width = 60 // sessionsDialogSpec MinW
	const leftX, gap = 2, tv.DefaultButtonGap
	rightX := width - 3
	rects := footerButtonRects(sessionsFooterLabels, leftX, rightX, 0, gap)

	// No rect was clamped: each keeps its natural ButtonLabelWidth and starts at or
	// after leftX with its right edge at or before rightX.
	for i, r := range rects {
		if r.W != tv.ButtonLabelWidth(sessionsFooterLabels[i]) {
			t.Errorf("at floor width %d, button %q clamped to W=%d (group does not fit)",
				width, sessionsFooterLabels[i], r.W)
		}
		if r.X < leftX || r.X+r.W-1 > rightX {
			t.Errorf("button %q rect %+v escapes [%d,%d]", sessionsFooterLabels[i], r, leftX, rightX)
		}
	}
	// And the leftmost button still has a clear left margin (group genuinely fits,
	// not merely touching the edge).
	if rects[0].X <= leftX {
		t.Errorf("group is flush against leftX at the floor width — no slack; first X=%d", rects[0].X)
	}
}

// --- #321/#322: integration — open the real dialog and inspect its layout ----

// TestSessionsDialogOpensContentSized opens the real dialog on a roomy terminal and
// asserts it resolved to its content footprint (104×26, #338 — widened from the
// initial #322 90×20 so a full list row fits) and is centered. The old shared
// browser spec would have produced the 80%×85% balloon instead.
func TestSessionsDialogOpensContentSized(t *testing.T) {
	const termW, termH = 200, 50
	w := wiredSessionsWorkbench(t, 3)
	w.app.Resize(termW, termH)
	w.showSessionsDialog()

	b := dialogBounds(w)
	if b.W != 104 || b.H != 26 {
		t.Errorf("opened size = %dx%d, want the content-driven 104x26 (#338)", b.W, b.H)
	}
	if b.W >= 160 || b.H >= 42 {
		t.Fatalf("dialog ballooned to %dx%d — the #322 balloon is back", b.W, b.H)
	}
	if b.X != (termW-b.W)/2 || b.Y != (termH-b.H)/2 {
		t.Errorf("dialog not centered: origin (%d,%d)", b.X, b.Y)
	}
}

// TestSessionsDialogFooterRendersFullWidth opens the real dialog and inspects the
// rendered button components (the three DrawOutside widgets): they must sit at the
// exact rects footerButtonRects produces for the resolved width — full
// ButtonLabelWidth, right-aligned, non-overlapping — proving #321 is fixed in the
// live layout, not just in the helper.
func TestSessionsDialogFooterRendersFullWidth(t *testing.T) {
	w := wiredSessionsWorkbench(t, 3)
	w.app.Resize(200, 50)
	w.showSessionsDialog()
	b := dialogBounds(w)

	wantFooter := footerButtonRects(sessionsFooterLabels, 2, b.W-3, b.H-3, tv.DefaultButtonGap)

	// The buttons are exactly the DrawOutside components (turbotui buttons draw their
	// shadow outside their bounds; labels/boxes do not).
	var buttonRects []tv.Rect
	for _, c := range dialogDescendants(w) {
		if c.DrawOutside {
			buttonRects = append(buttonRects, c.Bounds)
		}
	}
	if len(buttonRects) != len(wantFooter) {
		t.Fatalf("found %d button widgets, want %d", len(buttonRects), len(wantFooter))
	}
	for i, want := range wantFooter {
		if !containsRect(buttonRects, want) {
			t.Errorf("footer button %d (%q) not rendered at %+v; got %+v",
				i, sessionsFooterLabels[i], want, buttonRects)
		}
		// Non-clipped: the rendered face is at least the caption's needed width.
		if want.W < tv.ButtonLabelWidth(sessionsFooterLabels[i]) {
			t.Errorf("rendered button %q W=%d below ButtonLabelWidth", sessionsFooterLabels[i], want.W)
		}
	}
	// All three on one row, none overlapping.
	for i := 0; i < len(buttonRects); i++ {
		for j := i + 1; j < len(buttonRects); j++ {
			if rectsOverlap(buttonRects[i], buttonRects[j]) {
				t.Errorf("rendered buttons overlap: %+v and %+v", buttonRects[i], buttonRects[j])
			}
		}
	}
}

// TestSessionsDialogHintOnOwnRowAboveButtons is the second half of #321: the
// keyboard hint moved to its OWN row (height-4) ABOVE the button row (height-3), so
// it can never collide with the buttons. The test asserts the hint label spans the
// content width on row height-4, the buttons are on row height-3, and — crucially —
// NOTHING but the buttons sits on the button row (a regression that put the hint
// back on the button row would land another component there).
func TestSessionsDialogHintOnOwnRowAboveButtons(t *testing.T) {
	w := wiredSessionsWorkbench(t, 3)
	w.app.Resize(200, 50)
	w.showSessionsDialog()
	b := dialogBounds(w)

	hintY := b.H - 4
	buttonY := b.H - 3
	if hintY >= buttonY {
		t.Fatalf("hint row %d is not above button row %d", hintY, buttonY)
	}

	descendants := dialogDescendants(w)

	// The hint is a full-content-width label on its own row.
	wantHint := tv.Rect{X: 2, Y: hintY, W: b.W - 4, H: 1}
	var rects []tv.Rect
	for _, c := range descendants {
		rects = append(rects, c.Bounds)
	}
	if !containsRect(rects, wantHint) {
		t.Errorf("hint label not found at its own row %+v; rects=%+v", wantHint, rects)
	}

	// Nothing sits on the hint row except the hint itself (no button bled up into it).
	for _, c := range descendants {
		if c.Bounds.Y == hintY && c.Bounds != wantHint {
			t.Errorf("unexpected component shares the hint row %d: %+v", hintY, c.Bounds)
		}
	}

	// The button row carries ONLY the action buttons — the #321 collision was the
	// hint sharing this row. Every component on buttonY must be a DrawOutside button.
	wantFooter := footerButtonRects(sessionsFooterLabels, 2, b.W-3, buttonY, tv.DefaultButtonGap)
	for _, c := range descendants {
		if c.Bounds.Y != buttonY {
			continue
		}
		if !c.DrawOutside {
			t.Errorf("non-button component on the button row %d: %+v (hint/label must not share it)", buttonY, c.Bounds)
		}
		if !containsRect(wantFooter, c.Bounds) {
			t.Errorf("component on button row not at a footer rect: %+v (want one of %+v)", c.Bounds, wantFooter)
		}
	}

	// And the panes shrank by exactly the one row the hint took: list/detail end at
	// row height-5, leaving the hint (height-4) and button (height-3) rows clear.
	wantPaneBottom := b.H - 5
	var sawList bool
	for _, c := range descendants {
		// The list/detail panes are the two tall focusable, non-DrawOutside widgets.
		if c.Focusable && !c.DrawOutside && c.Bounds.H > 1 {
			sawList = true
			if bottom := c.Bounds.Y + c.Bounds.H; bottom > wantPaneBottom {
				t.Errorf("pane %+v extends past row %d into the footer (paneH not shrunk for the hint row)", c.Bounds, wantPaneBottom)
			}
		}
	}
	if !sawList {
		t.Error("did not find the list/detail panes to check their height")
	}
}

// TestSessionsDialogFloorsOnTinyTerminal opens the dialog on a sub-floor terminal
// and asserts it honours the 60×14 floor (never collapsing) with an on-screen
// origin, and that the footer STILL lays out cleanly at that floor width.
func TestSessionsDialogFloorsOnTinyTerminal(t *testing.T) {
	w := wiredSessionsWorkbench(t, 1)
	w.app.Resize(50, 16)
	w.showSessionsDialog()
	b := dialogBounds(w)

	if b.W < 60 || b.H < 14 {
		t.Errorf("floored size = %dx%d, want at least 60x14", b.W, b.H)
	}
	if b.X < 0 || b.Y < 0 {
		t.Errorf("origin = (%d,%d), want both >= 0", b.X, b.Y)
	}
	// Footer still clean at the floor.
	wantFooter := footerButtonRects(sessionsFooterLabels, 2, b.W-3, b.H-3, tv.DefaultButtonGap)
	assertFooterInvariants(t, sessionsFooterLabels, wantFooter, 2, b.W-3, b.H-3, tv.DefaultButtonGap)
}

// TestSessionsDialogFrameReResolvesOnResize is the #322 dialog.Fit behavior: the
// spec is static, so growing the terminal while the dialog is open re-resolves its
// FRAME to the size a fresh open on the new terminal would produce (path-
// independent) and re-centers it. (Inner widget positions are laid out once at open
// and are not re-flowed on a live resize — a pre-existing limitation shared by the
// other browsers, out of scope for #321/#322; this test pins only the frame.)
func TestSessionsDialogFrameReResolvesOnResize(t *testing.T) {
	resized := wiredSessionsWorkbench(t, 3)
	resized.app.Resize(80, 24)
	resized.showSessionsDialog()
	small := dialogBounds(resized)
	if small.W != 64 || small.H != 20 {
		t.Fatalf("opened on 80x24 = %dx%d, want 64x20", small.W, small.H)
	}

	resized.app.Resize(200, 50)
	grown := dialogBounds(resized)

	fresh := wiredSessionsWorkbench(t, 3)
	fresh.app.Resize(200, 50)
	fresh.showSessionsDialog()
	want := dialogBounds(fresh)

	if grown.W != want.W || grown.H != want.H {
		t.Errorf("after resize frame = %dx%d, want %dx%d (fresh open on the same terminal — Fit must be path-independent)",
			grown.W, grown.H, want.W, want.H)
	}
	if grown.X != want.X || grown.Y != want.Y {
		t.Errorf("after resize origin = (%d,%d), want re-centered (%d,%d)", grown.X, grown.Y, want.X, want.Y)
	}
	if grown.W <= small.W {
		t.Errorf("frame did not grow on resize: 80x24=%d -> 200x50=%d", small.W, grown.W)
	}
}

// TestSessionsDialogShrinksOnResize checks the re-resolution also SHRINKS the open
// dialog frame when the terminal gets smaller (the symmetric case).
func TestSessionsDialogShrinksOnResize(t *testing.T) {
	w := wiredSessionsWorkbench(t, 3)
	w.app.Resize(200, 50)
	w.showSessionsDialog()
	big := dialogBounds(w)

	w.app.Resize(80, 24)
	small := dialogBounds(w)

	if small.W >= big.W {
		t.Errorf("frame did not shrink: 200x50=%d -> 80x24=%d", big.W, small.W)
	}
	if small.W != 64 || small.H != 20 {
		t.Errorf("shrunk frame = %dx%d, want 64x20", small.W, small.H)
	}
}

// TestSessionsDialogUnavailableWithoutHandlers covers the error path: with no
// ListSavedSessions / OpenSavedSession wired, showSessionsDialog must NOT open the
// browser — it shows an "unavailable" confirmation instead.
func TestSessionsDialogUnavailableWithoutHandlers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		handlers Handlers
	}{
		{"both nil", Handlers{}},
		{"only list wired", Handlers{ListSavedSessions: func() []SessionMeta { return nil }}},
		{"only open wired", Handlers{OpenSavedSession: func(string, bool) (RestoredSession, bool) { return RestoredSession{}, false }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestWorkbench(t)
			w.app.Resize(200, 50)
			w.SetHandlers(tc.handlers)
			w.showSessionsDialog()

			top := w.desktop.TopLayer()
			if top == nil || top.Root == nil {
				t.Fatal("expected a confirmation dialog, got none")
			}
			// The browser is titled "Saved Sessions" and is a two-pane layout; the
			// unavailable path is a small confirm. Distinguish by size: the browser
			// would be >= 60 wide, the confirm is compact.
			if b := top.Root.Bounds; b.W >= 60 {
				t.Errorf("a %dx%d dialog opened — the sessions browser should be hidden without its handlers", b.W, b.H)
			}
		})
	}
}

// TestSessionsDialogSpecIsContentDrivenNotBrowserShare contrasts the dedicated
// sessions spec with the shared browserDialogSpec on the SAME roomy terminal: the
// sessions dialog is materially smaller in both axes (it sizes to content) while
// the browser fills the percentage box. This is the #322 before/after in one test.
func TestSessionsDialogSpecIsContentDrivenNotBrowserShare(t *testing.T) {
	const termW, termH = 200, 50
	w := newTestWorkbench(t)
	w.app.Resize(termW, termH)

	_, _, sw, sh := tv.ResolveDialogRect(w.sessionsDialogSpec(), termW, termH)
	_, _, bw, bh := tv.ResolveDialogRect(w.browserDialogSpec(), termW, termH)

	if sw >= bw || sh >= bh {
		t.Errorf("sessions %dx%d is not smaller than the shared browser %dx%d — the dedicated content spec is not in effect (#322)",
			sw, sh, bw, bh)
	}
	if bw != 160 || bh != 42 {
		t.Errorf("browser spec on 200x50 = %dx%d, want the 160x42 balloon (premise of the contrast)", bw, bh)
	}
}
