package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Round-3 coverage for the fixes-round-2 footer rework: the footer "Close" button
// was removed (it pushed the seven-control row past an 80-column terminal), "Open
// Session" was shortened to "Open", the dialog width is floored to the footer's
// need, and the dialog is now dismissed via Esc or the title-bar [■]. These tests
// guard those changes — especially that the dialog can still be closed at all (with
// the footer Close gone, a broken Esc/[■] would make it a modal trap).

// openWatchersDialogWith opens the dialog over one window with a single watcher
// selected, on a roomy terminal, and returns the workbench.
func openWatchersDialogWith(t *testing.T) *Workbench {
	t.Helper()
	att := WatcherInfo{ID: "w-1", Name: "poll", Free: false, TargetSession: "s",
		SessionID: "s", Enabled: true, Status: "idle", Task: "poll"}
	w, _ := wiredWatcherWorkbench(t, "s", att)
	w.showWatchersDialog()
	return w
}

// TestWatchersDialogClosesOnEsc verifies Esc tears the modal layer down via the
// dialog root's key handler — the keyboard exit now that the footer Close is gone.
func TestWatchersDialogClosesOnEsc(t *testing.T) {
	w := openWatchersDialogWith(t)
	top := w.desktop.TopLayer()
	if top == nil || top.Name != "watchers-dialog" {
		t.Fatalf("watchers dialog not on top: %+v", top)
	}
	root := top.Root
	if root.OnTypeFn == nil {
		t.Fatal("dialog root has no key handler to receive Esc")
	}
	if handled := root.OnTypeFn(root, tui.TypeEvent{Key: tui.KeyEscape}); !handled {
		t.Error("Esc should be handled (consumed) by the watchers dialog")
	}
	if now := w.desktop.TopLayer(); now != nil && now.Name == "watchers-dialog" {
		t.Error("Esc should close the watchers dialog (remove its modal layer)")
	}
}

// TestWatchersDialogShowsTitleBarClose verifies the title-bar [■] close affordance
// is actually rendered — with the footer Close removed in round 2, this is the only
// mouse exit, so a regression to ShowClose=false would strand the user.
func TestWatchersDialogShowsTitleBarClose(t *testing.T) {
	w := openWatchersDialogWith(t)
	screen := screenText(w)
	if !strings.Contains(screen, "[■]") {
		t.Errorf("watchers dialog title bar should render the [■] close button (footer Close was removed)\n%s", screen)
	}
	// And the title is present so we know we are looking at the right dialog.
	if !strings.Contains(screen, "Watchers") {
		t.Errorf("watchers dialog title not on screen:\n%s", screen)
	}
}

// TestWatchersFooterHasNoCloseButton is the regression for the round-2 label
// change: the footer row carries EXACTLY the six action buttons (Open, Enable,
// Disable, Run, Stop, Delete) and no seventh Close — re-adding Close would push the
// row past an 80-column terminal again.
func TestWatchersFooterHasNoCloseButton(t *testing.T) {
	if len(watchersFooterLabels) != 6 {
		t.Fatalf("expected 6 footer labels, got %d: %v", len(watchersFooterLabels), watchersFooterLabels)
	}
	for _, l := range watchersFooterLabels {
		if strings.Contains(strings.ToLower(l), "close") {
			t.Errorf("footer must not contain a Close button (moved to title-bar/Esc): %q", l)
		}
	}
	// The first control is Open (shortened from "Open Session").
	if watchersFooterLabels[0] != "&Open" {
		t.Errorf("first footer control = %q, want \"&Open\"", watchersFooterLabels[0])
	}

	// On screen, exactly six DrawOutside footer buttons sit on the button row.
	w := openWatchersDialogWith(t)
	b := dialogBounds(w)
	buttonY := b.H - 3
	wantFooter := footerButtonRects(watchersFooterLabels, 2, b.W-3, buttonY, tv.DefaultButtonGap)
	got := 0
	for _, c := range dialogDescendants(w) {
		if c.Bounds.Y == buttonY && c.DrawOutside {
			got++
			if !containsRect(wantFooter, c.Bounds) {
				t.Errorf("footer button at unexpected rect %+v (want one of %+v)", c.Bounds, wantFooter)
			}
		}
	}
	if got != 6 {
		t.Errorf("button row has %d buttons, want exactly 6 (no footer Close)", got)
	}
}

// TestFooterRowMinWidth pins the helper that floors the dialog width: the sum of
// the rendered button widths, the inter-button gaps, and the 4 edge columns
// footerButtonRects reserves (leftX=2 inset + width-3 right margin).
func TestFooterRowMinWidth(t *testing.T) {
	labels := []string{"AAA", "BBB"} // two buttons
	gap := tv.DefaultButtonGap
	want := tv.ButtonLabelWidth("AAA") + gap + tv.ButtonLabelWidth("BBB") + 4
	if got := footerRowMinWidth(labels, gap); got != want {
		t.Errorf("footerRowMinWidth = %d, want %d", got, want)
	}
	// A single button: no gap term.
	if got := footerRowMinWidth([]string{"X"}, gap); got != tv.ButtonLabelWidth("X")+4 {
		t.Errorf("single-button min = %d, want %d", got, tv.ButtonLabelWidth("X")+4)
	}
}

// TestWatchersFooterFitsStandardTerminal is the core guarantee of the round-2 fix:
// the six-control footer must fit within an 80-column terminal. footerRowMinWidth
// is the floored dialog width, so it must be <= 80 — otherwise the dialog overflows
// the standard terminal (the symptom the fix targets). This also guards against a
// future label addition silently pushing the floor back over 80.
func TestWatchersFooterFitsStandardTerminal(t *testing.T) {
	need := footerRowMinWidth(watchersFooterLabels, tv.DefaultButtonGap)
	if need > 80 {
		t.Errorf("watchers footer needs %d cols (> 80): it will overflow a standard terminal", need)
	}
}

// TestWatchersDialogFloorAppliedOnOpen verifies the floor is actually applied when
// the dialog opens on an 80-column terminal: the resolved width is at least the
// footer's need (so the buttons do not clamp into overlap) and does not exceed the
// screen.
func TestWatchersDialogFloorAppliedOnOpen(t *testing.T) {
	att := WatcherInfo{ID: "w-1", Name: "poll", Free: false, TargetSession: "s", SessionID: "s", Enabled: true, Status: "idle"}
	w, _ := wiredWatcherWorkbench(t, "s", att)
	w.app.Resize(80, 24)
	w.showWatchersDialog()
	b := dialogBounds(w)
	need := footerRowMinWidth(watchersFooterLabels, tv.DefaultButtonGap)
	if b.W < need {
		t.Errorf("dialog opened %d wide, below the footer floor %d (buttons would overlap)", b.W, need)
	}
	if b.X+b.W > 80 {
		t.Errorf("dialog right edge %d exceeds the 80-col screen", b.X+b.W)
	}
}
