package ui

// Issue #464 (gogent half) — Ctrl+Shift+G no longer silently degrades to Ctrl+G in the
// Customize Keybindings dialog. The terminal-decode fix lives in turbotui (already
// merged); these tests pin the gogent-side behaviour it relies on:
//
//   - the capture flow refuses a Ctrl+Shift+<letter> chord on a terminal that can't
//     deliver it (the test binary runs with turbotui's extendedKeyboardActive == false,
//     i.e. the legacy verdict), surfacing an actionable, chord-specific status line and
//     NEVER silently binding the degraded Ctrl+<letter>;
//   - a Shift-bearing chord whose deliverability does not depend on the flag still binds
//     with Shift intact (the capable-path proxy);
//   - a persisted Ctrl+Shift+<letter> override survives LoadKeybindings even though the
//     load-time verdict is false (config loads before the terminal handshake), while
//     permanently-undeliverable chords are still dropped and same-scope conflicts are
//     still resolved.
//
// They also guard the two couplings the design accepts: captureRefusalMessage's one-line
// copy must fit the single-row status Label (turbotui draws only row 0 of a wrapped
// label), and isCapabilityGated must track turbotui's capability-gated chord set.

import (
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// TestIssue464CaptureRefusalMessageIsChordSpecificFitsAndActionable checks the status
// line produced for a refused capability-gated capture. It must name the actual chord,
// state a remedy, must not leak the raw toolkit string for the gated case, and — because
// the customizer's status Label is a single wrapped row (turbotui renders only row 0) —
// "✗ " + message must fit the dialog's floored status width or the remedy is clipped.
func TestIssue464CaptureRefusalMessageIsChordSpecificFitsAndActionable(t *testing.T) {
	w := newTestWorkbench(t)
	floor := w.keybindingsDialogSpec().MinW - 4 // status Label inner width at the dialog floor
	if floor < 1 {
		t.Fatalf("status floor = %d, want >0", floor)
	}
	for l := 'a'; l <= 'z'; l++ {
		chord := tv.Chord{Rune: l, Ctrl: true, Shift: true}
		msg := captureRefusalMessage(chord, "RAW TURBOTUI REASON")

		if !strings.Contains(msg, displayChord(chord)) {
			t.Errorf("Ctrl+Shift+%c: refusal %q does not name the chord %q", l, msg, displayChord(chord))
		}
		if !strings.Contains(msg, "key") {
			t.Errorf("Ctrl+Shift+%c: refusal %q states no remedy (not actionable)", l, msg)
		}
		if strings.Contains(msg, "RAW TURBOTUI REASON") {
			t.Errorf("Ctrl+Shift+%c: gated refusal leaked the raw toolkit reason: %q", l, msg)
		}
		if got := tui.StringWidth("✗ " + msg); got > floor {
			t.Errorf("Ctrl+Shift+%c: \"✗ %s\" is %d cells, exceeds the 1-row status floor %d (remedy clips)",
				l, msg, got, floor)
		}
	}
}

// TestIssue464CaptureRefusalMessagePassesThroughNonCapabilityReasons ensures the helper
// only rewrites the capability-gated case: a permanently-undeliverable chord (Ctrl+S,
// flow control — Shift is false so it is NOT capability-gated) surfaces turbotui's own
// reason verbatim, so the scope/deliverability reasons a user can still hit are not
// mangled.
func TestIssue464CaptureRefusalMessagePassesThroughNonCapabilityReasons(t *testing.T) {
	ctrlS := tv.Chord{Rune: 's', Ctrl: true}
	if isCapabilityGated(ctrlS) {
		t.Fatal("Ctrl+S is capability-gated; it must not be (Shift is false)")
	}
	_, raw := ctrlS.Deliverable()
	if raw == "" {
		t.Fatal("Ctrl+S should be undeliverable with a reason under the legacy verdict")
	}
	if got := captureRefusalMessage(ctrlS, raw); got != raw {
		t.Errorf("non-gated refusal = %q, want the raw reason %q passed through unchanged", got, raw)
	}
}

// TestIssue464IsCapabilityGatedMatchesTurbotuiGatedSet pins the coupling accepted by the
// design: isCapabilityGated duplicates turbotui's Ctrl+Shift+letter branch (app.go:1248)
// because turbotui is read-only and exposes no "is this verdict terminal-dependent?"
// query. If the two ever diverge, this test fires. The test binary holds the legacy
// verdict (extendedKeyboardActive is package-private to tui and false here), so a gated
// chord reports undeliverable.
func TestIssue464IsCapabilityGatedMatchesTurbotuiGatedSet(t *testing.T) {
	// Every Ctrl+Shift+<letter> is the gated class and reports undeliverable here.
	for l := 'a'; l <= 'z'; l++ {
		chord := tv.Chord{Rune: l, Ctrl: true, Shift: true}
		if !isCapabilityGated(chord) {
			t.Errorf("Ctrl+Shift+%c is not capability-gated", l)
		}
		if ok, reason := chord.Deliverable(); ok || reason == "" {
			t.Errorf("Ctrl+Shift+%c: Deliverable() = (%v, %q), want (false, reason) under the legacy verdict", l, ok, reason)
		}
	}
	// Ctrl+Shift on a non-letter is NOT gated and stays deliverable.
	for _, r := range []rune{'0', '1', '5', '9', '/', '.', '-'} {
		chord := tv.Chord{Rune: r, Ctrl: true, Shift: true}
		if isCapabilityGated(chord) {
			t.Errorf("Ctrl+Shift+%q is capability-gated; only letters should be", r)
		}
		if ok, _ := chord.Deliverable(); !ok {
			t.Errorf("Ctrl+Shift+%q should be deliverable (not letter-gated)", r)
		}
	}
	// Ctrl-only (no Shift) letters are never capability-gated — even the permanently-
	// undeliverable ones (Ctrl+M/S/Q/Z), which Deliverable filters on its own.
	for _, l := range "abcdefghijklmnopqrstuvwxyz" {
		if isCapabilityGated(tv.Chord{Rune: l, Ctrl: true}) {
			t.Errorf("Ctrl+%c (no Shift) is capability-gated; it must not be", l)
		}
	}
	// Defensive: a stray uppercase stored rune still classifies (chords are canonicalised
	// to lowercase, but the predicate lower-cases before the letter test).
	if !isCapabilityGated(tv.Chord{Rune: 'G', Ctrl: true, Shift: true}) {
		t.Error("Ctrl+Shift+G with an uppercase stored rune is not capability-gated")
	}
}

// TestIssue464ValidateCaptureRefusesCapabilityGatedAllowsShiftBearer is the unit-level
// gate test: a Ctrl+Shift+<letter> is refused with a reason (no silent bind), while a
// Shift-bearing chord whose deliverability is flag-independent (Shift+F5) is accepted.
func TestIssue464ValidateCaptureRefusesCapabilityGatedAllowsShiftBearer(t *testing.T) {
	w := newTestWorkbench(t)
	a, ok := w.actionByID(actionSessionNew)
	if !ok {
		t.Fatal("session.new missing from the catalog")
	}
	if ok, reason := w.validateCapture(a, tv.Chord{Rune: 'g', Ctrl: true, Shift: true}); ok || reason == "" {
		t.Fatalf("validateCapture(Ctrl+Shift+G) = (%v, %q), want (false, reason)", ok, reason)
	}
	if ok, reason := w.validateCapture(a, tv.Chord{Key: tui.KeyF5, Shift: true}); !ok {
		t.Fatalf("validateCapture(Shift+F5) = (%v, %q), want ok (deliverable independent of the flag)", ok, reason)
	}
}

// TestIssue464CaptureCtrlShiftLetterIsRefusedNotSilentlyDowngraded drives the full
// customizer capture path: on a terminal that emits the Shift modifier but has not
// confirmed the extended-keyboard flag, capturing Ctrl+Shift+G must be REFUSED — the
// binding stays at its default, no override is recorded, and the degraded Ctrl+G is
// never bound. The refusal must also not break the retry loop.
func TestIssue464CaptureCtrlShiftLetterIsRefusedNotSilentlyDowngraded(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")

	w.showKeybindingCustomizer()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "keybinding-customizer" {
		t.Fatalf("top layer = %v, want keybinding-customizer", top)
	}
	selectCustomizerAction(t, w, actionSessionNew)
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter}) {
		t.Fatal("Enter did not enter capture mode")
	}
	// Legacy terminal that carries Shift: chordFromEvent preserves Shift, so the chord is
	// Ctrl+Shift+G, which validateCapture must refuse rather than silently bind as Ctrl+G.
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyRune, Rune: 'g', Ctrl: true, Shift: true}) {
		t.Fatal("captured Ctrl+Shift+G was not consumed by the customizer")
	}
	// No silent downgrade: session.new is still its Ctrl+N default and records no override.
	assertChord(t, chordForAction(t, w, actionSessionNew), tv.Chord{Rune: 'n', Ctrl: true})
	if w.isOverridden(actionSessionNew) {
		t.Fatal("a refused capture recorded an override for session.new")
	}
	// The customizer stays open (a refusal does not close the dialog).
	if top := w.desktop.TopLayer(); top == nil || top.Name != "keybinding-customizer" {
		t.Fatalf("top layer after refusal = %v, want keybinding-customizer still open", top)
	}
	// Recovery: the refused capture must not leave the dialog stuck — re-entering capture
	// and pressing a valid chord still binds.
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter}) {
		t.Fatal("Enter did not re-enter capture mode after a refusal")
	}
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyF6}) {
		t.Fatal("recovery F6 capture was not consumed")
	}
	assertChord(t, chordForAction(t, w, actionSessionNew), tv.Chord{Key: tui.KeyF6})
}

// TestIssue464CaptureShiftBearingChordBindsWithShiftIntact is the capable-path proxy:
// the test binary cannot flip turbotui's package-private flag, so it uses a Shift-bearing
// chord whose deliverability is flag-independent (Shift+F5) to prove the gogent pipeline
// never drops Shift on the success path — capture -> commit -> apply -> persist ->
// round-trip.
func TestIssue464CaptureShiftBearingChordBindsWithShiftIntact(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "S")
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }

	w.showKeybindingCustomizer()
	selectCustomizerAction(t, w, actionSessionNew)
	typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
	if !typeFocused(w, tui.TypeEvent{Key: tui.KeyF5, Shift: true}) {
		t.Fatal("captured Shift+F5 was not consumed")
	}
	assertChord(t, chordForAction(t, w, actionSessionNew), tv.Chord{Key: tui.KeyF5, Shift: true})
	if got := persisted.Overrides[string(actionSessionNew)]; got != "Shift+F5" {
		t.Fatalf("persisted session.new = %q, want Shift+F5 in %+v", got, persisted)
	}
	parsed, ok := parseChordSpec("Shift+F5")
	if !ok || !sameChord(parsed, tv.Chord{Key: tui.KeyF5, Shift: true}) {
		t.Fatalf("parseChordSpec(\"Shift+F5\") = %+v ok=%v, want the Shift+F5 chord back", parsed, ok)
	}
}

// TestIssue464LoadKeybindingsKeepsCapabilityGatedOverride guards the §4 fix: a persisted
// Ctrl+Shift+<letter> override survives LoadKeybindings even though the load-time verdict
// is false (config loads before the terminal handshake at all three entry points). It
// uses Ctrl+Shift+T, which differs from session.new's default (Ctrl+N) AND from every
// catalog Ctrl+Shift+letter default (V/H/G/D/M), so the conflict pass keeps it.
func TestIssue464LoadKeybindingsKeepsCapabilityGatedOverride(t *testing.T) {
	w := newTestWorkbench(t)
	w.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
		string(actionSessionNew): "Ctrl+Shift+T",
	}})
	want := tv.Chord{Rune: 't', Ctrl: true, Shift: true}
	if got := w.chordFor(actionSessionNew); !sameChord(got, want) {
		t.Fatalf("chordFor(session.new) = %+v, want %+v (capability-gated override dropped at load)", got, want)
	}
	if !w.isOverridden(actionSessionNew) {
		t.Error("capability-gated Ctrl+Shift+T override was dropped at load (§4 regression)")
	}
	// It is registered live, so it would fire on a capable terminal.
	b, ok := w.desktop.Bindings().BindingFor(actionSessionNew)
	if !ok || !sameChord(b.Chord, want) {
		t.Fatalf("live binding for session.new = %+v ok=%v, want Ctrl+Shift+T registered", b, ok)
	}
}

// TestIssue464LoadKeybindingsDropsPermanentlyUndeliverable confirms the §4 narrowing is
// precise: permanently-undeliverable chords (Shift==false, so not capability-gated) are
// still filtered at load, falling back to the default. This is what stops §4 from
// re-admitting Ctrl+S/M/Q/Z bindings that could never fire.
func TestIssue464LoadKeybindingsDropsPermanentlyUndeliverable(t *testing.T) {
	for _, spec := range []string{"Ctrl+S", "Ctrl+M", "Ctrl+Z", "Ctrl+Q"} {
		w := newTestWorkbench(t)
		w.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
			string(actionSessionNew): spec,
		}})
		if got := w.chordFor(actionSessionNew); !sameChord(got, tv.Chord{Rune: 'n', Ctrl: true}) {
			t.Errorf("%s: chordFor(session.new) = %+v, want the Ctrl+N default (should be dropped)", spec, got)
		}
		if w.isOverridden(actionSessionNew) {
			t.Errorf("%s: permanently-undeliverable override was kept at load", spec)
		}
	}
}

// TestIssue464LoadKeybindingsConflictStillResolvesCapabilityGatedOverride proves §4
// (keep capability-gated chords through the deliverability filter) does NOT bypass
// same-scope conflict resolution: a persisted session.new=Ctrl+Shift+G collides with
// window.tileGrid's catalog default (also Ctrl+Shift+G, Global), so the conflict pass
// still reverts the override. (This is also why a reload test must use a free letter like
// T rather than G — G equals an existing default.)
func TestIssue464LoadKeybindingsConflictStillResolvesCapabilityGatedOverride(t *testing.T) {
	w := newTestWorkbench(t)
	w.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
		string(actionSessionNew): "Ctrl+Shift+G",
	}})
	if got := w.chordFor(actionSessionNew); !sameChord(got, tv.Chord{Rune: 'n', Ctrl: true}) {
		t.Fatalf("chordFor(session.new) = %+v, want Ctrl+N (conflicting override must revert to default)", got)
	}
	if w.isOverridden(actionSessionNew) {
		t.Error("conflicting Ctrl+Shift+G override for session.new was not reverted")
	}
	// The default holder keeps its binding.
	if got := w.chordFor(actionWindowTileGrid); !sameChord(got, tv.Chord{Rune: 'g', Ctrl: true, Shift: true}) {
		t.Fatalf("chordFor(window.tileGrid) = %+v, want its Ctrl+Shift+G default", got)
	}
}

// TestIssue464CtrlShiftLetterSpecRoundTrips locks the persistence path for the exact
// chord class of #464: a Ctrl+Shift+<letter> captured on a capable terminal is written
// to the config as a spec string and reloaded, so the Shift modifier must survive
// formatChordSpec -> parseChordSpec (incl. the catalog Ctrl+Shift defaults V/H/G/D/M).
func TestIssue464CtrlShiftLetterSpecRoundTrips(t *testing.T) {
	for _, l := range "gvhdmt" {
		chord := tv.Chord{Rune: l, Ctrl: true, Shift: true}
		spec := formatChordSpec(chord)
		if !strings.Contains(spec, "Shift") {
			t.Errorf("Ctrl+Shift+%c: spec %q lost the Shift modifier", l, spec)
		}
		back, ok := parseChordSpec(spec)
		if !ok || !sameChord(back, chord) {
			t.Errorf("Ctrl+Shift+%c: spec %q round-tripped to %+v ok=%v, want %+v", l, spec, back, ok, chord)
		}
	}
}

// TestIssue464NormalCaptureUnaffected confirms no regression to ordinary capture: a plain
// chord (F7) and a Ctrl chord (Ctrl+L) capture and bind through the same commit path that
// now routes capability-gated chords through captureRefusalMessage.
func TestIssue464NormalCaptureUnaffected(t *testing.T) {
	for _, ev := range []tui.TypeEvent{
		{Key: tui.KeyF7},
		{Key: tui.KeyRune, Rune: 'l', Ctrl: true},
	} {
		w := newTestWorkbench(t)
		w.openWindow("s", "S")
		w.showKeybindingCustomizer()
		selectCustomizerAction(t, w, actionSessionNext) // default Ctrl+], Global
		typeFocused(w, tui.TypeEvent{Key: tui.KeyEnter})
		if !typeFocused(w, ev) {
			t.Fatalf("normal capture %+v was not consumed", ev)
		}
		chord := chordFromEvent(ev)
		if got := chordForAction(t, w, actionSessionNext); !sameChord(got, chord) {
			t.Fatalf("captured %+v: binding = %+v, want %+v", ev, got, chord)
		}
	}
}
