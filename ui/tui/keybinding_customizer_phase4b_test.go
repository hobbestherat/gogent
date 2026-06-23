package ui

import (
	"sort"
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func focusedComponent(root *tv.VisualComponent) *tv.VisualComponent {
	if root == nil {
		return nil
	}
	if root.Focused() {
		return root
	}
	for _, child := range root.Children() {
		if found := focusedComponent(child); found != nil {
			return found
		}
	}
	return nil
}

func focusableComponents(root *tv.VisualComponent) []*tv.VisualComponent {
	if root == nil || !root.Visible || !root.Enabled {
		return nil
	}
	var out []*tv.VisualComponent
	if root.Focusable {
		out = append(out, root)
	}
	for _, child := range root.Children() {
		out = append(out, focusableComponents(child)...)
	}
	return out
}

func focusableByBottomRowAndX(t *testing.T, w *Workbench, index int) *tv.VisualComponent {
	t.Helper()
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		t.Fatal("no top layer")
	}
	items := focusableComponents(top.Root)
	if len(items) == 0 {
		t.Fatal("top layer has no focusable components")
	}
	maxY := items[0].Bounds.Y
	for _, item := range items[1:] {
		if item.Bounds.Y > maxY {
			maxY = item.Bounds.Y
		}
	}
	var bottom []*tv.VisualComponent
	for _, item := range items {
		if item.Bounds.Y == maxY {
			bottom = append(bottom, item)
		}
	}
	sort.Slice(bottom, func(i, j int) bool { return bottom[i].Bounds.X < bottom[j].Bounds.X })
	if index < 0 || index >= len(bottom) {
		t.Fatalf("bottom-row focusable index %d out of %d", index, len(bottom))
	}
	return bottom[index]
}

func pressBottomButton(t *testing.T, w *Workbench, index int) {
	t.Helper()
	button := focusableByBottomRowAndX(t, w, index)
	w.desktop.SetFocus(button)
	if !button.BubbleType(tui.TypeEvent{Key: tui.KeyEnter}) {
		t.Fatalf("bottom button %d did not consume Enter", index)
	}
}

func topFocusedComponent(w *Workbench) *tv.VisualComponent {
	top := w.desktop.TopLayer()
	if top == nil {
		return nil
	}
	return focusedComponent(top.Root)
}

func typeFocused(w *Workbench, ev tui.TypeEvent) bool {
	focused := topFocusedComponent(w)
	if focused == nil {
		return false
	}
	return focused.BubbleType(ev)
}

func selectCustomizerRow(t *testing.T, w *Workbench, visibleRow int) {
	t.Helper()
	for i := 0; i < visibleRow; i++ {
		if !typeFocused(w, tui.TypeEvent{Key: tui.KeyDown}) {
			t.Fatalf("customizer row navigation stopped at step %d toward row %d", i, visibleRow)
		}
	}
}

func pressConfirm(t *testing.T, w *Workbench, yes bool) {
	t.Helper()
	top := w.desktop.TopLayer()
	if top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("top layer = %v, want confirm-dialog", top)
	}
	if yes {
		pressBottomButton(t, w, 0)
		return
	}
	pressBottomButton(t, w, 1)
}

func chordForAction(t *testing.T, w *Workbench, id tv.ActionID) tv.Chord {
	t.Helper()
	b, ok := w.desktop.ScopedBindings().BindingFor(id)
	if !ok {
		t.Fatalf("no live binding for %q", id)
	}
	return b.Chord
}

func assertChord(t *testing.T, got tv.Chord, want tv.Chord) {
	t.Helper()
	if !sameChord(got, want) {
		t.Fatalf("chord = %+v (%s), want %+v (%s)", got, displayChord(got), want, displayChord(want))
	}
}

func TestKeybindingCustomizerDiscoverabilityAndRows(t *testing.T) {
	w := newTestWorkbench(t)

	c, ok := findCommand(w.commands(), "Customize keybindings")
	if !ok {
		t.Fatal("command palette is missing Customize keybindings")
	}
	if c.category != "App" || c.run == nil {
		t.Fatalf("Customize keybindings command = %+v, want App runnable command", c)
	}

	w.showHelpOverlay()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "help-overlay" {
		t.Fatalf("top layer after help = %v, want help-overlay", top)
	}
	// The rooted entry is the left footer button before Close; pressing it should
	// close help and open the real customizer.
	pressBottomButton(t, w, 0)
	if top := w.desktop.TopLayer(); top == nil || top.Name != "keybinding-customizer" {
		t.Fatalf("top layer after Customize = %v, want keybinding-customizer", top)
	}

	rowDefault := w.keybindRowText(keybindActions()[2]) // Toggle messages.
	for _, want := range []string{"Toggle messages", "a", "(default)"} {
		if !strings.Contains(rowDefault, want) {
			t.Fatalf("default row %q missing %q", rowDefault, want)
		}
	}
	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'm'})
	rowCustom := w.keybindRowText(keybindActions()[2])
	for _, want := range []string{"Toggle messages", "m", "(custom)"} {
		if !strings.Contains(rowCustom, want) {
			t.Fatalf("custom row %q missing %q", rowCustom, want)
		}
	}
}

func TestKeybindingCustomizerCaptureAppliesLivePersistsAndUpdatesDisplay(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }

	w.showKeybindingCustomizer()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "keybinding-customizer" {
		t.Fatalf("top layer after customizer = %v, want keybinding-customizer", top)
	}
	selectCustomizerRow(t, w, 3) // Transcript header, Find, Esc, Toggle messages.
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter}) {
		t.Fatal("Enter did not enter capture mode")
	}
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyRune, Rune: 'm'}) {
		t.Fatal("captured key was not consumed")
	}

	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'm'})
	if got := persisted.Overrides[string(actionTranscriptToggleMsg)]; got != "M" {
		t.Fatalf("persisted override = %q, want M in %+v", got, persisted)
	}
	c, ok := findCommand(w.commands(), "Toggle messages")
	if !ok {
		t.Fatal("Toggle messages command missing")
	}
	if c.keys != "m" {
		t.Fatalf("updated command display = %q, want m", c.keys)
	}
}

func TestKeybindingCustomizerConflictCancelLeavesBindings(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")

	w.showKeybindingCustomizer()
	selectCustomizerRow(t, w, 3) // Toggle messages.
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyRune, Rune: 'r'}) // Toggle thinking's chord.
	if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("top layer = %v, want conflict confirm-dialog", top)
	}
	pressConfirm(t, w, false)

	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'a'})
	assertChord(t, chordForAction(t, w, actionTranscriptToggleThink), tv.Chord{Rune: 'r'})
}

func TestKeybindingCustomizerConflictConfirmSwapsBindings(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }

	w.showKeybindingCustomizer()
	selectCustomizerRow(t, w, 3) // Toggle messages.
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyRune, Rune: 'r'}) // Toggle thinking's chord.
	pressConfirm(t, w, true)

	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'r'})
	assertChord(t, chordForAction(t, w, actionTranscriptToggleThink), tv.Chord{Rune: 'a'})
	if got := persisted.Overrides[string(actionTranscriptToggleMsg)]; got != "R" {
		t.Fatalf("message override = %q, want R in %+v", got, persisted)
	}
	if got := persisted.Overrides[string(actionTranscriptToggleThink)]; got != "A" {
		t.Fatalf("thinking override = %q, want A in %+v", got, persisted)
	}
}

func TestKeybindingCustomizerRejectsUndeliverableChord(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")

	w.showKeybindingCustomizer()
	selectCustomizerRow(t, w, 3) // Toggle messages.
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyRune, Rune: 'm', Ctrl: true})

	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'a'})
	if w.isOverridden(actionTranscriptToggleMsg) {
		t.Fatal("undeliverable capture recorded an override")
	}
	ok, reason := w.validateCapture(keybindActions()[2], tv.Chord{Rune: 'm', Ctrl: true})
	if ok || strings.TrimSpace(reason) == "" {
		t.Fatalf("validateCapture(Ctrl+M) = %v,%q, want rejection with reason", ok, reason)
	}
}

func TestKeybindingCustomizerResetSelectedAndResetAll(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }

	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'm'})
	w.applyBinding(actionTranscriptToggleThink, tv.Chord{Rune: 'q'})
	w.showKeybindingCustomizer()
	selectCustomizerRow(t, w, 3) // Toggle messages.
	pressBottomButton(t, w, 0)
	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'a'})
	assertChord(t, chordForAction(t, w, actionTranscriptToggleThink), tv.Chord{Rune: 'q'})
	if _, ok := persisted.Overrides[string(actionTranscriptToggleMsg)]; ok {
		t.Fatalf("per-row reset still persisted message override: %+v", persisted)
	}

	pressBottomButton(t, w, 1)
	assertChord(t, chordForAction(t, w, actionTranscriptToggleThink), tv.Chord{Rune: 'r'})
	if len(persisted.Overrides) != 0 {
		t.Fatalf("reset-all persisted overrides = %+v, want empty", persisted)
	}
}

func TestKeybindingsConfigRoundTripAndLoadAppliesRegistry(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	w.applyBinding(actionHelpOverlay, tv.Chord{Key: tui.KeyF2})
	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'm'})

	cfg := w.buildKeybindingsConfig()
	if got := cfg.Overrides[string(actionHelpOverlay)]; got != "F2" {
		t.Fatalf("help override = %q, want F2 in %+v", got, cfg)
	}
	if got := cfg.Overrides[string(actionTranscriptToggleMsg)]; got != "M" {
		t.Fatalf("message override = %q, want M in %+v", got, cfg)
	}

	reloaded := newTestWorkbench(t)
	reloaded.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
		string(actionHelpOverlay):          "F2",
		string(actionTranscriptToggleMsg):  "M",
		"unknown.action":                   "F3",
		string(actionTranscriptToggleTool): "not-a-key",
	}})
	reloaded.openWindow("s", "S")

	assertChord(t, chordForAction(t, reloaded, actionHelpOverlay), tv.Chord{Key: tui.KeyF2})
	assertChord(t, chordForAction(t, reloaded, actionTranscriptToggleMsg), tv.Chord{Rune: 'm'})
	assertChord(t, chordForAction(t, reloaded, actionTranscriptToggleTool), tv.Chord{Rune: 't'})
}

func TestKeybindingScopeRulesRejectPlainGlobalButAllowScopedPlainKeys(t *testing.T) {
	if ok, reason := validateScopeRule(tv.ScopeGlobal, tv.Chord{Rune: 'x'}); ok || !strings.Contains(reason, "plain key") {
		t.Fatalf("plain global validation = %v,%q, want plain-key rejection", ok, reason)
	}
	for _, scope := range []tv.Scope{tv.ScopeFocus, tv.ScopeFallthrough} {
		if ok, reason := validateScopeRule(scope, tv.Chord{Rune: 'x'}); !ok {
			t.Fatalf("plain key in scope %v rejected: %q", scope, reason)
		}
	}
	if ok, reason := validateScopeRule(tv.ScopeGlobal, tv.Chord{Rune: 'x', Ctrl: true}); !ok {
		t.Fatalf("Ctrl global rejected: %q", reason)
	}
	if ok, reason := validateScopeRule(tv.ScopeGlobal, tv.Chord{Key: tui.KeyF5}); !ok {
		t.Fatalf("function-key global rejected: %q", reason)
	}
}

func TestKeybindingSelfLockoutRequiresConfirmation(t *testing.T) {
	w := newTestWorkbench(t)

	w.showKeybindingCustomizer()
	selectCustomizerRow(t, w, 11) // App / Keybinding help.
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyF2})
	if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("top layer after help rebind = %v, want self-lockout confirm-dialog", top)
	}
	pressConfirm(t, w, false)
	assertChord(t, chordForAction(t, w, actionHelpOverlay), tv.Chord{Rune: '?'})

	w.showKeybindingCustomizer()
	selectCustomizerRow(t, w, 11) // App / Keybinding help.
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyF2})
	pressConfirm(t, w, true)
	assertChord(t, chordForAction(t, w, actionHelpOverlay), tv.Chord{Key: tui.KeyF2})
}
