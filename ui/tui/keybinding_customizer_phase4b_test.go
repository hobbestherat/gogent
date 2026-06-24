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

func customizerRowForAction(t *testing.T, w *Workbench, id tv.ActionID) int {
	t.Helper()
	row := -1
	category := ""
	for _, a := range w.rebindable() {
		if a.category != category {
			category = a.category
			row++
		}
		row++
		if a.actionID == id {
			return row
		}
	}
	t.Fatalf("action %q not found in rebindable catalog", id)
	return 0
}

func selectCustomizerAction(t *testing.T, w *Workbench, id tv.ActionID) {
	t.Helper()
	selectCustomizerRow(t, w, customizerRowForAction(t, w, id))
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

	toggleMsg, ok := w.actionByID(actionTranscriptToggleMsg)
	if !ok {
		t.Fatal("Toggle messages action missing")
	}
	rowDefault := w.keybindRowText(toggleMsg)
	for _, want := range []string{"Toggle messages", "a", "(default)"} {
		if !strings.Contains(rowDefault, want) {
			t.Fatalf("default row %q missing %q", rowDefault, want)
		}
	}
	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'm'})
	rowCustom := w.keybindRowText(toggleMsg)
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
	selectCustomizerAction(t, w, actionTranscriptToggleMsg)
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

func TestKeybindingRebindAppliesToEveryOpenTranscriptWindow(t *testing.T) {
	w := newTestWorkbench(t)
	w1 := w.openWindow("s1", "S1")
	w2 := w.openWindow("s2", "S2")

	w.applyBinding(actionTranscriptToggleMsg, tv.Chord{Rune: 'm'})

	if _, fired := dispatchAtFocus(w, w1.history.Component, runeEv('m')); !fired {
		t.Fatal("new binding did not fire in first open transcript window")
	}
	if w1.transcript.hidden&kindAssistant.bit() == 0 {
		t.Fatal("first window did not toggle messages on new binding")
	}
	if _, fired := dispatchAtFocus(w, w2.history.Component, runeEv('m')); !fired {
		t.Fatal("new binding did not fire in second open transcript window")
	}
	if w2.transcript.hidden&kindAssistant.bit() == 0 {
		t.Fatal("second window did not toggle messages on new binding")
	}

	before := w2.transcript.hidden
	if _, fired := dispatchAtFocus(w, w2.history.Component, runeEv('a')); fired {
		t.Fatal("old binding still fired in second open transcript window after live rebind")
	}
	if w2.transcript.hidden != before {
		t.Fatal("old binding changed second window state after live rebind")
	}
}

func TestCommandPaletteDisplayTracksReboundPaletteBinding(t *testing.T) {
	w := newTestWorkbench(t)
	w.applyBinding(actionCommandPalette, tv.Chord{Key: tui.KeyF3})
	if !w.desktop.ScopedBindings().DispatchFallthrough(tui.TypeEvent{Key: tui.KeyF3}) {
		t.Fatal("rebuilt command-palette binding does not dispatch from registry")
	}

	c, ok := findCommand(w.commands(), "Command palette")
	if !ok {
		t.Fatal("Command palette command missing")
	}
	if !strings.Contains(c.keys, "F3") || strings.Contains(c.keys, ":") {
		t.Fatalf("Command palette display = %q, want rebound F3 and no stale ':' hint", c.keys)
	}
}

func TestKeybindingCustomizerConflictCancelLeavesBindings(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionTranscriptToggleMsg)
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
	selectCustomizerAction(t, w, actionTranscriptToggleMsg)
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
	selectCustomizerAction(t, w, actionTranscriptToggleMsg)
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyRune, Rune: 'm', Ctrl: true})

	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'a'})
	if w.isOverridden(actionTranscriptToggleMsg) {
		t.Fatal("undeliverable capture recorded an override")
	}
	toggleMsg, okAction := w.actionByID(actionTranscriptToggleMsg)
	if !okAction {
		t.Fatal("Toggle messages action missing")
	}
	ok, reason := w.validateCapture(toggleMsg, tv.Chord{Rune: 'm', Ctrl: true})
	if ok || strings.TrimSpace(reason) == "" {
		t.Fatalf("validateCapture(Ctrl+M) = %v,%q, want rejection with reason", ok, reason)
	}
}

func TestKeybindingCustomizerBackspaceClearsBinding(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionTranscriptToggleMsg)
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter}) {
		t.Fatal("Enter did not enter capture mode")
	}
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyBackspace}) {
		t.Fatal("Backspace clear was not consumed")
	}

	if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('a')); fired {
		t.Fatal("cleared Toggle messages binding still fires on its old default key")
	}
	if b, ok := w.desktop.ScopedBindings().BindingFor(actionTranscriptToggleMsg); ok {
		t.Fatalf("cleared Toggle messages binding still has live registry entry %+v", b)
	}
	if got := persisted.Overrides[string(actionTranscriptToggleMsg)]; got != "none" {
		t.Fatalf("cleared Toggle messages persisted override = %q, want none in %+v", got, persisted)
	}

	pressBottomButton(t, w, 0) // Reset selected row.
	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'a'})
	if _, ok := persisted.Overrides[string(actionTranscriptToggleMsg)]; ok {
		t.Fatalf("reset after clear still persisted Toggle messages override: %+v", persisted)
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
	selectCustomizerAction(t, w, actionTranscriptToggleMsg)
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

func TestClearedKeybindingPersistsAndReloadsAsUnbound(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	w.clearBinding(actionTranscriptToggleMsg)

	cfg := w.buildKeybindingsConfig()
	if got := cfg.Overrides[string(actionTranscriptToggleMsg)]; got != "none" {
		t.Fatalf("cleared override = %q, want none in %+v", got, cfg)
	}

	reloaded := newTestWorkbench(t)
	reloaded.LoadKeybindings(cfg)
	sw := reloaded.openWindow("s", "S")
	if _, ok := reloaded.desktop.ScopedBindings().BindingFor(actionTranscriptToggleMsg); ok {
		t.Fatal("reloaded cleared Toggle messages still has a live registry binding")
	}
	if _, fired := dispatchAtFocus(reloaded, sw.history.Component, runeEv('a')); fired {
		t.Fatal("reloaded cleared Toggle messages still fires on default key")
	}
	if got := reloaded.buildKeybindingsConfig().Overrides[string(actionTranscriptToggleMsg)]; got != "none" {
		t.Fatalf("reloaded cleared override reserialized as %q, want none", got)
	}
}

func TestLoadKeybindingsDropsConflictingPersistedOverride(t *testing.T) {
	w := newTestWorkbench(t)
	w.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
		string(actionHelpOverlay): ":",
	}})

	assertChord(t, chordForAction(t, w, actionHelpOverlay), tv.Chord{Rune: '?'})
	if w.isOverridden(actionHelpOverlay) {
		t.Fatal("conflicting hand-edited override remains marked as active after registry rejected it")
	}
	if got := w.buildKeybindingsConfig().Overrides[string(actionHelpOverlay)]; got != "" {
		t.Fatalf("conflicting rejected override was re-serialized as %q", got)
	}
}

func TestKeybindingCustomizerClearEscapeHatchRequiresConfirmation(t *testing.T) {
	w := newTestWorkbench(t)
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionHelpOverlay)
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyBackspace})
	if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("top layer after help clear = %v, want self-lockout confirm-dialog", top)
	}
	pressConfirm(t, w, false)
	assertChord(t, chordForAction(t, w, actionHelpOverlay), tv.Chord{Rune: '?'})
	if len(persisted.Overrides) != 0 {
		t.Fatalf("cancelled clear persisted overrides: %+v", persisted)
	}

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionHelpOverlay)
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyBackspace})
	pressConfirm(t, w, true)
	if b, ok := w.desktop.ScopedBindings().BindingFor(actionHelpOverlay); ok {
		t.Fatalf("confirmed clear left help overlay registered as %+v", b)
	}
	if got := persisted.Overrides[string(actionHelpOverlay)]; got != "none" {
		t.Fatalf("confirmed help clear persisted override = %q, want none in %+v", got, persisted)
	}
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
	selectCustomizerAction(t, w, actionHelpOverlay)
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyF2})
	if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
		t.Fatalf("top layer after help rebind = %v, want self-lockout confirm-dialog", top)
	}
	pressConfirm(t, w, false)
	assertChord(t, chordForAction(t, w, actionHelpOverlay), tv.Chord{Rune: '?'})

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionHelpOverlay)
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	typeFocused(w, tui.TypeEvent{Key: tui.KeyF2})
	pressConfirm(t, w, true)
	assertChord(t, chordForAction(t, w, actionHelpOverlay), tv.Chord{Key: tui.KeyF2})
}
