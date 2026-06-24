package ui

// Tests for issue #269 phase 4a: gogent's keybinding registry integration.
//
// Phase 4a re-routes the previously-hardcoded transcript-context keys and the
// '?'/':' app-fallthrough keys through turbotui's BindingRegistry (on the
// Desktop), and derives the command-palette / '?'-cheatsheet key hints from that
// registry instead of from hardcoded display strings. The HARD constraint is that
// behavior is preserved exactly — every key does today what it did before.
//
// These tests exercise the integration at the two real dispatch positions the
// toolkit consults (DispatchFocus at the focused-widget stage, DispatchFallthrough
// at the unhandledKeyFn stage), assert the no-drift derivation, and probe the
// edges (modifiers, scoping to the owning window, capital letters, fallback when
// no binding is registered).
//
// Round 1 surfaced two divergences from "behavior preserved exactly": (a) the
// palette/cheatsheet showed transcript letters upper-cased, now FIXED via
// displayChord and guarded by TestTranscriptLetterDisplayIsLowercase /
// TestDisplayChordRendersPressedKeys; (b) capital letters now trigger transcript
// actions because turbotui's matcher is case-insensitive — an ACCEPTED, documented
// divergence (out of phase-4a scope to fix in gogent), characterized by
// TestCapitalLetterTriggersTranscriptAction so any future change stays visible.

import (
	"strings"
	"testing"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// runeEv builds a plain printable-rune key event (no modifiers), exactly as the
// app's byte decoder produces for a typed character.
func runeEv(r rune) tui.TypeEvent { return tui.TypeEvent{Key: tui.KeyRune, Rune: r} }

// dispatchAtFocus faithfully replays the focused-widget dispatch step from
// Desktop.handleType: the focused widget gets the key first (BubbleType); only if
// it declines is the desktop's scoped Focus registry consulted. It returns whether
// the widget consumed the key and whether a Focus binding fired.
func dispatchAtFocus(w *Workbench, focused *tv.VisualComponent, ev tui.TypeEvent) (widgetConsumed, bindingFired bool) {
	if focused != nil && focused.BubbleType(ev) {
		return true, false
	}
	return false, w.desktop.ScopedBindings().DispatchFocus(ev, focused)
}

// transcriptActions lists the Focus-scope transcript bindings and the rune that
// triggers each, together with the historical lowercase hint the command catalog
// used to show.
var transcriptActions = []struct {
	id        tv.ActionID
	rune      rune
	hardcoded string
	cmdName   string
}{
	{actionTranscriptToggleMsg, 'a', "a", "Toggle messages"},
	{actionTranscriptToggleTool, 't', "t", "Toggle tool calls"},
	{actionTranscriptToggleThink, 'r', "r", "Toggle thinking"},
	{actionTranscriptToggleErr, 'e', "e", "Toggle errors"},
	{actionTranscriptFoldAll, 'f', "f", "Fold all"},
	{actionTranscriptUnfoldAll, 'u', "u", "Unfold all"},
	{actionTranscriptCopyAnswer, 'y', "y", "Copy last answer"},
}

// --- Registration -----------------------------------------------------------

// TestTranscriptBindingsRegisteredOnWindowOpen verifies that opening a session
// window registers each transcript-context key as a Focus-scope binding whose
// Target is that window's transcript view, with the expected chord.
func TestTranscriptBindingsRegisteredOnWindowOpen(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	reg := w.desktop.ScopedBindings()
	target := sw.history.Component

	for _, a := range transcriptActions {
		b, ok := reg.BindingFor(a.id)
		if !ok {
			t.Errorf("no binding registered for %q", a.id)
			continue
		}
		if b.Scope != tv.ScopeFocus {
			t.Errorf("%q scope = %v, want Focus", a.id, b.Scope)
		}
		if b.Target != target {
			t.Errorf("%q Target = %p, want the window's transcript view %p", a.id, b.Target, target)
		}
		if b.Chord.Rune != a.rune || b.Chord.Ctrl || b.Chord.Alt || b.Chord.Shift {
			t.Errorf("%q chord = %+v, want plain rune %q", a.id, b.Chord, a.rune)
		}
	}

	// Esc (Show all / clear filter) is a named-key chord, not a rune.
	b, ok := reg.BindingFor(actionTranscriptShowAll)
	if !ok {
		t.Fatalf("no binding registered for %q", actionTranscriptShowAll)
	}
	if b.Chord.Key != tui.KeyEscape || b.Scope != tv.ScopeFocus {
		t.Errorf("showAll binding = %+v, want Esc/Focus", b)
	}

	// The find key '/' is registered as a Focus binding even though no command
	// entry references it by actionID (the palette keeps the composite "Ctrl+F, /").
	if _, ok := reg.BindingFor(actionTranscriptFind); !ok {
		t.Errorf("no binding registered for %q ('/')", actionTranscriptFind)
	}
}

// --- The keys still trigger their actions (Focus scope) ----------------------

// TestTranscriptToggleKeysViaFocusRegistry drives each toggle key through the real
// focused-widget dispatch step and asserts it flips exactly its own event-kind
// filter, then flips it back when pressed again.
func TestTranscriptToggleKeysViaFocusRegistry(t *testing.T) {
	cases := []struct {
		r    rune
		kind eventKind
	}{
		{'a', kindAssistant},
		{'t', kindTool},
		{'r', kindThinking},
		{'e', kindError},
	}
	for _, c := range cases {
		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		hist := sw.history.Component

		if sw.transcript.hidden&c.kind.bit() != 0 {
			t.Fatalf("%c: kind hidden before any key press", c.r)
		}
		if widget, fired := dispatchAtFocus(w, hist, runeEv(c.r)); widget || !fired {
			t.Fatalf("%c: dispatch widget=%v fired=%v, want fired via binding", c.r, widget, fired)
		}
		if sw.transcript.hidden&c.kind.bit() == 0 {
			t.Errorf("%c: kind not hidden after first press", c.r)
		}
		// Pressing again toggles it back.
		dispatchAtFocus(w, hist, runeEv(c.r))
		if sw.transcript.hidden&c.kind.bit() != 0 {
			t.Errorf("%c: kind still hidden after second press", c.r)
		}
	}
}

// TestTranscriptFoldKeysViaFocusRegistry verifies 'f' folds and 'u' unfolds every
// record through the registry.
func TestTranscriptFoldKeysViaFocusRegistry(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	hist := sw.history.Component
	sw.addAssistant("first answer")
	sw.addAssistant("second answer")
	if len(sw.transcript.records) == 0 {
		t.Fatal("expected transcript records to assert fold state against")
	}

	if _, fired := dispatchAtFocus(w, hist, runeEv('f')); !fired {
		t.Fatal("'f' did not fire via the Focus registry")
	}
	for i, r := range sw.transcript.records {
		if !r.collapsed {
			t.Errorf("record %d not collapsed after 'f'", i)
		}
	}
	if _, fired := dispatchAtFocus(w, hist, runeEv('u')); !fired {
		t.Fatal("'u' did not fire via the Focus registry")
	}
	for i, r := range sw.transcript.records {
		if r.collapsed {
			t.Errorf("record %d still collapsed after 'u'", i)
		}
	}
}

// TestTranscriptFindKeyOpensPrompt verifies the '/' key opens the find prompt via
// the registry (the actionTranscriptFind binding), matching the old switch's '/'.
func TestTranscriptFindKeyOpensPrompt(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('/')); !fired {
		t.Fatal("'/' did not fire via the Focus registry")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "input-dialog" {
		t.Fatalf("top layer after '/' = %v, want input-dialog (find prompt)", top)
	}
}

// TestTranscriptCopyKeyViaRegistry verifies the 'y' yank key fires and, with no
// answer yet, records the same "no answer" note the old handler produced — without
// panicking on the clipboard path.
func TestTranscriptCopyKeyViaRegistry(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('y')); !fired {
		t.Fatal("'y' did not fire via the Focus registry")
	}
	if !noteContains(sw, "no answer") {
		t.Error("expected a 'no answer to copy yet' note after 'y' with an empty transcript")
	}
}

// TestTranscriptShowAllEscBehavior pins the subtle Esc contract that must be
// preserved: when a filter is active Esc clears it AND consumes the key; when
// nothing is filtered Esc declines (returns false) so it keeps falling through the
// dispatch chain exactly as the old handleTranscriptKey did.
func TestTranscriptShowAllEscBehavior(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	hist := sw.history.Component
	esc := tui.TypeEvent{Key: tui.KeyEscape}

	// Not filtering: Esc must decline so it can fall through.
	if sw.transcript.filtering() {
		t.Fatal("fresh transcript should not be filtering")
	}
	if _, fired := dispatchAtFocus(w, hist, esc); fired {
		t.Error("Esc consumed the key while not filtering; it must fall through")
	}

	// Now apply a filter, then Esc must clear it and consume.
	sw.transcript.toggleKind(kindAssistant)
	if !sw.transcript.filtering() {
		t.Fatal("expected filtering active after toggling a kind")
	}
	if _, fired := dispatchAtFocus(w, hist, esc); !fired {
		t.Error("Esc did not consume the key while filtering")
	}
	if sw.transcript.filtering() {
		t.Error("Esc did not clear the active filter")
	}
}

// TestTranscriptKeysDeclinedByWidget confirms the precondition the new dispatch
// position relies on: the transcript view (and its ancestors) decline every
// transcript-context key, so the Focus binding — consulted only AFTER the widget
// declines — is the authoritative handler. If the view ever started consuming one
// of these keys, the binding would silently stop firing.
func TestTranscriptKeysDeclinedByWidget(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	hist := sw.history.Component
	for _, a := range transcriptActions {
		if hist.BubbleType(runeEv(a.rune)) {
			t.Errorf("transcript view consumed %q; the Focus binding would never see it", a.rune)
		}
	}
	if hist.BubbleType(runeEv('/')) {
		t.Error("transcript view consumed '/'")
	}
}

// --- Modifiers are not part of these chords ---------------------------------

// TestTranscriptBindingsIgnoreModifiers verifies Ctrl/Alt-modified variants of the
// transcript keys do NOT fire, preserving the old "if event.Ctrl || event.Alt
// return false" guard.
func TestTranscriptBindingsIgnoreModifiers(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	hist := sw.history.Component
	mods := []tui.TypeEvent{
		{Key: tui.KeyRune, Rune: 'a', Ctrl: true},
		{Key: tui.KeyRune, Rune: 'a', Alt: true},
		{Key: tui.KeyEscape, Ctrl: true},
	}
	for _, ev := range mods {
		before := sw.transcript.hidden
		if w.desktop.ScopedBindings().DispatchFocus(ev, hist) {
			t.Errorf("modified key %+v fired a transcript binding", ev)
		}
		if sw.transcript.hidden != before {
			t.Errorf("modified key %+v changed filter state", ev)
		}
	}
}

// --- Focus scoping to the owning window -------------------------------------

// TestTranscriptBindingsScopedToOwningWindow verifies a key fires only the binding
// of the window whose transcript currently holds focus, leaving other windows
// untouched, and that a focus outside any transcript fires nothing.
func TestTranscriptBindingsScopedToOwningWindow(t *testing.T) {
	w := newTestWorkbench(t)
	w1 := w.openWindow("s1", "S1")
	w2 := w.openWindow("s2", "S2")

	// Focus on w1's transcript toggles w1 only.
	if _, fired := dispatchAtFocus(w, w1.history.Component, runeEv('a')); !fired {
		t.Fatal("'a' did not fire for w1")
	}
	if w1.transcript.hidden&kindAssistant.bit() == 0 {
		t.Error("w1 messages not toggled")
	}
	if w2.transcript.hidden&kindAssistant.bit() != 0 {
		t.Error("w2 messages toggled by a key delivered to w1")
	}

	// Focus on w2's transcript toggles w2 only.
	dispatchAtFocus(w, w2.history.Component, runeEv('a'))
	if w2.transcript.hidden&kindAssistant.bit() == 0 {
		t.Error("w2 messages not toggled when focused")
	}
}

// TestTranscriptBindingsNoMatchOffTarget verifies a Focus binding never fires when
// focus is outside any registered transcript (an unrelated component, or nil).
func TestTranscriptBindingsNoMatchOffTarget(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	reg := w.desktop.ScopedBindings()

	if reg.DispatchFocus(runeEv('a'), nil) {
		t.Error("'a' fired with nil focus")
	}
	stranger := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: 1, H: 1})
	if reg.DispatchFocus(runeEv('a'), stranger) {
		t.Error("'a' fired with focus outside any transcript")
	}
	if sw.transcript.hidden != 0 {
		t.Error("off-target dispatch mutated transcript state")
	}
}

// TestReadOnlyWindowGetsTranscriptBindings verifies the analysis (read-only)
// window also registers its transcript bindings — the implementation registers
// them before the readOnly early return.
func TestReadOnlyWindowGetsTranscriptBindings(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindowAny("ro", "Read only", true)
	if _, fired := dispatchAtFocus(w, sw.history.Component, runeEv('a')); !fired {
		t.Fatal("read-only window's 'a' did not fire via the Focus registry")
	}
	if sw.transcript.hidden&kindAssistant.bit() == 0 {
		t.Error("read-only window transcript filter not toggled")
	}
}

// --- Fallthrough scope: '?' and ':' ------------------------------------------

// TestFallthroughBindingsRegistered verifies '?' and ':' are registered as
// Fallthrough-scope bindings on the desktop's scoped registry (not the menu/global
// one) at workbench construction, before any window is opened.
func TestFallthroughBindingsRegistered(t *testing.T) {
	w := newTestWorkbench(t)
	reg := w.desktop.ScopedBindings()

	help, ok := reg.BindingFor(actionHelpOverlay)
	if !ok || help.Scope != tv.ScopeFallthrough || help.Chord.Rune != '?' {
		t.Errorf("help binding = %+v ok=%v, want Fallthrough '?'", help, ok)
	}
	pal, ok := reg.BindingFor(actionCommandPalette)
	if !ok || pal.Scope != tv.ScopeFallthrough || pal.Chord.Rune != ':' {
		t.Errorf("palette binding = %+v ok=%v, want Fallthrough ':'", pal, ok)
	}

	// They answer to the Fallthrough lookup, not the Global/Focus ones.
	if _, ok := reg.MatchFallthrough(runeEv('?')); !ok {
		t.Error("'?' not found via MatchFallthrough")
	}
	if _, ok := reg.Match(runeEv('?')); ok {
		t.Error("'?' must not surface as a Global binding")
	}
	if _, ok := reg.MatchFocus(runeEv('?'), nil); ok {
		t.Error("'?' must not surface as a Focus binding")
	}
}

// TestFallthroughKeysOpenOverlays verifies '?' opens the help overlay and ':' opens
// the command palette through the Fallthrough dispatch position.
func TestFallthroughKeysOpenOverlays(t *testing.T) {
	w := newTestWorkbench(t)
	reg := w.desktop.ScopedBindings()

	if !reg.DispatchFallthrough(runeEv('?')) {
		t.Fatal("'?' did not fire via DispatchFallthrough")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "help-overlay" {
		t.Fatalf("top layer after '?' = %v, want help-overlay", top)
	}

	if !reg.DispatchFallthrough(runeEv(':')) {
		t.Fatal("':' did not fire via DispatchFallthrough")
	}
	if top := w.desktop.TopLayer(); top == nil || top.Name != "command-palette" {
		t.Fatalf("top layer after ':' = %v, want command-palette", top)
	}
}

// TestFallthroughIgnoresModifiers verifies modified '?'/':' do not match the
// Fallthrough bindings.
func TestFallthroughIgnoresModifiers(t *testing.T) {
	w := newTestWorkbench(t)
	reg := w.desktop.ScopedBindings()
	for _, ev := range []tui.TypeEvent{
		{Key: tui.KeyRune, Rune: '?', Ctrl: true},
		{Key: tui.KeyRune, Rune: '?', Alt: true},
		{Key: tui.KeyRune, Rune: ':', Ctrl: true},
	} {
		if reg.DispatchFallthrough(ev) {
			t.Errorf("modified key %+v fired a Fallthrough binding", ev)
		}
	}
}

// --- Menu accelerators unaffected; registry separation -----------------------

// TestMenuAcceleratorsUnaffected verifies menu accelerators (e.g. Ctrl+N) still
// live in the menu's Global registry and that the scoped keys ('?', ':', and the
// transcript letters) are NOT registered there.
func TestMenuAcceleratorsUnaffected(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S") // also registers transcript bindings (scoped, not menu)
	menu := w.desktop.Bindings()

	if _, ok := menu.Match(tui.TypeEvent{Key: tui.KeyRune, Rune: 'n', Ctrl: true}); !ok {
		t.Error("Ctrl+N menu accelerator no longer matches in the Global registry")
	}
	// Scoped keys must not leak into the menu (Global) registry.
	for _, ev := range []tui.TypeEvent{runeEv('?'), runeEv(':'), runeEv('a'), runeEv('r')} {
		if _, ok := menu.Match(ev); ok {
			t.Errorf("scoped key %+v unexpectedly matched as a Global menu accelerator", ev)
		}
	}
}

// --- Derived display: no drift, and fallback when unregistered ---------------

// TestPaletteCheatsheetDeriveFromRegistryNoDrift verifies the core invariant: when
// a command entry carries an actionID and that action is registered, the entry's
// key hint is exactly the registry-derived display for the live binding — palette
// and cheatsheet can never drift from the real binding.
//
// The derivation is displayChord(binding.Chord), NOT Chord.String() directly: the
// fix for the uppercase-letter display regression renders a bare letter chord as the
// unshifted key the user presses ("a", not "A"). Asserting against displayChord is
// the true no-drift contract — the palette must show whatever that single derivation
// function produces, so a future rebind flows through identically.
func TestPaletteCheatsheetDeriveFromRegistryNoDrift(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	reg := w.desktop.ScopedBindings()
	cmds := w.commands()

	check := func(id tv.ActionID, name string) {
		c, ok := findCommand(cmds, name)
		if !ok {
			t.Errorf("command %q missing", name)
			return
		}
		b, ok := reg.BindingFor(id)
		if !ok {
			t.Errorf("registry has no binding for %q", id)
			return
		}
		if want := displayChord(b.Chord); c.keys != want {
			t.Errorf("%q palette keys = %q, registry-derived = %q (drift)", name, c.keys, want)
		}
	}
	check(actionTranscriptToggleMsg, "Toggle messages")
	check(actionTranscriptToggleThink, "Toggle thinking")
	check(actionTranscriptShowAll, "Show all (clear filter)")
	check(actionHelpOverlay, "Keybinding help")

	// The cheatsheet text contains the derived hint for a derived action.
	if b, ok := reg.BindingFor(actionTranscriptToggleMsg); ok {
		if want := displayChord(b.Chord); !strings.Contains(helpText(cmds), want) {
			t.Errorf("cheatsheet missing derived hint %q", want)
		}
	}
}

// TestCommandsFallbackToHardcodedHints verifies the fallback path: when there is no
// desktop (bare Workbench) or the action is not yet registered (real workbench
// before any session window opens), the catalog keeps its hardcoded key hint
// rather than blanking it.
func TestCommandsFallbackToHardcodedHints(t *testing.T) {
	// (a) Bare workbench: chordDisplay short-circuits on the nil desktop.
	bare := (&Workbench{}).commands()
	if c, _ := findCommand(bare, "Toggle messages"); c.keys != "a" {
		t.Errorf("bare workbench 'Toggle messages' keys = %q, want hardcoded \"a\"", c.keys)
	}

	// (b) Real workbench, no window yet: transcript bindings are registered per
	// window, so none exist and the catalog keeps the hardcoded hint.
	w := newTestWorkbench(t)
	if c, _ := findCommand(w.commands(), "Toggle messages"); c.keys != "a" {
		t.Errorf("windowless workbench 'Toggle messages' keys = %q, want hardcoded \"a\"", c.keys)
	}
}

// TestChordForHelper unit-tests the unified chord lookup directly. Since #401 the
// display path is catalog-based rather than registry-based: it works without a
// desktop, reflects overrides, and unknown actions resolve to the zero chord.
func TestChordForHelper(t *testing.T) {
	if got := chordLabel((&Workbench{}).chordFor(actionHelpOverlay)); got != "?" {
		t.Errorf("bare workbench help display = %q, want ?", got)
	}
	w := newTestWorkbench(t)
	if disp := chordLabel(w.chordFor(actionHelpOverlay)); disp != "?" {
		t.Errorf("chordFor(help) display = %q, want ?", disp)
	}
	if got := w.chordFor(tv.ActionID("does.not.exist")); got != (tv.Chord{}) {
		t.Errorf("unknown action chord = %+v, want zero chord", got)
	}
}

// --- Display fidelity + characterization of the accepted divergence ----------

// TestTranscriptLetterDisplayIsLowercase guards the fix for the display regression
// I flagged in round 1: the catalog must show each transcript letter as the
// unshifted key the user actually presses ("a","t","r","e","f","u","y"), not the
// Shift-looking upper case that tv.Chord.String() produces. The derivation runs
// through displayChord, which lower-cases a bare letter chord while leaving modified
// and named-key chords to Chord.String(). This is the issue's "derive the display
// only if the produced string is equivalent" requirement met.
func TestTranscriptLetterDisplayIsLowercase(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	cmds := w.commands()
	for _, a := range transcriptActions {
		c, ok := findCommand(cmds, a.cmdName)
		if !ok {
			t.Errorf("command %q missing", a.cmdName)
			continue
		}
		if c.keys != a.hardcoded {
			t.Errorf("%q key hint = %q, want %q (the unshifted key the user presses)",
				a.cmdName, c.keys, a.hardcoded)
		}
	}
}

// TestDisplayChordRendersPressedKeys characterizes the displayChord helper across
// the cases the palette/cheatsheet feed it: a bare lowercase letter and the
// (case-insensitively equivalent) upper-case spelling both render lowercase; named
// keys, punctuation, and modified chords pass through tv.Chord.String() unchanged.
func TestDisplayChordRendersPressedKeys(t *testing.T) {
	cases := []struct {
		chord tv.Chord
		want  string
	}{
		{tv.Chord{Rune: 'a'}, "a"},                    // bare letter → lowercase
		{tv.Chord{Rune: 'A'}, "a"},                    // upper spelling → still lowercase
		{tv.Chord{Rune: '/'}, "/"},                    // punctuation unchanged
		{tv.Chord{Rune: '?'}, "?"},                    // punctuation unchanged
		{tv.Chord{Key: tui.KeyEscape}, "Esc"},         // named key via String()
		{tv.Chord{Rune: 'r', Ctrl: true}, "Ctrl+R"},   // modified → String() (upper, with mod)
		{tv.Chord{Rune: 'f', Shift: true}, "Shift+F"}, // any modifier → String()
		{tv.Chord{Key: tui.KeyF1}, "F1"},              // function key via String()
	}
	for _, c := range cases {
		if got := displayChord(c.chord); got != c.want {
			t.Errorf("displayChord(%+v) = %q, want %q", c.chord, got, c.want)
		}
	}
}

// TestCapitalLetterTriggersTranscriptAction pins the ACCEPTED behavior change the
// driver documented in registerTranscriptBindings: the old handleTranscriptKey
// switched on the exact rune, so a capital letter (Shift+a → rune 'A' delivered with
// Shift=false on the single-byte decode path) was inert. Routing through the
// registry, whose Chord.Matches is case-insensitive, makes capitals fire the same
// action. This is a property of turbotui's matcher, not something gogent can override
// while dispatching through the registry, so it is out of phase-4a scope and accepted
// rather than fixed. This test characterizes that reality (and that ALL the letter
// actions behave consistently under capitals) so any future change is visible; if
// turbotui later restores case-exact matching, this test flips and must be revisited.
func TestCapitalLetterTriggersTranscriptAction(t *testing.T) {
	for _, a := range transcriptActions {
		if a.rune < 'a' || a.rune > 'z' {
			continue
		}
		w := newTestWorkbench(t)
		sw := w.openWindow("s", "S")
		reg := w.desktop.ScopedBindings()
		hist := sw.history.Component
		capital := tui.TypeEvent{Key: tui.KeyRune, Rune: a.rune - ('a' - 'A')}

		if _, ok := reg.MatchFocus(capital, hist); !ok {
			t.Errorf("%q: capital spelling no longer matches; the accepted case-insensitive "+
				"divergence changed — revisit", a.cmdName)
		}
		if !reg.DispatchFocus(capital, hist) {
			t.Errorf("%q: capital spelling did not dispatch via the Focus registry", a.cmdName)
		}
	}
}
