package ui

import (
	"fmt"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// keybindCustomizerIdleHint is the status line shown while browsing the customizer
// (issue #269): it names every gesture so the dialog is self-documenting.
const keybindCustomizerIdleHint = "Enter: rebind · Reset / Reset All · ↑↓ move · Esc close"

// keybindRowText renders one action row for the customizer list: the action name, its
// current binding, and a (default)/(custom)/(unbound) tag distinguishing a binding left
// at its catalog default, one the user has overridden, and one the user has cleared
// (issue #269). The columns line up so the bindings and tags read as tidy columns,
// mirroring the command palette's rows.
func (w *Workbench) keybindRowText(a keybindAction) string {
	tag := "default"
	switch {
	case w.isUnbound(a.id):
		tag = "unbound"
	case w.isOverridden(a.id):
		tag = "custom"
	}
	return "  " + padName(a.name, 26) + "  " + padName(chordLabel(w.chordFor(a.id)), 10) + " (" + tag + ")"
}

// capturePrompt is the status line shown while waiting for the user to press the new
// chord for an action (issue #269's capture mode).
func capturePrompt(a keybindAction) string {
	return fmt.Sprintf("Press a key for %q…  (Esc cancel · Backspace clear)", a.name)
}

// selectedKeybindAction returns the catalog action under the list's current selection,
// or nil when the selection is on a category header (a non-action row) or the list is
// empty. It is how the capture and reset gestures resolve which action they act on.
func selectedKeybindAction(list *tv.Tree) *keybindAction {
	n := list.Selected()
	if n == nil {
		return nil
	}
	a, ok := n.Data.(keybindAction)
	if !ok {
		return nil
	}
	return &a
}

// showKeybindingCustomizer opens the modal keybinding customizer (issue #269, phase 4b):
// a category-grouped list of every rebindable action showing "name … binding (tag)".
// Selecting a row (Enter) enters capture mode — the next chord becomes the new binding,
// after the deliverability, scope-rule, self-lockout and same-scope conflict checks.
// Every committed change applies to the live registry at once (so the new key works
// immediately and the cheatsheet/palette reflect it) and is persisted through the
// SetKeybindings handler. "Reset" restores the selected action's default; "Reset All"
// restores every default.
func (w *Workbench) showKeybindingCustomizer() {
	actions := keybindActions()
	categories := 0
	seen := map[string]bool{}
	for _, a := range actions {
		if !seen[a.category] {
			seen[a.category] = true
			categories++
		}
	}
	// Content-driven height: one row per action plus one per category header, capped so a
	// short catalog does not balloon on a roomy terminal (mirrors the palette/help specs).
	rows := len(actions) + categories
	spec := tv.DialogSpec{MinW: 58, MinH: 16, MaxH: rows + 9}
	x, y, width, height := w.dialogRect(spec)

	var layer *tv.Layer
	closeFn := func() { w.desktop.RemoveLayer(layer) }
	dialog := newCloseableDialog("Customize Keybindings", x, y, width, height, closeFn)

	dialog.Window.AddContent(dialogLabel("Select an action and press Enter to rebind it.",
		tv.Rect{X: 2, Y: 1, W: width - 4, H: 1}))

	listY := 3
	listH := height - listY - 4 // status row + button row + bottom margin + border
	if listH < 3 {
		listH = 3
	}
	list := tv.NewTree(tv.Rect{X: 2, Y: listY, W: width - 4, H: listH})
	list.FG = tv.DefaultTheme.ListFG
	list.BG = tv.DefaultTheme.ListBG
	list.SelFG, list.SelBG = selectionColorsFor(
		tv.DefaultTheme.ListFG, tv.DefaultTheme.ListBG,
		tv.DefaultTheme.SelectionFG, tv.DefaultTheme.SelectionBG)
	dialog.Window.AddContent(list)

	status := dialogLabel(keybindCustomizerIdleHint, tv.Rect{X: 2, Y: height - 3, W: width - 4, H: 1})
	dialog.Window.AddContent(status)
	setStatus := func(msg string) {
		status.SetText(msg)
		w.desktop.Redraw()
	}

	// render rebuilds the grouped list from the live bindings, so it reflects every
	// committed rebind/reset. Category headers are non-selectable rows (nil Data) drawn
	// at the left margin; action rows are indented and carry their keybindAction in Data.
	render := func() {
		var nodes []*tv.TreeNode
		cur := ""
		for _, a := range actions {
			if a.category != cur {
				nodes = append(nodes, tv.NewTreeNode(a.category))
				cur = a.category
			}
			n := tv.NewTreeNode(w.keybindRowText(a))
			n.Data = a
			nodes = append(nodes, n)
		}
		list.Roots = nodes
		w.desktop.Redraw()
	}

	// capturing names the action awaiting a new chord, or nil while browsing. It is the
	// single piece of mode state the list's key handler branches on.
	var capturing *keybindAction

	// commit runs the full validation/confirm pipeline for a captured chord and, on
	// success, applies it live and persists it. The order is: deliverability + scope
	// (refuse outright), then the self-lockout confirm (a path to the customizer), then
	// the same-scope conflict confirm (reassign via a lossless swap), then the plain
	// apply.
	commit := func(a keybindAction, chord tv.Chord) {
		if ok, reason := w.validateCapture(a, chord); !ok {
			setStatus("✗ " + reason)
			return
		}
		apply := func() {
			if holder, ok := w.conflictHolder(a, chord); ok {
				w.showConfirm("Keybinding conflict",
					fmt.Sprintf("⚠ %s is already bound to %q.\n\nReassign it to %q (and give %q its old key)?",
						displayChord(chord), keybindActionName(holder), a.name, keybindActionName(holder)),
					func(yes bool) {
						if !yes {
							setStatus("Reassign cancelled.")
							return
						}
						w.swapBindings(a, holder, chord)
						w.persistKeybindings()
						render()
						setStatus(fmt.Sprintf("%s → %s (swapped with %q).", a.name, displayChord(chord), keybindActionName(holder)))
					})
				return
			}
			// No conflict (checked above): a plain force-set applies it live in every
			// open window and records the override.
			w.applyBinding(a.id, chord)
			w.persistKeybindings()
			render()
			setStatus(fmt.Sprintf("%s → %s.", a.name, displayChord(chord)))
		}
		if isEscapeHatch(a.id) {
			w.showConfirm("Self-lockout warning",
				fmt.Sprintf("%q is a keyboard path to this customizer.\n\nRebinding it to %s could make the customizer harder to reach.\nContinue?",
					a.name, displayChord(chord)),
				func(yes bool) {
					if !yes {
						setStatus("Rebind cancelled.")
						return
					}
					apply()
				})
			return
		}
		apply()
	}

	// clearBinding unbinds an action from capture mode (Backspace). Clearing an
	// escape-hatch action (the '?'/':' paths to this customizer) is confirmed first, so
	// the user can't silently remove their only keyboard route back here (self-lockout).
	clearBinding := func(a keybindAction) {
		do := func() {
			if w.isUnbound(a.id) {
				setStatus(fmt.Sprintf("%q is already unbound.", a.name))
				return
			}
			w.clearBinding(a.id)
			w.persistKeybindings()
			render()
			setStatus(fmt.Sprintf("%q cleared (unbound).", a.name))
		}
		if isEscapeHatch(a.id) {
			w.showConfirm("Self-lockout warning",
				fmt.Sprintf("%q is a keyboard path to this customizer.\n\nClearing it removes that path. Continue?", a.name),
				func(yes bool) {
					if !yes {
						setStatus("Clear cancelled.")
						return
					}
					do()
				})
			return
		}
		do()
	}

	startCapture := func() {
		if capturing != nil {
			return
		}
		a := selectedKeybindAction(list)
		if a == nil {
			return
		}
		w.desktop.SetFocus(list)
		capturing = a
		setStatus(capturePrompt(*a))
	}

	// The list owns key handling: while capturing it swallows the next chord (Esc
	// cancels, Backspace re-arms); otherwise Enter starts a capture and Esc closes, with
	// every other key left to the tree for navigation.
	baseType := list.Component.OnTypeFn
	list.Component.OnTypeFn = func(c *tv.VisualComponent, ev tui.TypeEvent) bool {
		if capturing != nil {
			switch ev.Key {
			case tui.KeyEscape:
				a := *capturing
				capturing = nil
				setStatus(fmt.Sprintf("Rebind of %q cancelled.", a.name))
				return true
			case tui.KeyBackspace:
				// "Clear" the action: unbind it entirely (no key fires it, not even its
				// old default), then leave capture. Capture commits on the first chord, so
				// there is no pending buffer to erase — a true unbind is the meaning of
				// "Backspace clear" the prompt promises (issue #269).
				a := *capturing
				capturing = nil
				clearBinding(a)
				return true
			}
			a := *capturing
			capturing = nil
			commit(a, chordFromEvent(ev))
			return true
		}
		switch ev.Key {
		case tui.KeyEscape:
			closeFn()
			return true
		case tui.KeyEnter:
			startCapture()
			return true
		}
		if baseType != nil {
			return baseType(c, ev)
		}
		return false
	}
	list.OnActivate = func(*tv.TreeNode) { startCapture() }

	resetSelected := func() {
		if capturing != nil {
			return
		}
		a := selectedKeybindAction(list)
		if a == nil {
			return
		}
		if !w.isOverridden(a.id) {
			setStatus(fmt.Sprintf("%s is already at its default (%s).", a.name, displayChord(a.deflt)))
			return
		}
		if !w.resetBinding(a.id) {
			holder, _ := w.conflictHolder(*a, a.deflt)
			setStatus(fmt.Sprintf("Can't reset %s: default %s is in use by %q.", a.name, displayChord(a.deflt), keybindActionName(holder)))
			return
		}
		w.persistKeybindings()
		render()
		setStatus(fmt.Sprintf("%s reset to %s.", a.name, displayChord(a.deflt)))
	}

	resetAll := func() {
		if capturing != nil {
			return
		}
		w.resetAllBindings()
		w.persistKeybindings()
		render()
		setStatus("All keybindings reset to their defaults.")
	}

	labels := []string{"&Reset", "Reset &All", "Close"}
	rects := footerButtonRects(labels, 2, width-3, height-2, 2)
	dialog.Window.AddContent(newButton(labels[0], rects[0], resetSelected))
	dialog.Window.AddContent(newButton(labels[1], rects[1], resetAll))
	dialog.Window.AddContent(newButton(labels[2], rects[2], closeFn))

	// Backup Esc handler for when focus rests on a footer button rather than the list;
	// it never closes mid-capture (the list owns that).
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, ev tui.TypeEvent) bool {
		if capturing != nil {
			return false
		}
		if ev.Key == tui.KeyEscape {
			closeFn()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("keybinding-customizer", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec) // re-resolve the rect when the terminal is resized (issue #299)
	render()
	w.desktop.SetFocus(list)
}
