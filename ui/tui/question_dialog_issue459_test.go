package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This suite exercises the gogent #459 fix: the ask_user question dialog must render
// its action buttons, scroll overflowing topic content (interactive widgets, not just
// text), reveal a required field that sits below the scroll fold, reflow on terminal
// resize, and keep every topic reachable when the tab strip overflows.
//
// It drives the real desktop input paths (type handlers for keys, scroll handlers for
// the wheel, App.Resize for the resize hook) and reads back the rendered cell grid, so
// it observes the same draw/clip/layout pipeline a user does — including the
// content-relative row math that clipped the buttons before the fix.

// q459OverflowRequest builds a single-topic request of n plain text fields whose
// labels (Q01..Qn) are short, distinctive and easy to search for in the rendered
// grid. Each item costs 3 logical rows (label + box + spacer), so at the small
// terminal sizes these tests use the topic overflows the panel and scrolls.
func q459OverflowRequest(n int) agent.QuestionRequest {
	items := make([]agent.QuestionItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, agent.QuestionItem{
			ID:    fmt.Sprintf("f%02d", i),
			Label: fmt.Sprintf("Q%02d", i+1),
			Type:  agent.QuestionText,
		})
	}
	return agent.QuestionRequest{
		Title:  "Overflow",
		Topics: []agent.QuestionTopic{{Title: "Many", Items: items}},
	}
}

// q459Dispatch sends a key through the app's real type handlers (the desktop's
// handleType), so capture/bubble/focus navigation all run as in production.
func q459Dispatch(t *testing.T, app *tui.App, ev tui.TypeEvent) {
	t.Helper()
	issue406Dispatch(t, app, ev)
}

// q459Scroll dispatches a mouse-wheel event through the app's real scroll handlers
// (the desktop's handleScroll → hit-test → BubbleScroll). X/Y must land inside the
// target widget for the event to reach it.
func q459Scroll(t *testing.T, app *tui.App, ev tui.ScrollEvent) {
	t.Helper()
	handlers := append([]func(tui.ScrollEvent){}, *exportedField[[]func(tui.ScrollEvent)](t, app, "scrollHandlers")...)
	if len(handlers) == 0 {
		t.Fatal("app has no scroll handlers; desktop scroll dispatch is not wired")
	}
	for _, h := range handlers {
		h(ev)
	}
}

// q459Screen renders the whole cell grid to a newline-separated string (reusing the
// issue406 helper) so tests can substring-search for labels and chrome.
func q459Screen(app *tui.App) string {
	return issue406ScreenText(app)
}

// q459RowOf returns the screen row (y) containing s, or -1. Used to assert a label or
// button lands on the expected row rather than just somewhere on screen.
func q459RowOf(t *testing.T, app *tui.App, s string) int {
	t.Helper()
	for y := 0; y < app.Height(); y++ {
		var b strings.Builder
		for x := 0; x < app.Width(); x++ {
			ch := app.ReadCell(x, y).Ch
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		if strings.Contains(b.String(), s) {
			return y
		}
	}
	return -1
}

// q459HasScrollbar reports whether the topic's vertical scrollbar glyph renders
// anywhere on screen. drawDialogVScrollbar paints the ▲/▼ caps (unique to a scroll
// bar — the dialog border is box-drawing and there are no title buttons), so their
// presence is a faithful "overflow + bar drawn" signal.
func q459HasScrollbar(app *tui.App) bool {
	for y := 0; y < app.Height(); y++ {
		for x := 0; x < app.Width(); x++ {
			switch app.ReadCell(x, y).Ch {
			case '▲', '▼':
				return true
			}
		}
	}
	return false
}

// q459DialogBounds returns the on-screen rect of the open question dialog.
func q459DialogBounds(t *testing.T, desktop *tv.Desktop) tv.Rect {
	t.Helper()
	top := desktop.TopLayer()
	if top == nil {
		t.Fatal("no question-dialog layer open")
	}
	return top.Root.Bounds
}

// ---------------------------------------------------------------------------
// Criterion 1 — buttons render on the last interior row (the primary defect).
// ---------------------------------------------------------------------------

// TestQ459ButtonsRenderOnLastInteriorRow is the primary regression: before the fix
// btnY was height-2, landing Cancel/Submit on the bottom border where turbotui's
// draw-time clip dropped them, so NO button text rendered. They must now appear, on
// the row one above the bottom border (height-3, content-relative).
func TestQ459ButtonsRenderOnLastInteriorRow(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(2), func(agent.QuestionResponse) {})
	desktop.Redraw()
	screen := q459Screen(app)

	for _, want := range []string{"Cancel", "Submit"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("button %q did not render — primary #459 defect (clipped onto the bottom border):\n%s", want, screen)
		}
	}

	b := q459DialogBounds(t, desktop)
	borderRow := b.Y + b.H - 1 // window row H-1: the bottom border
	submitRow := q459RowOf(t, app, "Submit")
	if submitRow == -1 {
		t.Fatalf("Submit not found on screen")
	}
	if submitRow == borderRow {
		t.Fatalf("Submit rendered on the bottom-border row %d (would be clipped) — want the row above it:\n%s", borderRow, screen)
	}
	// Submit should sit on the last visible interior row: content-relative height-3
	// == window row (dialogY + dialogH - 2).
	if want := b.Y + b.H - 2; submitRow != want {
		t.Fatalf("Submit on row %d, want last interior row %d:\n%s", submitRow, want, screen)
	}
}

// TestQ459ButtonsReachableKeyboard submits via Ctrl+Enter and cancels via Escape to
// prove the (now visible) buttons' key bindings still fire end-to-end.
func TestQ459ButtonsReachableKeyboard(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	res := make(chan agent.QuestionResponse, 2)
	showQuestionDialog(desktop, q459OverflowRequest(2), func(r agent.QuestionResponse) { res <- r })
	desktop.Redraw()

	// Ctrl+Enter submits (answers present because fields are optional/blank → omitted).
	issue406SubmitViaDialogRoot(t, desktop)
	r := <-res
	if r.Cancelled {
		t.Fatalf("Ctrl+Enter produced Cancelled=true, want a submit")
	}

	// Reopen and Escape cancels.
	showQuestionDialog(desktop, q459OverflowRequest(2), func(rr agent.QuestionResponse) { res <- rr })
	desktop.Redraw()
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyEscape})
	r = <-res
	if !r.Cancelled {
		t.Fatalf("Escape produced Cancelled=false, want a cancel")
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 — scrollable interactive topic content.
// ---------------------------------------------------------------------------

// q459SmallTerminal opens an overflowing single-topic dialog on a 60×16 terminal and
// returns it already redrawn. At this size the panel shows ~7 logical rows, so a
// 6-item (18-row) topic overflows and scrolls.
func q459SmallTerminal(t *testing.T) (*tui.App, *tv.Desktop, agent.QuestionRequest) {
	t.Helper()
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	req := q459OverflowRequest(6)
	showQuestionDialog(desktop, req, func(agent.QuestionResponse) {})
	desktop.Redraw()
	// Precondition: the topic genuinely overflows — the first label is visible and
	// the last is not. If this fails, the resolved geometry changed and the scroll
	// assertions below are no longer meaningful.
	if !strings.Contains(q459Screen(app), "Q01") {
		t.Fatal("precondition failed: Q01 not visible at scroll top — geometry changed")
	}
	if strings.Contains(q459Screen(app), "Q06") {
		t.Fatal("precondition failed: Q06 already visible at scroll top — topic does not overflow here; use more items or a smaller terminal")
	}
	return app, desktop, req
}

// TestQ459PageDownRevealsHiddenField proves scrolling works for interactive widgets:
// PageDown moves a below-the-fold TextBox into view and scrolls the top one away.
// (The scrollbar *glyph* itself is asserted separately — see TestQ459ScrollbarRenders
// — so this test isolates the scroll mechanics, which work.)
func TestQ459PageDownRevealsHiddenField(t *testing.T) {
	app, desktop, _ := q459SmallTerminal(t)

	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	desktop.Redraw()
	screen := q459Screen(app)

	if strings.Contains(screen, "Q01") {
		t.Fatalf("PageDown did not scroll Q01 off the top:\n%s", screen)
	}
	if !strings.Contains(screen, "Q04") {
		t.Fatalf("PageDown did not reveal a below-the-fold field (Q04):\n%s", screen)
	}
}

// TestQ459ArrowKeysLineScroll proves plain Up/Down scroll one row at a time when the
// focused TextBox declines them. A single Down should scroll the top label (Q01) off.
func TestQ459ArrowKeysLineScroll(t *testing.T) {
	app, desktop, _ := q459SmallTerminal(t)

	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyDown})
	desktop.Redraw()
	if strings.Contains(q459Screen(app), "Q01") {
		t.Fatalf("Down arrow did not line-scroll (Q01 should have left the top):\n%s", q459Screen(app))
	}

	// Scrolling all the way down reveals the last field (criterion 2: no field is
	// hidden due to overflow).
	for i := 0; i < 30; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyDown})
	}
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "Q06") {
		t.Fatalf("last field Q06 is unreachable by scrolling down (clamping broke):\n%s", q459Screen(app))
	}
}

// TestQ459ScrollClampsAndReverses: scrolling far past the bottom clamps (no negative
// offset, last field stays put, no panic), and PageUp returns to the top.
func TestQ459ScrollClampsAndReverses(t *testing.T) {
	app, desktop, _ := q459SmallTerminal(t)

	for i := 0; i < 40; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "Q06") {
		t.Fatalf("over-scroll did not leave the last field visible:\n%s", q459Screen(app))
	}

	// PageUp pages back up by visibleRows, so it takes a few to reach the top. The
	// point is the scroll is reversible (no clamped-forever offset): page up until
	// the top field returns.
	for i := 0; i < 10 && !strings.Contains(q459Screen(app), "Q01"); i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageUp})
		desktop.Redraw()
	}
	if !strings.Contains(q459Screen(app), "Q01") {
		t.Fatalf("PageUp did not return to the top (scroll not reversible):\n%s", q459Screen(app))
	}
}

// TestQ459MouseWheelScrolls proves the wheel scrolls the panel regardless of which
// field holds focus — the guaranteed scroll affordance per the design.
func TestQ459MouseWheelScrolls(t *testing.T) {
	app, desktop, _ := q459SmallTerminal(t)
	b := q459DialogBounds(t, desktop)
	// Aim at the centre of the dialog content: inside the tab panel, away from the
	// scrollbar column and the buttons.
	cx, cy := b.X+b.W/2, b.Y+b.H/2

	q459Scroll(t, app, tui.ScrollEvent{X: cx, Y: cy, Delta: -1}) // wheel down
	desktop.Redraw()
	if strings.Contains(q459Screen(app), "Q01") {
		t.Fatalf("wheel-down did not scroll (Q01 still at top):\n%s", q459Screen(app))
	}

	q459Scroll(t, app, tui.ScrollEvent{X: cx, Y: cy, Delta: 1}) // wheel up
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "Q01") {
		t.Fatalf("wheel-up did not scroll back to the top:\n%s", q459Screen(app))
	}
}

// TestQ459ScrollbarHidesWhenContentFits: the bar must not render (maxScroll==0) when a
// topic fits its panel — guards against a permanently-visible scrollbar.
func TestQ459ScrollbarHidesWhenContentFits(t *testing.T) {
	app := tui.NewWithSize(120, 40, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(2), func(agent.QuestionResponse) {})
	desktop.Redraw()
	if q459HasScrollbar(app) {
		t.Fatalf("scrollbar rendered for content that fits:\n%s", q459Screen(app))
	}
}

// TestQ459ScrollbarRendersWhenOverflowing is the scrollbar half of criterion 2
// ("scrolls … with a scrollbar"). It currently FAILS and exposes a real defect:
// the 1-column scrollbar is placed at panel-relative barX = width-1, but the panel
// fills the Tabs widget (dialog width-2) which itself sits at content-relative X:1,
// so the bar lands on the window's right-BORDER column. The window's content clip
// (Inset(1), cols content-1..content-W-2) drops that column, so no scrollbar glyph
// (▲/▼/█/│) ever renders — at any terminal size. The scroll mechanics themselves
// work (see TestQ459PageDownRevealsHiddenField); only the visual bar is clipped away.
func TestQ459ScrollbarRendersWhenOverflowing(t *testing.T) {
	for _, size := range [][2]int{{60, 16}, {80, 24}, {100, 30}} {
		app := tui.NewWithSize(size[0], size[1], &strings.Builder{})
		desktop := tv.NewDesktop(app)
		showQuestionDialog(desktop, q459OverflowRequest(8), func(agent.QuestionResponse) {})
		desktop.Redraw()
		if !q459HasScrollbar(app) {
			t.Fatalf("scrollbar did not render for overflowing content at %vx%v (bar clipped onto the right border):\n%s",
				size[0], size[1], q459Screen(app))
		}
		desktop.RemoveLayer(desktop.TopLayer())
	}
}

// TestQ459ScrollsInteractiveWidgetTypes exercises each input widget kind inside a
// scrolling panel (choice/multiselect checkboxes, textarea, text) and confirms a
// below-the-fold item of each is reachable by scrolling.
func TestQ459ScrollsInteractiveWidgetTypes(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	req := agent.QuestionRequest{
		Title: "Kinds",
		Topics: []agent.QuestionTopic{{Title: "K", Items: []agent.QuestionItem{
			{ID: "c", Label: "CHOOSEONE", Type: agent.QuestionChoice, Options: []string{"c1", "c2"}},
			{ID: "m", Label: "MULTIPICK", Type: agent.QuestionMultiSelect, Options: []string{"m1", "m2"}},
			{ID: "t", Label: "TXTAREA", Type: agent.QuestionTextarea},
			{ID: "x", Label: "TAILFIELD", Type: agent.QuestionText},
		}}},
	}
	showQuestionDialog(desktop, req, func(agent.QuestionResponse) {})
	desktop.Redraw()

	if !strings.Contains(q459Screen(app), "CHOOSEONE") {
		t.Fatalf("first field not visible at top:\n%s", q459Screen(app))
	}
	// Scroll to the bottom and confirm the last (text) field is reachable.
	for i := 0; i < 40; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "TAILFIELD") {
		t.Fatalf("TAILFIELD unreachable by scrolling an interactive panel:\n%s", q459Screen(app))
	}
}

// ---------------------------------------------------------------------------
// Criterion 3 — required validation reaches a field below the scroll fold.
// ---------------------------------------------------------------------------

// TestQ459RequiredFieldBelowFoldRevealed: a required field that is initially scrolled
// out of view must not permanently block submit — submit scrolls it into view and
// focuses it, and shows the inline error.
func TestQ459RequiredFieldBelowFoldRevealed(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	items := make([]agent.QuestionItem, 0, 5)
	for i := 0; i < 4; i++ {
		items = append(items, agent.QuestionItem{
			ID: fmt.Sprintf("opt%02d", i), Label: fmt.Sprintf("Opt%02d", i), Type: agent.QuestionText,
		})
	}
	// Required field placed last so it starts below the fold.
	items = append(items, agent.QuestionItem{ID: "must", Label: "MUSTFILL", Type: agent.QuestionText, Required: true})
	showQuestionDialog(desktop, agent.QuestionRequest{
		Title:  "Required",
		Topics: []agent.QuestionTopic{{Title: "R", Items: items}},
	}, func(agent.QuestionResponse) {})
	desktop.Redraw()

	if strings.Contains(q459Screen(app), "MUSTFILL") {
		t.Fatalf("precondition failed: MUSTFILL already visible at top:\n%s", q459Screen(app))
	}

	issue406SubmitViaDialogRoot(t, desktop) // Ctrl+Enter
	desktop.Redraw()
	screen := q459Screen(app)

	if !strings.Contains(screen, "MUSTFILL is required") {
		t.Fatalf("required error not rendered:\n%s", screen)
	}
	if !strings.Contains(screen, "MUSTFILL") {
		t.Fatalf("required field MUSTFILL was not scrolled into view on submit — a hidden required field blocks submit:\n%s", screen)
	}
	// The focused widget (the MUSTFILL input) must now be visible-in-tree: typing into
	// it must land in the (visible) field rather than strand on a hidden one. The next
	// test completes the scenario by filling it and re-submitting.
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: 'X'})
	desktop.Redraw()
}

// TestQ459RequiredBelowFoldThenAnsweredSubmits completes the previous scenario: after
// the required field is revealed and filled, submit succeeds.
func TestQ459RequiredBelowFoldThenAnsweredSubmits(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	res := make(chan agent.QuestionResponse, 2)
	items := []agent.QuestionItem{
		{ID: "a", Label: "FillA", Type: agent.QuestionText},
		{ID: "b", Label: "FillB", Type: agent.QuestionText},
		{ID: "c", Label: "FillC", Type: agent.QuestionText},
		{ID: "c4", Label: "FillD", Type: agent.QuestionText},
		{ID: "must", Label: "MUSTFILL", Type: agent.QuestionText, Required: true},
	}
	showQuestionDialog(desktop, agent.QuestionRequest{
		Title:  "Required",
		Topics: []agent.QuestionTopic{{Title: "R", Items: items}},
	}, func(r agent.QuestionResponse) { res <- r })
	desktop.Redraw()

	// First submit: blocked, MUSTFILL revealed & focused.
	issue406SubmitViaDialogRoot(t, desktop)
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "MUSTFILL") {
		t.Fatalf("MUSTFILL not revealed:\n%s", q459Screen(app))
	}
	// Type into the now-focused required field, then submit again.
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: 'o'})
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: 'k'})
	issue406SubmitViaDialogRoot(t, desktop)

	select {
	case got := <-res:
		if got.Cancelled {
			t.Fatalf("submit cancelled after filling the required field: %+v", got)
		}
		if got.Answers["must"] != "ok" {
			t.Fatalf("required answer not collected: %#v", got.Answers)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for submit after filling the required field")
	}
}

// ---------------------------------------------------------------------------
// Criterion 4 — resize reflow.
// ---------------------------------------------------------------------------

// TestQ459ResizeReflowsGrowsAndShrinks: after a terminal resize the dialog frame,
// buttons and panel all re-derive from the new size. Growing the terminal should let
// more (eventually all) fields fit without scrolling; shrinking must not panic and
// must keep the buttons visible and the dialog scrollable.
func TestQ459ResizeReflowsGrowsAndShrinks(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(6), func(agent.QuestionResponse) {})
	desktop.Redraw()
	if strings.Contains(q459Screen(app), "Q06") {
		t.Fatal("precondition: Q06 should be hidden at 60×16")
	}

	// Grow: on a large terminal the panel gains rows; the bar should disappear once
	// everything fits, proving visibleRows was re-derived.
	app.Resize(120, 40)
	desktop.Redraw()
	grown := q459Screen(app)
	if !strings.Contains(grown, "Submit") {
		t.Fatalf("buttons not visible after resize-up:\n%s", grown)
	}
	if !strings.Contains(grown, "Q06") {
		t.Fatalf("content did not reflow to reveal Q06 after resize-up (panel height not re-derived):\n%s", grown)
	}
	if q459HasScrollbar(app) {
		t.Fatalf("scrollbar still present after resize-up to a size where content fits:\n%s", grown)
	}

	// Shrink back: must not panic, buttons stay visible, and overflow/scroll return.
	app.Resize(60, 16)
	desktop.Redraw()
	shrunk := q459Screen(app)
	if !strings.Contains(shrunk, "Submit") {
		t.Fatalf("buttons not visible after resize-down:\n%s", shrunk)
	}
	if strings.Contains(shrunk, "Q06") {
		t.Fatalf("content did not re-clip after resize-down:\n%s", shrunk)
	}
	// And scrolling still works after the round-trip.
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "Q04") {
		t.Fatalf("scrolling broken after a resize round-trip:\n%s", q459Screen(app))
	}
}

// TestQ459ResizeKeepsButtonsOnInteriorRow: after resize the buttons must still sit on
// the last interior row of the NEW dialog, never on the border.
func TestQ459ResizeKeepsButtonsOnInteriorRow(t *testing.T) {
	app := tui.NewWithSize(80, 24, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(2), func(agent.QuestionResponse) {})
	desktop.Redraw()

	for _, size := range [][2]int{{120, 40}, {100, 30}, {70, 20}, {50, 14}} {
		app.Resize(size[0], size[1])
		desktop.Redraw()
		b := q459DialogBounds(t, desktop)
		borderRow := b.Y + b.H - 1
		if r := q459RowOf(t, app, "Submit"); r == -1 {
			t.Fatalf("Submit vanished after resize to %v:\n%s", size, q459Screen(app))
		} else if r == borderRow {
			t.Fatalf("Submit on the border row %d after resize to %v (clip regression):\n%s", borderRow, size, q459Screen(app))
		}
	}
}

// TestQ459HorizontalResizeReflowsFieldWidth: a horizontal resize must re-derive field
// widths (itemW), not just the frame — a field should not keep its open-time width and
// overflow/underflow the new panel.
func TestQ459HorizontalResizeReflowsFieldWidth(t *testing.T) {
	app := tui.NewWithSize(80, 24, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	// A wide, distinctive label so truncation/overflow is detectable.
	req := agent.QuestionRequest{
		Title: "Width",
		Topics: []agent.QuestionTopic{{Title: "W", Items: []agent.QuestionItem{
			{ID: "x", Label: "LABELSTAYSVISIBLE", Type: agent.QuestionText},
		}}},
	}
	showQuestionDialog(desktop, req, func(agent.QuestionResponse) {})
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "LABELSTAYSVISIBLE") {
		t.Fatalf("label not visible at open:\n%s", q459Screen(app))
	}
	// Narrow and widen; the label must remain visible (reflowed, not clipped away).
	for _, size := range [][2]int{{60, 24}, {100, 24}, {50, 24}} {
		app.Resize(size[0], size[1])
		desktop.Redraw()
		if !strings.Contains(q459Screen(app), "LABELSTAYSVISIBLE") {
			t.Fatalf("label lost after horizontal resize to %v (field width not re-derived):\n%s", size, q459Screen(app))
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 5 — tab-label overflow reachability.
// ---------------------------------------------------------------------------

// TestQ459TabOverflowAllTopicsReachable: with more topics than the strip can label,
// every topic must stay reachable via Prev/Next (which wrap), and the indicator must
// name the active one so the user knows where they are.
func TestQ459TabOverflowAllTopicsReachable(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	topics := make([]agent.QuestionTopic, 0, 8)
	for i := 0; i < 8; i++ {
		topics = append(topics, agent.QuestionTopic{
			Title: fmt.Sprintf("TopicNumber%d", i+1), // long enough to overflow the strip
			Items: []agent.QuestionItem{{ID: fmt.Sprintf("only%d", i), Label: fmt.Sprintf("MARKER%02d", i+1), Type: agent.QuestionText}},
		})
	}
	showQuestionDialog(desktop, agent.QuestionRequest{Title: "ManyTopics", Topics: topics}, func(agent.QuestionResponse) {})
	desktop.Redraw()

	// Indicator should announce topic position for >1 topic.
	if !strings.Contains(q459Screen(app), "Topic ") || !strings.Contains(q459Screen(app), "/8") {
		t.Fatalf("topic indicator missing Topic n/N:\n%s", q459Screen(app))
	}

	// Next through every topic, wrapping back to the first; each topic's marker must
	// render when it becomes active (proving reachability despite a clipped strip).
	for i := 0; i < 8; i++ {
		want := fmt.Sprintf("MARKER%02d", ((i+1)%8)+1)
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab, Ctrl: true}) // Ctrl+Tab → switchBy(+1)
		desktop.Redraw()
		if !strings.Contains(q459Screen(app), want) {
			t.Fatalf("topic %d marker %q not reachable (iteration %d):\n%s", ((i+1)%8)+1, want, i, q459Screen(app))
		}
	}
	// Indicator updated to reflect the active topic after switches.
	if !strings.Contains(q459Screen(app), "/8") {
		t.Fatalf("indicator lost after tab cycling:\n%s", q459Screen(app))
	}
}

// TestQ459TabTitleElidedOnStripButFullInIndicator: a long title is capped on the strip
// (elideTabTitle) yet the full title remains in the indicator row.
func TestQ459TabTitleElidedOnStripButFullInIndicator(t *testing.T) {
	if got := elideTabTitle("A Very Long Topic Title Indeed"); got == "A Very Long Topic Title Indeed" {
		t.Fatalf("elideTabTitle did not cap a long title: %q", got)
	}
	// Short titles pass through unchanged.
	if got := elideTabTitle("Short"); got != "Short" {
		t.Fatalf("elideTabTitle altered a short title: %q", got)
	}
}

// TestQ459PrevNextButtonsRenderMultiTopic: with >1 topic the Prev/Next buttons render
// alongside Cancel/Submit, and all four are visible.
func TestQ459PrevNextButtonsRenderMultiTopic(t *testing.T) {
	app := tui.NewWithSize(80, 24, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, agent.QuestionRequest{
		Title: "Multi",
		Topics: []agent.QuestionTopic{
			{Title: "A", Items: []agent.QuestionItem{{ID: "a", Label: "A", Type: agent.QuestionText}}},
			{Title: "B", Items: []agent.QuestionItem{{ID: "b", Label: "B", Type: agent.QuestionText}}},
		},
	}, func(agent.QuestionResponse) {})
	desktop.Redraw()
	screen := q459Screen(app)
	for _, want := range []string{"Cancel", "Prev", "Next", "Submit"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("multi-topic button %q not rendered:\n%s", want, screen)
		}
	}
}

// ---------------------------------------------------------------------------
// No-regression: the canScroll gate and per-tab independence.
// ---------------------------------------------------------------------------

// TestQ459ShortFormDoesNotScroll: when a topic fits (maxScroll==0), the scroll keys
// must decline and leave every field visible — a short form behaves exactly as before.
func TestQ459ShortFormDoesNotScroll(t *testing.T) {
	app := tui.NewWithSize(120, 40, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(2), func(agent.QuestionResponse) {})
	desktop.Redraw()
	before := q459Screen(app)
	for _, k := range []tui.KeyCode{tui.KeyUp, tui.KeyDown, tui.KeyPageUp, tui.KeyPageDown} {
		q459Dispatch(t, app, tui.TypeEvent{Key: k})
	}
	desktop.Redraw()
	after := q459Screen(app)
	// Nothing should have scrolled away: both fields still visible.
	for _, want := range []string{"Q01", "Q02"} {
		if !strings.Contains(after, want) {
			t.Fatalf("scroll key hid %q on a short form (canScroll gate failed):\nbefore:\n%s\nafter:\n%s", want, before, after)
		}
	}
}

// TestQ459PerTabScrollIndependence: scroll state is per-panel. Scrolling an
// overflowing tab, switching to a short tab (no scroll), then back, leaves the
// overflowing tab still scrollable and the short tab unaffected.
func TestQ459PerTabScrollIndependence(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	long := make([]agent.QuestionItem, 0, 6)
	for i := 0; i < 6; i++ {
		long = append(long, agent.QuestionItem{ID: fmt.Sprintf("l%d", i), Label: fmt.Sprintf("L%02d", i+1), Type: agent.QuestionText})
	}
	req := agent.QuestionRequest{Title: "Two", Topics: []agent.QuestionTopic{
		{Title: "Long", Items: long},
		{Title: "Short", Items: []agent.QuestionItem{{ID: "s", Label: "SHORTFIELD", Type: agent.QuestionText}}},
	}}
	showQuestionDialog(desktop, req, func(agent.QuestionResponse) {})
	desktop.Redraw()

	// Tab 0 (Long) overflows; scroll it.
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	desktop.Redraw()
	if strings.Contains(q459Screen(app), "L01") {
		t.Fatalf("Long tab did not scroll:\n%s", q459Screen(app))
	}

	// Switch to Short (Ctrl+Tab). It fits; scroll keys must not scroll it (gate).
	// Use PageDown (not Down) so the desktop's arrow spatial-nav cannot move focus
	// out of the panel — focus stays inside Tabs, letting the next Ctrl+Tab switch.
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab, Ctrl: true})
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "SHORTFIELD") {
		t.Fatalf("Short tab content not shown after switch:\n%s", q459Screen(app))
	}
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "SHORTFIELD") {
		t.Fatalf("Short tab scrolled away a fitting field (per-tab gate failed):\n%s", q459Screen(app))
	}

	// Back to Long; it must still be scrollable (its state is independent of Short's).
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab, Ctrl: true})
	desktop.Redraw()
	for i := 0; i < 40; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "L06") {
		t.Fatalf("Long tab last field unreachable after visiting Short tab:\n%s", q459Screen(app))
	}
}

// TestQ459EnterAdvancesAndScrollsIntoView: Enter in a single-line field advances focus
// to the next field and scrolls it into view if it was below the fold.
func TestQ459EnterAdvancesAndScrollsIntoView(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(6), func(agent.QuestionResponse) {})
	desktop.Redraw()

	// Focus starts on Q01's box. Pressing Enter repeatedly advances through the
	// fields; each newly-focused field must be scrolled into view, so we never land
	// on an invisible field.
	for i := 0; i < 6; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyEnter})
		desktop.Redraw()
	}
	// After advancing through all fields we should be able to type without the focus
	// being stranded off-screen (no panic, dialog still intact).
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: 'Z'})
	desktop.Redraw()
	if desktop.TopLayer() == nil {
		t.Fatal("dialog closed unexpectedly during Enter-advance focus")
	}
}

// ---------------------------------------------------------------------------
// Edge cases / robustness — must not panic.
// ---------------------------------------------------------------------------

// TestQ459EmptyAndZeroTopicRequestsDoNotPanic guards the no-items and no-topics
// branches (empty focusables, empty panels, submit with nothing to validate).
func TestQ459EmptyAndZeroTopicRequestsDoNotPanic(t *testing.T) {
	cases := []struct {
		name string
		req  agent.QuestionRequest
	}{
		{"zero topics", agent.QuestionRequest{Title: "None", Topics: nil}},
		{"one empty topic", agent.QuestionRequest{Title: "Empty", Topics: []agent.QuestionTopic{{Title: "E", Items: nil}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := tui.NewWithSize(60, 16, &strings.Builder{})
			desktop := tv.NewDesktop(app)
			res := make(chan agent.QuestionResponse, 1)
			showQuestionDialog(desktop, tc.req, func(r agent.QuestionResponse) { res <- r })
			desktop.Redraw()

			// Scroll keys must be a no-op (not a panic) with nothing to scroll.
			for _, k := range []tui.KeyCode{tui.KeyPageDown, tui.KeyPageUp, tui.KeyDown, tui.KeyUp} {
				q459Dispatch(t, app, tui.TypeEvent{Key: k})
			}
			desktop.Redraw()
			// Submit resolves (nothing required) and tear down via Escape as a backstop.
			issue406SubmitViaDialogRoot(t, desktop)
			desktop.Redraw()
			<-res
		})
	}
}

// TestQ459TinyTerminalRendersButtons ensures the minimum-size path (floored dialog on
// a very small terminal) still shows the buttons and does not panic on resize.
func TestQ459TinyTerminalRendersButtons(t *testing.T) {
	app := tui.NewWithSize(40, 12, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	showQuestionDialog(desktop, q459OverflowRequest(1), func(agent.QuestionResponse) {})
	desktop.Redraw()
	// Buttons should still attempt to render (clamped), and nothing panics.
	app.Resize(36, 11)
	desktop.Redraw()
	app.Resize(80, 24)
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "Submit") {
		t.Fatalf("Submit not visible after tiny→normal resize:\n%s", q459Screen(app))
	}
}

// ===========================================================================
// Round-2 coverage: thumb tracking, scroll×resize interaction, realistic forms,
// cross-tab required reachability, empty-options edges, wheel-over-textarea.
// ===========================================================================

// q459ThumbRow returns the screen row of the scrollbar thumb (█), or -1. █ is
// unique to the scrollbar — borders use box-drawing glyphs and the shadow is ░.
func q459ThumbRow(app *tui.App) int {
	for y := 0; y < app.Height(); y++ {
		for x := 0; x < app.Width(); x++ {
			if app.ReadCell(x, y).Ch == '█' {
				return y
			}
		}
	}
	return -1
}

// TestQ459ScrollbarThumbTracksScroll verifies the thumb actually moves with the
// offset (the round-1 test only checked the bar's presence). At the top the thumb
// sits near the ▲ cap; scrolled to the bottom it sits near the ▼ cap.
func TestQ459ScrollbarThumbTracksScroll(t *testing.T) {
	app, desktop, _ := q459SmallTerminal(t)
	topThumb := q459ThumbRow(app)
	if topThumb < 0 {
		t.Fatalf("no scrollbar thumb rendered at scroll top:\n%s", q459Screen(app))
	}

	for i := 0; i < 40; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	bottomThumb := q459ThumbRow(app)
	if bottomThumb < 0 {
		t.Fatalf("no scrollbar thumb rendered at scroll bottom:\n%s", q459Screen(app))
	}
	if bottomThumb <= topThumb {
		t.Fatalf("scrollbar thumb did not move down with scroll (top=%d bottom=%d):\n%s",
			topThumb, bottomThumb, q459Screen(app))
	}
}

// TestQ459ScrollThenResizeLargerClampsToTop: after scrolling down, a resize that
// makes the content fit must re-clamp scrollY to 0 (LayoutFn runs clampScroll),
// reveal the top field again, and hide the bar. Guards a stale scroll offset
// leaving the dialog looking empty after a grow.
func TestQ459ScrollThenResizeLargerClampsToTop(t *testing.T) {
	app, desktop, _ := q459SmallTerminal(t)
	for i := 0; i < 40; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	if strings.Contains(q459Screen(app), "Q01") {
		t.Fatalf("precondition: Q01 should be scrolled off the top:\n%s", q459Screen(app))
	}

	app.Resize(120, 40) // content now fits the taller panel
	desktop.Redraw()
	screen := q459Screen(app)
	if !strings.Contains(screen, "Q01") {
		t.Fatalf("scroll offset was not re-clamped to the top after a grow (Q01 still hidden):\n%s", screen)
	}
	if q459HasScrollbar(app) {
		t.Fatalf("scrollbar still present after a grow that fits the content:\n%s", screen)
	}
}

// TestQ459RealisticOverflowingFormAllReachable mirrors the issue's actual repro: a
// single topic mixing a text+placeholder, a textarea+placeholder, a multiselect, a
// choice, a text+help and a textarea — exactly the combo that overflows quickly. No
// field may be permanently hidden; the last one must be reachable by scrolling.
func TestQ459RealisticOverflowingFormAllReachable(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	req := agent.QuestionRequest{Title: "Real", Topics: []agent.QuestionTopic{{
		Title: "Form",
		Items: []agent.QuestionItem{
			{ID: "name", Label: "NAMEFIELD", Type: agent.QuestionText, Placeholder: "your name"},
			{ID: "bio", Label: "BIOFIELD", Type: agent.QuestionTextarea, Placeholder: "about you"},
			{ID: "skills", Label: "SKILLS", Type: agent.QuestionMultiSelect, Options: []string{"go", "python", "rust"}},
			{ID: "level", Label: "LEVEL", Type: agent.QuestionChoice, Options: []string{"junior", "mid", "senior"}},
			{ID: "loc", Label: "LOCFIELD", Type: agent.QuestionText, Help: "city only"},
			{ID: "last", Label: "LASTFIELD", Type: agent.QuestionTextarea},
		},
	}}}
	showQuestionDialog(desktop, req, func(agent.QuestionResponse) {})
	desktop.Redraw()

	if !strings.Contains(q459Screen(app), "NAMEFIELD") {
		t.Fatalf("first field not visible at top:\n%s", q459Screen(app))
	}
	if strings.Contains(q459Screen(app), "LASTFIELD") {
		t.Fatalf("precondition: LASTFIELD should be below the fold:\n%s", q459Screen(app))
	}
	// Scroll to the bottom; the last field must be reachable (not clipped away).
	for i := 0; i < 60; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	if !strings.Contains(q459Screen(app), "LASTFIELD") {
		t.Fatalf("LASTFIELD unreachable in a realistic overflowing form:\n%s", q459Screen(app))
	}
}

// TestQ459RequiredInNonActiveScrolledTab: a required field that lives in a
// non-active topic AND sits below that topic's fold must still be revealed, focused
// and error-tagged on submit (criterion 3 across tabs + scroll).
func TestQ459RequiredInNonActiveScrolledTab(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	tab1 := make([]agent.QuestionItem, 0, 5)
	for i := 0; i < 4; i++ {
		tab1 = append(tab1, agent.QuestionItem{ID: fmt.Sprintf("p%d", i), Label: fmt.Sprintf("Pad%02d", i), Type: agent.QuestionText})
	}
	tab1 = append(tab1, agent.QuestionItem{ID: "must", Label: "MUSTFILL", Type: agent.QuestionText, Required: true})
	showQuestionDialog(desktop, agent.QuestionRequest{Title: "Two", Topics: []agent.QuestionTopic{
		{Title: "First", Items: []agent.QuestionItem{{ID: "a", Label: "A0", Type: agent.QuestionText}}},
		{Title: "Second", Items: tab1},
	}}, func(agent.QuestionResponse) {})
	desktop.Redraw()

	// Active tab is First; MUSTFILL is in Second and below its fold.
	if strings.Contains(q459Screen(app), "MUSTFILL") {
		t.Fatalf("precondition: MUSTFILL should not be visible initially:\n%s", q459Screen(app))
	}

	issue406SubmitViaDialogRoot(t, desktop)
	desktop.Redraw()
	screen := q459Screen(app)
	if !strings.Contains(screen, "MUSTFILL is required") {
		t.Fatalf("required error not shown for the non-active tab's field:\n%s", screen)
	}
	if !strings.Contains(screen, "MUSTFILL") {
		t.Fatalf("MUSTFILL (in a non-active, scrolled topic) was not revealed on submit:\n%s", screen)
	}
	if !strings.Contains(screen, "Topic 2/2") {
		t.Fatalf("dialog did not switch to the offending topic:\n%s", screen)
	}
}

// TestQ459EmptyOptionsChoiceAndMultiSelectNoPanic: choice/multiselect with no
// Options build zero checkboxes. They must not panic, and submit must handle the
// (always-unanswered) item without deadlocking — optional ones are omitted.
func TestQ459EmptyOptionsChoiceAndMultiSelectNoPanic(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	res := make(chan agent.QuestionResponse, 1)
	showQuestionDialog(desktop, agent.QuestionRequest{Title: "Empty", Topics: []agent.QuestionTopic{{
		Title: "E",
		Items: []agent.QuestionItem{
			{ID: "c", Label: "EmptyChoice", Type: agent.QuestionChoice, Options: nil},
			{ID: "m", Label: "EmptyMulti", Type: agent.QuestionMultiSelect, Options: nil},
			{ID: "ok", Label: "OK", Type: agent.QuestionText},
		},
	}}}, func(r agent.QuestionResponse) { res <- r })
	desktop.Redraw()

	// Scrolling and submitting must not panic on the option-less items.
	for i := 0; i < 10; i++ {
		q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	}
	desktop.Redraw()
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageUp})
	desktop.Redraw()
	issue406SubmitViaDialogRoot(t, desktop)
	select {
	case got := <-res:
		// The empty choice/multiselect are unanswered → omitted; the text field is blank
		// → omitted too. Submit succeeds with an empty (but non-cancelled) answer set.
		if got.Cancelled {
			t.Fatalf("submit cancelled unexpectedly: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out: submit did not resolve for option-less choice/multiselect")
	}
}

// q459WalkVisible runs fn over root and its visible descendants.
func q459WalkVisible(root *tv.VisualComponent, fn func(*tv.VisualComponent)) {
	if root == nil || !root.Visible {
		return
	}
	fn(root)
	for _, c := range root.Children() {
		q459WalkVisible(c, fn)
	}
}

// q459FindVisibleByHeight returns the absolute bounds of the first visible leaf
// with the given Bounds.H (3 ⇒ a MultiLineInput textarea, since labels/textboxes are
// H=1), so a test can target a specific widget without hard-coding coordinates.
func q459FindVisibleByHeight(desktop *tv.Desktop, h int) (tv.Rect, bool) {
	top := desktop.TopLayer()
	if top == nil {
		return tv.Rect{}, false
	}
	var found tv.Rect
	hit := false
	q459WalkVisible(top.Root, func(c *tv.VisualComponent) {
		if !hit && c.Bounds.H == h {
			found = c.AbsoluteBounds()
			hit = true
		}
	})
	return found, hit
}

// TestQ459WheelOverTextareaDoesNotScrollPanel documents an interaction limitation
// the design's prose claimed was solved ("mouse wheel … always works, including
// inside a textarea"). It is NOT: MultiLineInput.handleScroll consumes every wheel
// event (returns true), so BubbleScroll never reaches the panel's OnScrollFn while
// the pointer is over a textarea. An empty textarea has nothing to scroll either, so
// the wheel is a pure no-op there. The panel is still scrollable via PageDown and via
// the wheel when it is over a label/textbox/spacer. This test pins the actual
// behaviour so a regression (in either direction) is caught.
func TestQ459WheelOverTextareaDoesNotScrollPanel(t *testing.T) {
	app := tui.NewWithSize(60, 16, &strings.Builder{})
	desktop := tv.NewDesktop(app)
	// A textarea as the first item (visible at the top) plus more fields so the topic
	// overflows and is scrollable.
	showQuestionDialog(desktop, agent.QuestionRequest{Title: "TA", Topics: []agent.QuestionTopic{{
		Title: "T",
		Items: []agent.QuestionItem{
			{ID: "ta", Label: "TOPAREA", Type: agent.QuestionTextarea},
			{ID: "x1", Label: "XTWO", Type: agent.QuestionText},
			{ID: "x2", Label: "XTHREE", Type: agent.QuestionText},
			{ID: "x3", Label: "XFOUR", Type: agent.QuestionText},
			{ID: "x4", Label: "XFIVE", Type: agent.QuestionText},
		},
	}}}, func(agent.QuestionResponse) {})
	desktop.Redraw()

	ta, ok := q459FindVisibleByHeight(desktop, 3)
	if !ok {
		t.Fatalf("no visible textarea (H=3) found to wheel over:\n%s", q459Screen(app))
	}
	cx := ta.X + ta.W/2
	cy := ta.Y + ta.H/2

	// Wheel straight down while over the textarea.
	for i := 0; i < 5; i++ {
		q459Scroll(t, app, tui.ScrollEvent{X: cx, Y: cy, Delta: -1})
	}
	desktop.Redraw()
	// The panel must NOT have scrolled: the first field is still on screen because
	// the textarea swallowed the wheel. (If this assertion fails, the panel now
	// captures the wheel over a textarea — update the documented behaviour.)
	if !strings.Contains(q459Screen(app), "TOPAREA") {
		t.Fatalf("wheel over a textarea scrolled the panel (textarea did not consume it) — behaviour changed:\n%s", q459Screen(app))
	}

	// …yet PageDown still scrolls the panel from the same focus, so reachability holds.
	q459Dispatch(t, app, tui.TypeEvent{Key: tui.KeyPageDown})
	desktop.Redraw()
	if strings.Contains(q459Screen(app), "TOPAREA") {
		t.Fatalf("PageDown did not scroll the panel (reachability regression):\n%s", q459Screen(app))
	}
}
