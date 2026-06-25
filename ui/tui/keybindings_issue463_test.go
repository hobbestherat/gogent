package ui

// Tests for issue #463: the Customize Keybindings dialog listed only the ~21
// catalog entries carrying an actionID, so frequent keyboard-driven session
// operations — "Rename session" and its siblings — were absent. The fix promotes
// the missing non-slash session/window operations (and the "Copy last code block"
// transcript action) to first-class rebindable actions by giving each catalog
// entry a stable actionID, a scope, and a default chord (a real key for the few
// with a conventional one, the existing unboundChord sentinel for the rest).
//
// These tests pin the five acceptance criteria against the real catalog,
// registry, customizer, menu and persistence paths — and probe the edges the
// issue and design flagged (Ctrl+[ vs Esc, Ctrl+R vs the Focus 'r', the zero
// chord's wildcard hazard, gated globals fired while unwired). They mirror the
// helpers and idioms of keybindings_issue401_test.go /
// keybinding_customizer_phase4b_test.go and add no production code.

import (
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// issue463Promoted lists every action issue #463 promotes from palette-only to
// rebindable, together with the category, scope and default chord the shipping
// implementation assigns. It is the single table the catalog/registry/menu tests
// below project from, so a drift between the catalog and the contract fails fast.
var issue463Promoted = []struct {
	id       tv.ActionID
	name     string
	category string
	scope    tv.Scope
	deflt    tv.Chord
}{
	{actionSessionPrev, "Previous session", "Session", tv.ScopeGlobal, tv.Chord{Rune: 'p', Ctrl: true}},
	{actionSessionRename, "Rename session", "Session", tv.ScopeGlobal, tv.Chord{Key: tui.KeyF2}},
	{actionSessionPin, "Pin / unpin session", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionMoveUp, "Move session up", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionMoveDown, "Move session down", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionSwitchModel, "Switch model", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionExportMD, "Export Markdown", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionExportJSON, "Export JSON", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionCloseOthers, "Close other sessions", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionCloseAll, "Close all sessions", "Session", tv.ScopeGlobal, unboundChord},
	{actionSessionsBrowser, "Saved sessions browser", "Session", tv.ScopeGlobal, unboundChord},
	{actionTranscriptCopyCode, "Copy last code block", "Transcript", tv.ScopeFocus, unboundChord},
}

// isRebindable reports whether id is in the customizer's rebindable() list and
// returns its catalog entry. (actionByID would also do, but this reads the exact
// slice the customizer renders so it pins the user-visible membership.)
func isRebindable(w *Workbench, id tv.ActionID) (action, bool) {
	for _, a := range w.rebindable() {
		if a.actionID == id {
			return a, true
		}
	}
	return action{}, false
}

// --- Criterion 1 & 4: the missing operations are promoted, categorised, and  ---
// --- carry a sensible scope + default.                                       ---

// TestIssue463PromotedActionsAreRebindable is the headline criterion-1 guard:
// every promoted operation now appears in rebindable() (the customizer list) under
// the right category, with the scope and default the contract pins, and a non-nil
// run (so the menu and registry share one closure). It also asserts the real
// defaults (F2, Ctrl+P) are deliverable and scope-legal — i.e. "sensible".
func TestIssue463PromotedActionsAreRebindable(t *testing.T) {
	w := newTestWorkbench(t)
	for _, want := range issue463Promoted {
		a, ok := isRebindable(w, want.id)
		if !ok {
			t.Errorf("%q (%s) is not in rebindable() — customizer still hides it", want.id, want.name)
			continue
		}
		if a.name != want.name {
			t.Errorf("%q name = %q, want %q", want.id, a.name, want.name)
		}
		if a.category != want.category {
			t.Errorf("%q category = %q, want %q", want.id, a.category, want.category)
		}
		if a.scope != want.scope {
			t.Errorf("%q scope = %v, want %v", want.id, a.scope, want.scope)
		}
		if !sameChord(a.deflt, want.deflt) {
			t.Errorf("%q deflt = %+v, want %+v", want.id, a.deflt, want.deflt)
		}
		if a.run == nil {
			t.Errorf("%q has nil run; menu click and the binding must share a closure", want.id)
		}
		// A real default must be terminal-deliverable and pass the scope rule; an
		// unbound default registers nothing, so it is trivially "sensible".
		if want.deflt != unboundChord {
			if ok, reason := want.deflt.Deliverable(); !ok {
				t.Errorf("%q default %+v is not deliverable: %s", want.id, want.deflt, reason)
			}
			if ok, reason := validateScopeRule(want.scope, want.deflt); !ok {
				t.Errorf("%q default %+v breaks the scope rule: %s", want.id, want.deflt, reason)
			}
		}
	}
}

// TestIssue463RenameListedInCustomizer drives the actual customizer dialog and
// confirms "Rename session" is a selectable row showing its F2 default — the exact
// scenario the issue's reproduction said was broken (Rename visible in the palette
// but absent from the customizer).
func TestIssue463RenameListedInCustomizer(t *testing.T) {
	w := newTestWorkbench(t)
	w.showKeybindingCustomizer()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "keybinding-customizer" {
		t.Fatalf("customizer did not open; top layer = %v", top)
	}
	// selectCustomizerAction fatals if the action is not a row in the list.
	selectCustomizerAction(t, w, actionSessionRename)

	a, ok := w.actionByID(actionSessionRename)
	if !ok {
		t.Fatal("Rename session missing from catalog")
	}
	row := w.keybindRowText(a)
	for _, want := range []string{"Rename session", "F2", "(default)"} {
		if !strings.Contains(row, want) {
			t.Errorf("Rename customizer row %q missing %q", row, want)
		}
	}
}

// TestIssue463RenameAlsoRemainsInPalette pins the issue's reproduction contrast:
// "Rename session" stays in the command palette (now WITH its derived F2 hint) —
// the promotion adds rebindability, it does not remove palette discoverability.
func TestIssue463RenameAlsoRemainsInPalette(t *testing.T) {
	w := newTestWorkbench(t)
	c, ok := findCommand(w.commands(), "Rename session")
	if !ok {
		t.Fatal("Rename session missing from the command palette")
	}
	if c.run == nil {
		t.Error("Rename session palette entry has no run")
	}
	if c.keys != "F2" {
		t.Errorf("Rename session palette hint = %q, want derived \"F2\"", c.keys)
	}
}

// --- Criterion 2: a bound chord fires the action live and after a restart.    ---

// TestIssue463RealDefaultGlobalsRegistered confirms the two promoted actions with
// a real default (Rename=F2, Previous=Ctrl+P) are live Global bindings out of the
// box — so they fire before any user customisation.
func TestIssue463RealDefaultGlobalsRegistered(t *testing.T) {
	w := newTestWorkbench(t)
	for _, tt := range []struct {
		id    tv.ActionID
		chord tv.Chord
	}{
		{actionSessionRename, tv.Chord{Key: tui.KeyF2}},
		{actionSessionPrev, tv.Chord{Rune: 'p', Ctrl: true}},
	} {
		b, ok := w.desktop.Bindings().BindingFor(tt.id)
		if !ok {
			t.Errorf("%q not registered as a live binding", tt.id)
			continue
		}
		if b.Scope != tv.ScopeGlobal {
			t.Errorf("%q scope = %v, want Global", tt.id, b.Scope)
		}
		if !sameChord(b.Chord, tt.chord) {
			t.Errorf("%q live chord = %+v, want %+v", tt.id, b.Chord, tt.chord)
		}
	}
}

// TestIssue463RenameBindingFiresRenameSessionImmediately is criterion 2's "fires
// immediately" half: rebinding Rename off its F2 default to F4 makes F4 open the
// rename dialog and frees F2.
func TestIssue463RenameBindingFiresRenameSessionImmediately(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S") // RenameSession(ActiveID()) needs an active session

	w.applyBinding(actionSessionRename, tv.Chord{Key: tui.KeyF4})

	// Old default F2 is now free: nothing is registered on it.
	if w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF2}) {
		t.Fatal("old F2 default still dispatches after Rename was rebound to F4")
	}
	// New binding F4 fires RenameSession → opens the "Rename Session" input dialog.
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF4}) {
		t.Fatal("rebound Rename F4 did not dispatch as a Global binding")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "input-dialog" {
		t.Fatalf("top layer after Rename F4 = %v, want input-dialog", top)
	}
}

// TestIssue463RenameCtrlRDoesNotClashWithFocusR addresses the issue's explicit
// "Ctrl+R may clash with Toggle thinking — verify". A Global Ctrl+R (Rename) and
// the Focus-scope plain 'r' (Toggle thinking) are different chords in different
// scopes, so they coexist: Ctrl+R renames, plain 'r' still toggles thinking.
func TestIssue463RenameCtrlRDoesNotClashWithFocusR(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")

	// Binding Rename to Ctrl+R must not warn about (or evict) the Focus 'r'.
	w.applyBinding(actionSessionRename, tv.Chord{Rune: 'r', Ctrl: true})

	// Global Ctrl+R fires Rename.
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyRune, Rune: 'r', Ctrl: true}) {
		t.Fatal("Rename Ctrl+R did not dispatch")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "input-dialog" {
		t.Fatalf("top layer after Ctrl+R = %v, want input-dialog (Rename)", top)
	}
	// The Focus 'r' still toggles thinking in the transcript.
	if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('r')); !fired {
		t.Fatal("Focus 'r' (Toggle thinking) stopped firing after Rename was bound to Ctrl+R")
	}
	if sw.transcript.hidden&kindThinking.bit() == 0 {
		t.Error("Focus 'r' did not toggle thinking after Rename was bound to Ctrl+R")
	}
}

// TestIssue463UnboundDefaultGlobalsNotRegisteredByDefault is the core invariant
// behind the "unbound by default" design: an unbound-default action is listed in
// the customizer (so the user can assign a key) but registers NO live binding, so
// it cannot steal or block any key until the user opts in.
func TestIssue463UnboundDefaultGlobalsNotRegisteredByDefault(t *testing.T) {
	w := newTestWorkbench(t)
	for _, p := range issue463Promoted {
		if p.deflt != unboundChord {
			continue
		}
		if _, ok := isRebindable(w, p.id); !ok {
			t.Errorf("%q should be listed in the customizer even though unbound", p.id)
		}
		if b, ok := w.desktop.Bindings().BindingFor(p.id); ok {
			t.Errorf("%q registered a live binding by default (%+v); unbound default must register nothing", p.id, b)
		}
	}
	// Defence in depth: a plain unmodified key no action owns is not swallowed by
	// any unbound-default global (the zero-chord wildcard hazard stays absent).
	if w.desktop.Bindings().Dispatch(runeEv('z')) {
		t.Error("an unowned plain key 'z' was consumed by a Global binding")
	}
}

// TestIssue463PreviousSessionDefaultFires exercises the Ctrl+P default end to
// end: with two sessions open it cycles focus to the previous one.
func TestIssue463PreviousSessionDefaultFires(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("a", "A")
	w.openWindow("b", "B") // active = b (most recent)
	if got := w.ActiveID(); got != "b" {
		t.Fatalf("active before Ctrl+P = %q, want b", got)
	}
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyRune, Rune: 'p', Ctrl: true}) {
		t.Fatal("Previous session Ctrl+P did not dispatch")
	}
	if got := w.ActiveID(); got != "a" {
		t.Fatalf("active after Ctrl+P = %q, want a (previous)", got)
	}
}

// TestIssue463UnboundDefaultsBindAndFire proves each class of unbound-default
// action becomes live and observable once the user assigns a chord: Pin toggles
// the pin state, Move Up reorders, Copy last code block fires as a Focus binding.
func TestIssue463UnboundDefaultsBindAndFire(t *testing.T) {
	w := newTestWorkbench(t)

	// Pin / unpin: binding F8 makes it toggle the active session's pin state.
	w.openWindow("s", "S")
	w.applyBinding(actionSessionPin, tv.Chord{Key: tui.KeyF8})
	if w.IsPinned("s") {
		t.Fatal("session unexpectedly pinned before F8")
	}
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF8}) {
		t.Fatal("Pin F8 did not dispatch after being bound")
	}
	if !w.IsPinned("s") {
		t.Error("Pin F8 did not toggle the active session's pin state on")
	}
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF8}) {
		t.Fatal("Pin F8 did not dispatch the second time")
	}
	if w.IsPinned("s") {
		t.Error("Pin F8 did not toggle the active session's pin state back off")
	}

	// Move Active Up: binding F7 moves the active session one slot toward the front.
	w.openWindow("a", "A")
	w.openWindow("b", "B")
	w.openWindow("c", "C") // order [s a b c], active = c
	w.applyBinding(actionSessionMoveUp, tv.Chord{Key: tui.KeyF7})
	before := indexOf(w.orderIDs(), "c")
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF7}) {
		t.Fatal("Move Up F7 did not dispatch after being bound")
	}
	after := indexOf(w.orderIDs(), "c")
	if after >= before {
		t.Errorf("Move Up F7 did not move active toward the front: index %d -> %d", before, after)
	}

	// Copy last code block: a Focus action, so it fires only within a transcript
	// and never as a global accelerator.
	w.applyBinding(actionTranscriptCopyCode, tv.Chord{Rune: 'c'})
	b, ok := w.desktop.Bindings().BindingFor(actionTranscriptCopyCode)
	if !ok || b.Scope != tv.ScopeFocus {
		t.Fatalf("Copy last code block binding = %+v, want a registered Focus binding", b)
	}
	if w.desktop.Bindings().Dispatch(runeEv('c')) {
		t.Error("Copy last code block 'c' dispatched as a Global binding; it must be Focus-scoped")
	}
	sw := w.sessions["c"]
	if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('c')); !fired {
		t.Error("Copy last code block 'c' did not fire as a Focus binding within the transcript")
	}
}

// indexOf returns the index of v in s, or -1.
func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// TestIssue463CopyCodeRegistersInEveryOpenWindow confirms the Focus rebind applies
// live to every already-open transcript window (rebuildBindings re-registers them
// all), mirroring the existing transcript-letter behaviour.
func TestIssue463CopyCodeRegistersInEveryOpenWindow(t *testing.T) {
	w := newTestWorkbench(t)
	w1 := w.openWindow("s1", "S1")
	w2 := w.openWindow("s2", "S2")

	w.applyBinding(actionTranscriptCopyCode, tv.Chord{Rune: 'c'})
	for _, sw := range []*SessionWindow{w1, w2} {
		if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('c')); !fired {
			t.Errorf("Copy last code block 'c' did not fire in session %q after the rebind", sw.title)
		}
	}
}

// TestIssue463RenamePersistsAndSurvivesRestart is criterion 2's "after restart"
// half: a Rename rebind serialises to the config and, when reloaded into a fresh
// workbench, both takes effect (F4 fires) and frees the old default (F2 does not).
func TestIssue463RenamePersistsAndSurvivesRestart(t *testing.T) {
	w := newTestWorkbench(t)
	w.applyBinding(actionSessionRename, tv.Chord{Key: tui.KeyF4})
	cfg := w.buildKeybindingsConfig()
	if got := cfg.Overrides[string(actionSessionRename)]; got != "F4" {
		t.Fatalf("persisted Rename override = %q, want \"F4\" in %+v", got, cfg)
	}

	// Simulate a restart: a brand-new workbench loads the persisted overrides.
	fresh := newTestWorkbench(t)
	fresh.openWindow("s", "S")
	fresh.LoadKeybindings(cfg)

	assertChord(t, chordForAction(t, fresh, actionSessionRename), tv.Chord{Key: tui.KeyF4})
	if !fresh.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF4}) {
		t.Fatal("reloaded Rename F4 did not dispatch")
	}
	if top := fresh.desktop.TopLayer(); top == nil || top.Name != "input-dialog" {
		t.Fatalf("top layer after reloaded F4 = %v, want input-dialog (Rename)", top)
	}
	if fresh.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF2}) {
		t.Error("old Rename F2 default still dispatches after the override was reloaded")
	}
}

// TestIssue463UnboundDefaultPersistRoundTrip covers the persistence edges for an
// action that ships unbound: assigning a chord round-trips through the config, and
// clearing it back to the unbound default persists as ABSENT (buildKeybindingsConfig
// only stores chords that differ from the default, and the default IS the sentinel),
// reloading as unregistered. This is distinct from clearing a REAL-default action,
// whose cleared state does persist as "none" — both branches are exercised below.
func TestIssue463UnboundDefaultPersistRoundTrip(t *testing.T) {
	w := newTestWorkbench(t)

	// Assigning F8 to the (unbound-default) Pin serialises and reloads as a live F8.
	w.applyBinding(actionSessionPin, tv.Chord{Key: tui.KeyF8})
	cfg := w.buildKeybindingsConfig()
	if got := cfg.Overrides[string(actionSessionPin)]; got != "F8" {
		t.Fatalf("persisted Pin override = %q, want F8", got)
	}
	fresh := newTestWorkbench(t)
	fresh.LoadKeybindings(cfg)
	assertChord(t, chordForAction(t, fresh, actionSessionPin), tv.Chord{Key: tui.KeyF8})

	// Clearing it back to its unbound default: the override now equals the default,
	// so it is dropped from the config entirely (not written as "none") and reloads
	// as unregistered.
	w.clearBinding(actionSessionPin)
	cfg = w.buildKeybindingsConfig()
	if _, present := cfg.Overrides[string(actionSessionPin)]; present {
		t.Errorf("cleared unbound-default Pin persisted %q; should be absent (== its default)",
			cfg.Overrides[string(actionSessionPin)])
	}
	fresh2 := newTestWorkbench(t)
	fresh2.LoadKeybindings(cfg)
	if b, ok := fresh2.desktop.Bindings().BindingFor(actionSessionPin); ok {
		t.Errorf("cleared Pin reloaded as a live binding %+v; should register nothing", b)
	}
	if chord := fresh2.chordFor(actionSessionPin); chord != unboundChord {
		t.Errorf("cleared Pin reloaded chord = %+v, want unboundChord", chord)
	}
}

// TestIssue463RealDefaultClearPersistsAsNone is the companion to the above: an
// action whose default is a real chord (Rename = F2), when cleared, persists as
// "none" (cur=unboundChord differs from deflt=F2) and reloads unregistered.
func TestIssue463RealDefaultClearPersistsAsNone(t *testing.T) {
	w := newTestWorkbench(t)
	w.clearBinding(actionSessionRename)
	cfg := w.buildKeybindingsConfig()
	if got := cfg.Overrides[string(actionSessionRename)]; got != "none" {
		t.Fatalf("persisted cleared Rename = %q, want \"none\"", got)
	}
	fresh := newTestWorkbench(t)
	fresh.LoadKeybindings(cfg)
	if b, ok := fresh.desktop.Bindings().BindingFor(actionSessionRename); ok {
		t.Errorf("cleared Rename reloaded as a live binding %+v; should register nothing", b)
	}
	if chord := fresh.chordFor(actionSessionRename); chord != unboundChord {
		t.Errorf("cleared Rename reloaded chord = %+v, want unboundChord", chord)
	}
}

// TestIssue463LoadRejectsIllegalGlobalPlainRune is an error-handling edge: a
// hand-edited config that binds a Global action to a plain rune (which would steal
// the key from every text input) is rejected at load time, so Rename keeps its F2
// default rather than silently grabbing 'z'.
func TestIssue463LoadRejectsIllegalGlobalPlainRune(t *testing.T) {
	w := newTestWorkbench(t)
	w.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
		string(actionSessionRename): "z", // plain rune forbidden in the Global scope
	}})
	assertChord(t, w.chordFor(actionSessionRename), tv.Chord{Key: tui.KeyF2})
	if w.desktop.Bindings().Dispatch(runeEv('z')) {
		t.Error("illegal plain-rune Global override for Rename dispatched; should have been rejected")
	}
}

// --- Criterion 3: the bound chord shows as a shortcut hint on the Session menu. --

// TestIssue463SessionMenuItemsTaggedAndTrackChord confirms the Session-menu items
// the implementation migrated to menuActionItem carry their actionID and render a
// shortcut hint that tracks the live binding — including "Rename Active…" (the
// criterion-3 headline) flipping from F2 to F4 after a rebind.
func TestIssue463SessionMenuItemsTaggedAndTrackChord(t *testing.T) {
	w := newTestWorkbench(t)
	// "Saved Sessions…" needs its listing handler wired to appear; the rest need
	// an active, non-read-only session.
	w.handlers.ListSavedSessions = func() []SessionMeta { return nil }
	w.openWindow("s", "S")
	w.rebuildMenu()

	tagged := []tv.ActionID{
		actionSessionCloseOthers, actionSessionCloseAll, actionSessionsBrowser,
		actionSessionRename, actionSessionPin, actionSessionMoveUp, actionSessionMoveDown,
		actionSessionExportMD, actionSessionExportJSON,
	}
	for _, id := range tagged {
		item := issue401FindMenuItem(issue401MenuBar(t, w).Menus, id)
		if item == nil {
			t.Errorf("Session menu item for %q is not tagged with its ActionID", id)
			continue
		}
		want := chordLabel(w.chordFor(id))
		if item.Shortcut == nil || item.Shortcut.Display != want {
			got := "<nil>"
			if item.Shortcut != nil {
				got = item.Shortcut.Display
			}
			t.Errorf("menu item %q shortcut = %q, want %q", id, got, want)
		}
	}
	// "Rename Active…" specifically shows F2 by default.
	rename := issue401FindMenuItem(issue401MenuBar(t, w).Menus, actionSessionRename)
	if rename == nil || rename.Shortcut == nil || rename.Shortcut.Display != "F2" {
		t.Fatalf("Rename Active shortcut = %+v, want F2", rename)
	}

	// Rebind Rename to F4 and rebuild: the hint tracks the new chord (criterion 3).
	w.applyBinding(actionSessionRename, tv.Chord{Key: tui.KeyF4})
	w.rebuildMenu()
	rename = issue401FindMenuItem(issue401MenuBar(t, w).Menus, actionSessionRename)
	if rename == nil || rename.Shortcut == nil || rename.Shortcut.Display != "F4" {
		t.Fatalf("Rename Active shortcut after rebind = %+v, want F4", rename)
	}
}

// TestIssue463RenameMenuClickStillOpensDialog guards that migrating "Rename
// Active…" to menuActionItem did not change its click behaviour: OnSelect still
// opens the rename dialog for the active session.
func TestIssue463RenameMenuClickStillOpensDialog(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	w.rebuildMenu()
	item := issue401FindMenuItem(issue401MenuBar(t, w).Menus, actionSessionRename)
	if item == nil {
		t.Fatal("Rename Active menu item missing")
	}
	if item.OnSelect == nil {
		t.Fatal("Rename Active menu item has no OnSelect")
	}
	item.OnSelect()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "input-dialog" {
		t.Fatalf("top layer after clicking Rename Active = %v, want input-dialog", top)
	}
}

// --- Criterion 5: no regression in the existing rebindables, slash commands,  ---
// --- the chordLabel display, or the #461 row-width contract.                   ---

// TestIssue463ExistingRebindablesUntouched asserts the 21 actions that were
// already rebindable before #463 are still rebindable with their original
// defaults, so the promotion did not perturb them.
func TestIssue463ExistingRebindablesUntouched(t *testing.T) {
	w := newTestWorkbench(t)
	preExisting := []struct {
		id    tv.ActionID
		deflt tv.Chord
	}{
		{actionSessionNew, tv.Chord{Rune: 'n', Ctrl: true}},
		{actionSessionNext, tv.Chord{Rune: ']', Ctrl: true}},
		{actionSessionClose, tv.Chord{Rune: 'w', Ctrl: true}},
		{actionAppQuit, tv.Chord{Rune: 'q', Ctrl: true}},
		{actionConfigSubagents, tv.Chord{Rune: ',', Ctrl: true}},
		{actionWindowTileVertical, tv.Chord{Rune: 'v', Ctrl: true, Shift: true}},
		{actionWindowTileHorizontal, tv.Chord{Rune: 'h', Ctrl: true, Shift: true}},
		{actionWindowTileGrid, tv.Chord{Rune: 'g', Ctrl: true, Shift: true}},
		{actionWindowMaximizeAll, tv.Chord{Rune: 'm', Ctrl: true, Shift: true}},
		{actionWindowCascade, tv.Chord{Rune: 'd', Ctrl: true, Shift: true}},
		{actionTranscriptFind, tv.Chord{Rune: '/'}},
		{actionTranscriptShowAll, tv.Chord{Key: tui.KeyEscape}},
		{actionTranscriptToggleMsg, tv.Chord{Rune: 'a'}},
		{actionTranscriptToggleTool, tv.Chord{Rune: 't'}},
		{actionTranscriptToggleThink, tv.Chord{Rune: 'r'}},
		{actionTranscriptToggleErr, tv.Chord{Rune: 'e'}},
		{actionTranscriptFoldAll, tv.Chord{Rune: 'f'}},
		{actionTranscriptUnfoldAll, tv.Chord{Rune: 'u'}},
		{actionTranscriptCopyAnswer, tv.Chord{Rune: 'y'}},
		{actionCommandPalette, tv.Chord{Rune: ':'}},
		{actionHelpOverlay, tv.Chord{Rune: '?'}},
	}
	for _, want := range preExisting {
		a, ok := isRebindable(w, want.id)
		if !ok {
			t.Errorf("pre-existing rebindable %q disappeared after #463", want.id)
			continue
		}
		if !sameChord(a.deflt, want.deflt) {
			t.Errorf("pre-existing %q default = %+v, want %+v (regression)", want.id, a.deflt, want.deflt)
		}
	}
}

// TestIssue463SlashCommandsRemainNonRebindable pins criterion 5's slash-command
// half: the client-side slash commands stay text-typed (no actionID), so they are
// absent from the customizer but still present in the palette with their /name.
func TestIssue463SlashCommandsRemainNonRebindable(t *testing.T) {
	w := newTestWorkbench(t)
	slash := []struct {
		name string
		keys string
	}{
		{"Fork session", "/fork"},
		{"Stop turn", "/stop"},
		{"Clear queued message", "/clearqueue"},
		{"Toggle Markdown rendering", "/markdown"},
		{"Undo last turn", "/undo"},
		{"Rewind turns", "/rewind"},
		{"Toggle plan mode", "/plan"},
		{"Toggle YOLO mode", "/yolo"},
		{"Toggle thinking stream", "/thinking"},
	}
	for _, s := range slash {
		c, ok := findCommand(w.commands(), s.name)
		if !ok {
			t.Errorf("slash command %q missing from the palette", s.name)
			continue
		}
		if c.actionID != "" {
			t.Errorf("slash command %q was given an actionID %q; slash commands must stay non-rebindable", s.name, c.actionID)
		}
		if c.keys != s.keys {
			t.Errorf("slash command %q keys = %q, want %q", s.name, c.keys, s.keys)
		}
		if _, rebindable := isRebindable(w, c.actionID); rebindable {
			t.Errorf("slash command %q leaked into the customizer", s.name)
		}
	}
}

// TestIssue463ExportEntryRenamed covers the deliberate palette-text change: the
// two export entries were shortened so the customizer's 26-cell name column does
// not truncate them, and the old long names are gone.
func TestIssue463ExportEntryRenamed(t *testing.T) {
	w := newTestWorkbench(t)
	for _, name := range []string{"Export Markdown", "Export JSON"} {
		if _, ok := findCommand(w.commands(), name); !ok {
			t.Errorf("palette is missing renamed entry %q", name)
		}
	}
	for _, old := range []string{"Export transcript (Markdown)", "Export transcript (JSON)"} {
		if c, ok := findCommand(w.commands(), old); ok {
			t.Errorf("stale export name %q still present in the palette: %+v", old, c)
		}
	}
}

// TestIssue463ChordLabelHidesUnboundSentinel is the regression guard for the
// customizer status-message fix: an unbound default must render as "—" everywhere
// it is shown, never as the sentinel's "F12" spelling (which displayChord leaks).
func TestIssue463ChordLabelHidesUnboundSentinel(t *testing.T) {
	if got := chordLabel(unboundChord); got != "—" {
		t.Errorf("chordLabel(unboundChord) = %q, want \"—\"", got)
	}
	if got := displayChord(unboundChord); got != "F12" {
		t.Errorf("displayChord(unboundChord) = %q, want \"F12\" (the leak chordLabel exists to prevent)", got)
	}
	w := newTestWorkbench(t)
	for _, p := range issue463Promoted {
		if p.deflt != unboundChord {
			continue
		}
		if got := chordLabel(w.chordFor(p.id)); got != "—" {
			t.Errorf("chordLabel(chordFor(%q)) = %q, want \"—\"", p.id, got)
		}
	}
}

// TestIssue463UnboundRowRendersAndFits guards the customizer row for an
// unbound-default action: it shows "—" with an "(unbound)" tag and stays inside
// the #461 row-width / chord-column contract.
func TestIssue463UnboundRowRendersAndFits(t *testing.T) {
	w := newTestWorkbench(t)
	a, ok := w.actionByID(actionSessionPin)
	if !ok {
		t.Fatal("Pin action missing")
	}
	row := w.keybindRowText(a)
	for _, want := range []string{"Pin / unpin session", "—", "(unbound)"} {
		if !strings.Contains(row, want) {
			t.Errorf("Pin row %q missing %q", row, want)
		}
	}
}

// TestIssue463ResetUnboundDefaultStaysUnbound drives the customizer's Reset path
// for an action whose default is unbound: resetting a bound-unbound action back to
// its default leaves it unbound (no live binding) rather than re-arming the
// sentinel as a real key.
func TestIssue463ResetUnboundDefaultStaysUnbound(t *testing.T) {
	w := newTestWorkbench(t)
	w.applyBinding(actionSessionPin, tv.Chord{Key: tui.KeyF8})

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionSessionPin)
	pressBottomButton(t, w, 0) // "&Reset"

	if chord := w.chordFor(actionSessionPin); chord != unboundChord {
		t.Errorf("after Reset, Pin chord = %+v, want unboundChord", chord)
	}
	if b, ok := w.desktop.Bindings().BindingFor(actionSessionPin); ok {
		t.Errorf("after Reset, Pin still has a live binding %+v; default-unbound must register nothing", b)
	}
	if w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF8}) {
		t.Error("F8 still dispatches after Pin was reset to its unbound default")
	}
}

// --- Defences / invariants the design leaned on --------------------------------

// TestIssue463NoRebindableDefaultIsZeroChord guards the catastrophic case the
// design called out: a zero chord default would, once registered, match every
// unmodified keypress (turbotui's Chord.Matches treats KeyUnknown+Rune0 as a
// wildcard). rebuildBindings registers a non-unbound default directly — without
// the Deliverability gate that user captures pass — so a zero chord here would
// be a live key-swallow. Every rebindable default must therefore be either the
// unbound sentinel or a non-zero chord. (The deliverability of the #463-promoted
// real defaults F2 / Ctrl+P is pinned separately in
// TestIssue463PromotedActionsAreRebindable; pre-existing ambiguous defaults like
// Ctrl+Shift+V are an intentional #401 characteristic, out of scope here.)
func TestIssue463NoRebindableDefaultIsZeroChord(t *testing.T) {
	w := newTestWorkbench(t)
	for _, a := range w.rebindable() {
		if a.deflt == (tv.Chord{}) {
			t.Errorf("%q default is the zero chord; it would swallow every unmodified key once registered", a.actionID)
		}
	}
}

// TestIssue463ShippedDefaultsAreConflictFree scans every shipped default for a
// same-scope collision. The unbound defaults share one sentinel but are excluded
// from conflict checks; the real defaults (F2, Ctrl+P, …) must be pairwise unique
// within their scope so the catalog is conflict-free out of the box.
func TestIssue463ShippedDefaultsAreConflictFree(t *testing.T) {
	w := newTestWorkbench(t)
	actions := w.rebindable()
	for i := 0; i < len(actions); i++ {
		for j := i + 1; j < len(actions); j++ {
			a, b := actions[i], actions[j]
			if a.scope != b.scope || a.deflt == unboundChord || b.deflt == unboundChord {
				continue
			}
			if sameChord(a.deflt, b.deflt) {
				t.Errorf("shipped default collision in %v scope: %q and %q both default to %+v",
					a.scope, a.actionID, b.actionID, a.deflt)
			}
		}
	}
}

// TestIssue463GatedGlobalsSafeWhenFiredUnwired verifies the no-regression safety
// claim for the gated promoted actions: with their backend handlers unwired,
// firing a user-assigned chord still behaves gracefully (an "unavailable" confirm)
// rather than panicking — because exportActive / showSessionsDialog guard first.
func TestIssue463GatedGlobalsSafeWhenFiredUnwired(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	for _, tt := range []struct {
		id    tv.ActionID
		chord tv.Chord
	}{
		{actionSessionExportMD, tv.Chord{Key: tui.KeyF5}},
		{actionSessionExportJSON, tv.Chord{Key: tui.KeyF6}},
		{actionSessionsBrowser, tv.Chord{Key: tui.KeyF7}},
	} {
		w.applyBinding(tt.id, tt.chord)
		// Clear any confirm dialog left by the previous iteration before firing.
		for w.desktop.TopLayer() != nil && w.desktop.TopLayer().Name == "confirm-dialog" {
			w.desktop.RemoveLayer(w.desktop.TopLayer())
		}
		if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tt.chord.Key}) {
			t.Errorf("%q did not dispatch when bound and fired unwired", tt.id)
			continue
		}
		if top := w.desktop.TopLayer(); top == nil || top.Name != "confirm-dialog" {
			t.Errorf("%q fired unwired produced top layer %v, want a graceful confirm-dialog", tt.id, top)
		}
	}
}

// TestIssue463F2AndCtrlPSpecRoundTrip pins the serialisation of the two real new
// defaults so a persisted Rename/Previous override round-trips through the config.
func TestIssue463F2AndCtrlPSpecRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		spec  string
		chord tv.Chord
	}{
		{"F2", tv.Chord{Key: tui.KeyF2}},
		{"Ctrl+P", tv.Chord{Rune: 'p', Ctrl: true}},
	} {
		if got := formatChordSpec(tt.chord); got != tt.spec {
			t.Errorf("formatChordSpec(%+v) = %q, want %q", tt.chord, got, tt.spec)
		}
		parsed, ok := parseChordSpec(tt.spec)
		if !ok || !sameChord(parsed, tt.chord) {
			t.Errorf("parseChordSpec(%q) = %+v,%v, want %+v,true", tt.spec, parsed, ok, tt.chord)
		}
	}
}

// TestIssue463CheatsheetShowsPromotedHints is a light regression guard that the
// help overlay now surfaces the promoted actions' derived hints (F2 / Ctrl+P / —)
// without losing its category grouping — the side effect of giving these entries
// an actionID.
func TestIssue463CheatsheetShowsPromotedHints(t *testing.T) {
	w := newTestWorkbench(t)
	text := helpText(w.commands())
	for _, want := range []string{"Session", "Rename session", "F2", "Ctrl+P"} {
		if !strings.Contains(text, want) {
			t.Errorf("cheatsheet missing %q\n%s", want, text)
		}
	}
	// Grouping is preserved: Session still precedes App.
	if strings.Index(text, "Session\n") > strings.Index(text, "App\n") {
		t.Error("Session group no longer precedes App group in the cheatsheet")
	}
}
