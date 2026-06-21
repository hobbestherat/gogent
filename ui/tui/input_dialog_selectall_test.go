package ui

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// These tests cover issue #235: the Rename Session dialog must open with the
// whole current name selected, so the first printable keystroke replaces it and
// an arrow / Home / End collapses the selection for in-place editing, while
// confirm/cancel behave as before. The two other input dialogs (Session Goal,
// Find in Transcript) must keep their edit-in-place behaviour (no select-all).
//
// The dialog's *tv.TextBox is a local inside showInputDialog and the component
// tree only stores *tv.VisualComponent, so the tests locate the field as the
// focused widget of the top (input-dialog) layer — exactly what the desktop
// delivers keystrokes to. Keystrokes are driven through BubbleType from that
// focused component, which is the same call the desktop makes
// (d.focused.BubbleType). Selection state is read non-mutatingly via Copy(),
// whose CopyFn on a TextBox returns the selected range (the whole name once
// select-all has run).

// inputResult captures the single onResult callback a dialog fires when it is
// accepted (ok=true) or cancelled (ok=false). fired reports whether the dialog
// has been dismissed at all.
type inputResult struct {
	value string
	ok    bool
	fired bool
}

// findFocusedComponent depth-walks the component tree and returns the node the
// desktop has focused, or nil when none is. After showInputDialog the focused
// node is the dialog's text field, so this is how the tests recover it without
// touching unexported widget state.
func findFocusedComponent(root *tv.VisualComponent) *tv.VisualComponent {
	if root == nil {
		return nil
	}
	if root.Focused() {
		return root
	}
	for _, child := range root.Children() {
		if found := findFocusedComponent(child); found != nil {
			return found
		}
	}
	return nil
}

// inputDialogBox returns the focused text field of the workbench's top layer,
// failing the test if no dialog is open, nothing in it has focus, or focus landed
// somewhere other than the editable field. It also asserts the layer is the
// input-dialog modal. The editable-field check uses the TextBox's CopyFn
// capability, which the dialog's OK/Cancel buttons do not expose, so a focus
// regression (e.g. focus on a button) fails here with a clear message rather
// than producing confusing mid-scenario failures.
func inputDialogBox(t *testing.T, w *Workbench) *tv.VisualComponent {
	t.Helper()
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatalf("expected an input-dialog layer open, got none")
	}
	if top.Name != "input-dialog" {
		t.Fatalf("top layer = %q, want input-dialog", top.Name)
	}
	box := findFocusedComponent(top.Root)
	if box == nil {
		t.Fatalf("no focused widget in input-dialog layer")
	}
	if box.CopyFn == nil {
		t.Fatalf("focused widget is not the editable text field (has no Copy capability); focus mis-targeted")
	}
	return box
}

// openDialog is a focused harness around showInputDialog: it opens a dialog
// seeded with initial, opting into select-all only when selectAll is true, and
// returns the focused text field plus a pointer that receives the onResult
// callback. Each call opens a fresh dialog, so scenarios stay independent.
func openDialog(t *testing.T, w *Workbench, initial string, selectAll bool) (*tv.VisualComponent, *inputResult) {
	t.Helper()
	res := &inputResult{}
	var opts []inputDialogOption
	if selectAll {
		opts = append(opts, withSelectAll())
	}
	w.showInputDialog("Rename Session", "&Title:", initial, func(value string, ok bool) {
		res.value = value
		res.ok = ok
		res.fired = true
	}, opts...)
	return inputDialogBox(t, w), res
}

// typeDlgRune dispatches a printable keystroke to the field exactly as the desktop
// would: bubble from the focused widget upward.
func typeDlgRune(box *tv.VisualComponent, r rune) {
	box.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: r})
}

// typeDlgKey dispatches a named key (arrow, Enter, Escape, Backspace, ...) to the
// field. It returns whether some component in the bubble chain consumed it.
func typeDlgKey(box *tv.VisualComponent, key tui.KeyCode) bool {
	return box.BubbleType(tui.TypeEvent{Key: key})
}

// submitDlg presses Enter, accepting the dialog; the captured result reflects it.
func submitDlg(box *tv.VisualComponent) {
	box.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})
}

// selectionOf reads the field's current selection non-mutatingly (Copy on a
// TextBox returns its highlighted range without changing caret or anchor).
func selectionOf(box *tv.VisualComponent) (string, bool) {
	return box.Copy()
}

// TestRenameDialogSelectsAllOnOpen is the core assertion for issue #235: right
// after RenameSession opens the dialog, the entire current name is selected. We
// read the selection through the widget's Copy capability, which returns the
// highlighted span — the full name only if select-all spanned every rune.
func TestRenameDialogSelectsAllOnOpen(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "My Refactor")

	w.RenameSession("s")
	box := inputDialogBox(t, w)

	got, ok := selectionOf(box)
	if !ok {
		t.Fatal("rename dialog opened with no selection; want the whole name selected")
	}
	if got != "My Refactor" {
		t.Errorf("initial selection = %q, want the full name %q", got, "My Refactor")
	}
}

// TestRenameDialogLayerIsModal confirms the dialog is the top modal layer so it
// captures keystrokes (and lower layers, including the menu, go inert) — the
// precondition for the first keystroke landing in the text field.
func TestRenameDialogLayerIsModal(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	w.RenameSession("s")

	top := w.desktop.TopLayer()
	if top == nil || top.Name != "input-dialog" || !top.Modal {
		t.Fatalf("top layer = %+v, want a modal input-dialog", top)
	}
}

// TestRenameDialogTypeReplacesWholeName checks the primary UX: a printable
// keystroke replaces the selected name rather than inserting into it. The
// original name is multi-character, so a single rune result proves replacement.
func TestRenameDialogTypeReplacesWholeName(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "Old Name", true)

	typeDlgRune(box, 'x')
	submitDlg(box)

	if !res.fired || !res.ok {
		t.Fatalf("dialog result = %+v, want accepted", res)
	}
	if res.value != "x" {
		t.Errorf("after typing 'x' = %q, want %q (full replace)", res.value, "x")
	}
}

// TestRenameDialogReplaceThenAppend confirms the selection is gone after the
// replacing keystroke, so a second rune appends instead of replacing again.
func TestRenameDialogReplaceThenAppend(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "Old Name", true)

	typeDlgRune(box, 'A') // replaces -> "A"
	typeDlgRune(box, 'B') // appends  -> "AB"
	submitDlg(box)

	if res.value != "AB" {
		t.Errorf("after 'A' then 'B' = %q, want %q", res.value, "AB")
	}
}

// TestRenameDialogRightArrowThenTypeAppends checks the "move caret to append"
// escape hatch: Right collapses the selection and parks the caret at the end,
// so the next rune appends rather than replaces (issue #235 desired behaviour).
func TestRenameDialogRightArrowThenTypeAppends(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	// Precondition: the name must be selected on open, otherwise this scenario
	// cannot prove the arrow collapsed anything.
	if _, ok := selectionOf(box); !ok {
		t.Fatal("precondition: name should be selected on open")
	}
	if consumed := typeDlgKey(box, tui.KeyRight); !consumed {
		t.Fatal("Right should be consumed by the field (collapse selection), not fall through")
	}
	// Selection must be cleared after Right (caret at the end, nothing selected).
	if _, ok := selectionOf(box); ok {
		t.Error("selection still active after Right; want it collapsed for in-place editing")
	}
	typeDlgRune(box, '!')
	submitDlg(box)

	if res.value != "My Session!" {
		t.Errorf("after Right then '!' = %q, want %q (append)", res.value, "My Session!")
	}
}

// TestRenameDialogEndKeyThenTypeAppends is the End variant of the collapse test:
// End clears the selection and moves the caret to the end so typing appends.
func TestRenameDialogEndKeyThenTypeAppends(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	if _, ok := selectionOf(box); !ok {
		t.Fatal("precondition: name should be selected on open")
	}
	typeDlgKey(box, tui.KeyEnd)
	if _, ok := selectionOf(box); ok {
		t.Error("selection still active after End; want it collapsed")
	}
	typeDlgRune(box, '!')
	submitDlg(box)

	if res.value != "My Session!" {
		t.Errorf("after End then '!' = %q, want %q (append)", res.value, "My Session!")
	}
}

// TestRenameDialogHomeKeyCollapsesAndPrepends checks Home: it collapses the
// selection and parks the caret at the start, so the next rune prepends. This
// guards the "arrow/Home/End collapses for in-place editing" half of the spec.
func TestRenameDialogHomeKeyCollapsesAndPrepends(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	if _, ok := selectionOf(box); !ok {
		t.Fatal("precondition: name should be selected on open")
	}
	typeDlgKey(box, tui.KeyHome)
	if _, ok := selectionOf(box); ok {
		t.Error("selection still active after Home; want it collapsed")
	}
	typeDlgRune(box, '>')
	submitDlg(box)

	if res.value != ">My Session" {
		t.Errorf("after Home then '>' = %q, want %q (prepend)", res.value, ">My Session")
	}
}

// TestRenameDialogLeftArrowCollapses checks Left collapses the selection too
// (landing the caret just before the last rune), so an in-place edit does not
// accidentally replace the whole name.
func TestRenameDialogLeftArrowCollapses(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "abc", true)

	if _, ok := selectionOf(box); !ok {
		t.Fatal("precondition: name should be selected on open")
	}
	// After select-all the caret is at len (3). Left moves it to 2, collapsing
	// the selection, so a rune inserts before the final 'c' -> "abXc".
	typeDlgKey(box, tui.KeyLeft)
	if _, ok := selectionOf(box); ok {
		t.Error("selection still active after Left; want it collapsed")
	}
	typeDlgRune(box, 'X')
	submitDlg(box)

	if res.value != "abXc" {
		t.Errorf("after Left then 'X' = %q, want %q", res.value, "abXc")
	}
}

// TestRenameDialogBackspaceClearsSelection verifies Backspace on the selected
// name deletes the whole selection (not a single trailing char), mirroring the
// standard select-then-delete affordance.
func TestRenameDialogBackspaceClearsSelection(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	typeDlgKey(box, tui.KeyBackspace)
	submitDlg(box)

	if !res.fired || !res.ok {
		t.Fatalf("dialog result = %+v, want accepted", res)
	}
	if res.value != "" {
		t.Errorf("after Backspace on full selection = %q, want empty", res.value)
	}
}

// TestRenameDialogDeleteClearsSelection is the Delete-key analogue: it removes
// the selected span wholesale.
func TestRenameDialogDeleteClearsSelection(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	typeDlgKey(box, tui.KeyDelete)
	submitDlg(box)

	if res.value != "" {
		t.Errorf("after Delete on full selection = %q, want empty", res.value)
	}
}

// TestRenameDialogEscapeCancels confirms cancel behaviour is unchanged: Escape
// dismisses the dialog with ok=false and an empty value, without applying.
func TestRenameDialogEscapeCancels(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	if consumed := typeDlgKey(box, tui.KeyEscape); !consumed {
		t.Fatal("Escape should be consumed by the dialog (cancel)")
	}
	if !res.fired {
		t.Fatal("Escape did not dismiss the dialog")
	}
	if res.ok {
		t.Error("Escape reported ok=true; want cancel (ok=false)")
	}
	if res.value != "" {
		t.Errorf("Escape value = %q, want empty", res.value)
	}
	if top := w.desktop.TopLayer(); top != nil && top.Name == "input-dialog" {
		t.Error("input-dialog layer still on top after Escape")
	}
}

// TestRenameDialogEnterWithSelectionCommitsFullName guards that an active
// selection does not block commit: Enter submits the field's full text (the
// selection spans text but does not remove it), so accepting immediately keeps
// the current name.
func TestRenameDialogEnterWithSelectionCommitsFullName(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	// Selection is still the whole name; Enter must commit it verbatim.
	if _, ok := selectionOf(box); !ok {
		t.Fatal("precondition: name should be selected on open")
	}
	submitDlg(box)

	if !res.fired || !res.ok {
		t.Fatalf("dialog result = %+v, want accepted", res)
	}
	if res.value != "My Session" {
		t.Errorf("Enter with selection = %q, want the full name %q", res.value, "My Session")
	}
}

// TestRenameDialogCtrlAReSelectsAfterCollapse checks the field's own Ctrl+A
// still works after the selection has been collapsed, so the user can re-arm a
// full replace mid-edit. This also exercises the same code path select-all on
// open uses, but driven by a "real" Ctrl+A event.
func TestRenameDialogCtrlAReSelectsAfterCollapse(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "My Session", true)

	typeDlgKey(box, tui.KeyRight) // collapse -> caret at end, no selection
	if _, ok := selectionOf(box); ok {
		t.Fatal("precondition: selection should be cleared after Right")
	}
	// Re-select all via the widget's public Ctrl+A handling.
	box.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: 'a', Ctrl: true})
	if sel, ok := selectionOf(box); !ok || sel != "My Session" {
		t.Fatalf("Ctrl+A re-selection = (%q,%v), want full name selected", sel, ok)
	}
	typeDlgRune(box, 'Z') // replaces the re-selected name
	submitDlg(box)

	if res.value != "Z" {
		t.Errorf("after Ctrl+A then 'Z' = %q, want %q", res.value, "Z")
	}
}

// TestRenameDialogUnicodeNameSelection confirms selection is rune-based, so a
// multi-byte name is selected and replaced as a whole (no off-by-one on bytes).
func TestRenameDialogUnicodeNameSelection(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "Café ☕ Résumé", true)

	sel, ok := selectionOf(box)
	if !ok || sel != "Café ☕ Résumé" {
		t.Fatalf("selection = (%q,%v), want the full unicode name", sel, ok)
	}
	typeDlgRune(box, 'x')
	submitDlg(box)

	if res.value != "x" {
		t.Errorf("after typing 'x' over unicode name = %q, want %q", res.value, "x")
	}
}

// TestRenameDialogEmptyNameIsNoOp covers the empty-initial edge case: select-all
// on no content is inert (an empty, zero-width selection), so the first rune
// simply inserts rather than misbehaving. This documents the implementation's
// "empty text is a no-op" guarantee.
func TestRenameDialogEmptyNameIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "", true)

	if _, ok := selectionOf(box); ok {
		t.Error("empty field should have no selection, got one")
	}
	typeDlgRune(box, 'x')
	submitDlg(box)

	if res.value != "x" {
		t.Errorf("after typing 'x' into empty field = %q, want %q", res.value, "x")
	}
}

// TestRenameDialogSingleCharName selects and replaces a one-rune name — the
// boundary where selAnchor (0) and the caret (len==1) are closest without the
// selection collapsing.
func TestRenameDialogSingleCharName(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "S", true)

	if sel, ok := selectionOf(box); !ok || sel != "S" {
		t.Fatalf("selection = (%q,%v), want the single char selected", sel, ok)
	}
	typeDlgRune(box, 'Z')
	submitDlg(box)

	if res.value != "Z" {
		t.Errorf("after typing 'Z' over single-char name = %q, want %q", res.value, "Z")
	}
}

// --- Dialogs that must NOT select-all (the withSelectAll opt is off) ----------

// TestInputDialogWithoutOptionHasNoSelection is the contract test for the other
// callers: with no option, showInputDialog leaves the caret at the end and
// nothing selected, so typing inserts (appends) instead of replacing.
func TestInputDialogWithoutOptionHasNoSelection(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "existing query", false)

	if _, ok := selectionOf(box); ok {
		t.Error("plain dialog opened with a selection; want caret-at-end, no selection")
	}
	typeDlgRune(box, '!')
	submitDlg(box)

	if res.value != "existing query!" {
		t.Errorf("plain dialog after '!' = %q, want %q (append, not replace)", res.value, "existing query!")
	}
}

// TestGoalDialogDoesNotSelectAll verifies editActiveGoal (Session Goal) is
// unchanged by issue #235: it passes no option, so the goal is not selected and
// typing appends to it rather than replacing it.
func TestGoalDialogDoesNotSelectAll(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.goal = "ship it"

	w.editActiveGoal()
	box := inputDialogBox(t, w)

	if _, ok := selectionOf(box); ok {
		t.Error("Session Goal dialog opened with a selection; want no select-all (unchanged)")
	}
	// The field is pre-filled with the current goal and caret at the end.
	typeDlgRune(box, '!')
	submitDlg(box)

	if sw.goal != "ship it!" {
		t.Errorf("goal after append = %q, want %q", sw.goal, "ship it!")
	}
}

// TestFindDialogDoesNotSelectAll verifies promptFind (Find in Transcript) is
// unchanged: no select-all, so refining a search appends rather than replacing.
func TestFindDialogDoesNotSelectAll(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	sw.transcript.setQuery("error")

	sw.promptFind()
	box := inputDialogBox(t, w)

	if _, ok := selectionOf(box); ok {
		t.Error("Find in Transcript dialog opened with a selection; want no select-all (unchanged)")
	}
	typeDlgRune(box, 's')
	submitDlg(box)

	if got := sw.transcript.query; got != "errors" {
		t.Errorf("find query after append = %q, want %q", got, "errors")
	}
}

// --- Integration through RenameSession ---------------------------------------

// TestRenameSessionIntegrationReplacesTitle drives the whole rename path:
// RenameSession opens the select-all dialog, typing replaces, Enter commits via
// SetSessionTitle, and the new title lands on the session window.
func TestRenameSessionIntegrationReplacesTitle(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "Old Title")

	w.RenameSession("s")
	box := inputDialogBox(t, w)

	if sel, ok := selectionOf(box); !ok || sel != "Old Title" {
		t.Fatalf("rename dialog selection = (%q,%v), want full title", sel, ok)
	}
	for _, r := range "New Title" {
		typeDlgRune(box, r)
	}
	submitDlg(box)

	sw := w.sessions["s"]
	if sw.title != "New Title" {
		t.Errorf("session title after rename = %q, want %q", sw.title, "New Title")
	}
	if sw.window.Title != "New Title" {
		t.Errorf("window title after rename = %q, want %q", sw.window.Title, "New Title")
	}
}

// TestRenameSessionIntegrationAppendsAfterArrow checks the arrow-collapse path
// end-to-end: the user collapses the selection and appends, and the resulting
// (non-empty) title is applied.
func TestRenameSessionIntegrationAppendsAfterArrow(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "Draft")

	w.RenameSession("s")
	box := inputDialogBox(t, w)

	typeDlgKey(box, tui.KeyEnd) // collapse, caret at end
	typeDlgRune(box, '!')
	submitDlg(box)

	if w.sessions["s"].title != "Draft!" {
		t.Errorf("title after End+! = %q, want %q", w.sessions["s"].title, "Draft!")
	}
}

// TestRenameSessionIntegrationBlankNotApplied confirms the cancel/empty guard in
// RenameSession's callback still holds with select-all on: clearing the name
// (Backspace on the selection) and committing does not wipe the title.
func TestRenameSessionIntegrationBlankNotApplied(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "Keep Me")

	w.RenameSession("s")
	box := inputDialogBox(t, w)

	typeDlgKey(box, tui.KeyBackspace) // delete the whole selected name -> empty
	submitDlg(box)

	if w.sessions["s"].title != "Keep Me" {
		t.Errorf("blank rename wiped title = %q, want %q", w.sessions["s"].title, "Keep Me")
	}
}

// TestRenameSessionIntegrationEscapeCancels checks Escape on the rename dialog
// leaves the title untouched.
func TestRenameSessionIntegrationEscapeCancels(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "Keep Me")

	w.RenameSession("s")
	box := inputDialogBox(t, w)

	typeDlgKey(box, tui.KeyEscape)

	if w.sessions["s"].title != "Keep Me" {
		t.Errorf("Escape changed title = %q, want %q", w.sessions["s"].title, "Keep Me")
	}
}

// TestRenameSessionUnknownIDIsNoOp verifies RenameSession on a missing session
// is a safe no-op (no dialog, no panic) — error handling on the entry path. The
// sidebar is always present as a layer, so the check is "no input-dialog on top"
// rather than "no layer at all".
func TestRenameSessionUnknownIDIsNoOp(t *testing.T) {
	w := newTestWorkbench(t)
	w.RenameSession("does-not-exist")
	if top := w.desktop.TopLayer(); top != nil && top.Name == "input-dialog" {
		t.Errorf("rename of unknown id opened an input-dialog; want no dialog")
	}
}

// --- Option + helper unit tests ---------------------------------------------

// TestWithSelectAllOptionSetsConfig checks the option mutates the config and
// that the zero config (no option) defaults to select-all off — the basis for
// the other two dialogs staying edit-in-place.
func TestWithSelectAllOptionSetsConfig(t *testing.T) {
	var cfg inputDialogConfig
	if cfg.selectAll {
		t.Error("zero config should default to select-all off")
	}
	withSelectAll()(&cfg)
	if !cfg.selectAll {
		t.Error("withSelectAll() did not set selectAll=true")
	}
}

// TestSelectAllTextBoxGuardsNoPanic covers the defensive guards in
// selectAllTextBox: a nil box, a box with no component, and a box whose
// component has no type handler must all be no-ops rather than panics.
func TestSelectAllTextBoxGuardsNoPanic(t *testing.T) {
	t.Run("nil box", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("selectAllTextBox(nil) panicked: %v", r)
			}
		}()
		selectAllTextBox(nil)
	})
	t.Run("nil component", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("selectAllTextBox on nil component panicked: %v", r)
			}
		}()
		selectAllTextBox(&tv.TextBox{}) // Component is nil
	})
	t.Run("real box selects all", func(t *testing.T) {
		// A fully wired box: selectAllTextBox should highlight the contents.
		box := tv.NewTextBox("hello", tv.Rect{W: 10, H: 1})
		selectAllTextBox(box)
		if got, ok := box.Component.Copy(); !ok || got != "hello" {
			t.Errorf("selectAllTextBox selection = (%q,%v), want full text", got, ok)
		}
	})
}

// TestShowInputDialogAcceptsNoOptions confirms the variadic option keeps the
// original call shape compiling and behaving as a plain prompt when no option is
// passed (the regression boundary for the API change).
func TestShowInputDialogAcceptsNoOptions(t *testing.T) {
	w := newTestWorkbench(t)
	res := &inputResult{}
	// No opts at all — must compile and behave as edit-in-place.
	w.showInputDialog("Plain", "&F:", "seed", func(value string, ok bool) {
		res.value = value
		res.ok = ok
		res.fired = true
	})
	box := inputDialogBox(t, w)
	if _, ok := selectionOf(box); ok {
		t.Error("no-option dialog opened with a selection; want plain edit-in-place")
	}
	typeDlgRune(box, '!')
	submitDlg(box)
	if res.value != "seed!" {
		t.Errorf("no-option dialog after '!' = %q, want %q", res.value, "seed!")
	}
}

// --- Focus target + further edge cases ---------------------------------------

// TestRenameDialogFocusTargetsEditableField documents and guards that on open the
// keyboard focus is on the editable text field — not the OK or Cancel button — so
// the first keystroke lands in the name. The field is the only focusable dialog
// child exposing a clipboard-copy capability, which inputDialogBox relies on.
func TestRenameDialogFocusTargetsEditableField(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	w.RenameSession("s")

	box := inputDialogBox(t, w) // asserts CopyFn != nil internally
	if !box.Focused() {
		t.Error("editable field is not focused on open")
	}
	if box.CopyFn == nil {
		t.Error("focused widget lacks CopyFn; focus is not on the editable field")
	}
}

// TestRenameDialogSelectsUntrimmedTitle guards that select-all spans the RAW title
// (RenameSession passes sw.title untrimmed), including any surrounding whitespace,
// so the first keystroke wipes the whole thing — not just the trimmed core.
func TestRenameDialogSelectsUntrimmedTitle(t *testing.T) {
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, "  Padded  ", true)

	sel, ok := selectionOf(box)
	if !ok || sel != "  Padded  " {
		t.Fatalf("selection = (%q,%v), want the full untrimmed title", sel, ok)
	}
	typeDlgRune(box, 'x')
	submitDlg(box)
	if res.value != "x" {
		t.Errorf("after typing over untrimmed title = %q, want %q", res.value, "x")
	}
}

// TestRenameDialogLongTitleSelectsAll checks select-all still spans a title longer
// than the visible field width: the selection anchor/caret are rune-indexed, so a
// replacing keystroke deletes the whole over-width name, not just the visible run.
func TestRenameDialogLongTitleSelectsAll(t *testing.T) {
	long := strings.Repeat("name-", 20) // 100 runes >> field width (~50)
	w := newTestWorkbench(t)
	box, res := openDialog(t, w, long, true)

	sel, ok := selectionOf(box)
	if !ok || sel != long {
		t.Fatalf("selection length = %d (ok=%v), want the full %d-rune title", len([]rune(sel)), ok, len([]rune(long)))
	}
	typeDlgRune(box, 'Z')
	submitDlg(box)
	if res.value != "Z" {
		t.Errorf("after typing over long title = %q, want %q", res.value, "Z")
	}
}
