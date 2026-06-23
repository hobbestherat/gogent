package ui

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// keybindAction describes one rebindable action in gogent's customizer catalog
// (issue #269, phase 4b): the opaque actionID it shares with turbotui's
// BindingRegistry, the cheatsheet category and label it is shown under, the dispatch
// scope its binding lives in, and its built-in default chord.
//
// keybindActions() is the single source of truth for "which actions are rebindable
// and what their defaults are". registerTranscriptBindings /
// registerFallthroughBindings register exactly these (taking each chord from chordFor
// so an override is applied at registration), and the customizer lists, rebinds and
// resets them — so a default can never drift between registration and reset.
type keybindAction struct {
	id       tv.ActionID
	category string
	name     string
	scope    tv.Scope
	deflt    tv.Chord
}

// keybindActions returns the ordered catalog of rebindable actions. The order and
// categories mirror the '?' cheatsheet grouping (issue #60): the transcript-context
// Focus keys first, then the app-fallthrough overlay keys. Each entry's id/scope/chord
// matches the binding phase 4a registered, so the customizer operates on the live
// bindings rather than a parallel list.
func keybindActions() []keybindAction {
	return []keybindAction{
		{actionTranscriptFind, "Transcript", "Find in transcript", tv.ScopeFocus, tv.Chord{Rune: '/'}},
		{actionTranscriptShowAll, "Transcript", "Show all (clear filter)", tv.ScopeFocus, tv.Chord{Key: tui.KeyEscape}},
		{actionTranscriptToggleMsg, "Transcript", "Toggle messages", tv.ScopeFocus, tv.Chord{Rune: 'a'}},
		{actionTranscriptToggleTool, "Transcript", "Toggle tool calls", tv.ScopeFocus, tv.Chord{Rune: 't'}},
		{actionTranscriptToggleThink, "Transcript", "Toggle thinking", tv.ScopeFocus, tv.Chord{Rune: 'r'}},
		{actionTranscriptToggleErr, "Transcript", "Toggle errors", tv.ScopeFocus, tv.Chord{Rune: 'e'}},
		{actionTranscriptFoldAll, "Transcript", "Fold all", tv.ScopeFocus, tv.Chord{Rune: 'f'}},
		{actionTranscriptUnfoldAll, "Transcript", "Unfold all", tv.ScopeFocus, tv.Chord{Rune: 'u'}},
		{actionTranscriptCopyAnswer, "Transcript", "Copy last answer", tv.ScopeFocus, tv.Chord{Rune: 'y'}},
		{actionHelpOverlay, "App", "Keybinding help", tv.ScopeFallthrough, tv.Chord{Rune: '?'}},
		{actionCommandPalette, "App", "Command palette", tv.ScopeFallthrough, tv.Chord{Rune: ':'}},
	}
}

// keybindByID looks the catalog entry for id up; ok is false for an unknown action.
func keybindByID(id tv.ActionID) (keybindAction, bool) {
	for _, a := range keybindActions() {
		if a.id == id {
			return a, true
		}
	}
	return keybindAction{}, false
}

// keybindDefault returns the catalog default chord for id, or the zero chord for an
// action not in the catalog.
func keybindDefault(id tv.ActionID) tv.Chord {
	a, _ := keybindByID(id)
	return a.deflt
}

// keybindActionName returns the human label for id, falling back to the raw actionID
// string for one not in the catalog (used in conflict messages).
func keybindActionName(id tv.ActionID) string {
	if a, ok := keybindByID(id); ok {
		return a.name
	}
	return string(id)
}

// isEscapeHatch reports whether id is one of the keyboard paths to the customizer —
// the command palette (':'/Ctrl+K) or the help overlay ('?'). Rebinding one of these
// risks the classic self-lockout (issue #269), so the customizer confirms before
// committing such a change.
func isEscapeHatch(id tv.ActionID) bool {
	return id == actionCommandPalette || id == actionHelpOverlay
}

// chordFor returns the chord currently assigned to id: the user's override when one is
// recorded, otherwise the catalog default. It is the single lookup the registration
// sites use so a persisted override is applied the moment a binding is registered, and
// the customizer renders the live assignment. A nil keybindings map (the zero-value
// Workbench used by some unit tests) reads as "no override", so every action renders
// its default.
func (w *Workbench) chordFor(id tv.ActionID) tv.Chord {
	if c, ok := w.keybindings[id]; ok {
		return c
	}
	return keybindDefault(id)
}

// isOverridden reports whether id is currently bound to something other than its
// catalog default — the "custom" vs "default" tag the customizer shows.
func (w *Workbench) isOverridden(id tv.ActionID) bool {
	return !sameChord(w.chordFor(id), keybindDefault(id))
}

// --- Validation (pure; no mutation) -----------------------------------------

// validateScopeRule enforces issue #269's scope rule: the Global scope is reserved for
// chorded keys (Ctrl/Alt combos, function/named keys), because a plain printable key
// bound globally would be stolen from every text input. Plain letters / '?' / ':' are
// therefore only legal in the Focus and Fallthrough scopes (which fire only when focus
// is not in a text input / the key fell through). ok is true when the chord is allowed
// in the scope; otherwise reason explains the rejection for the capture UI.
func validateScopeRule(scope tv.Scope, c tv.Chord) (ok bool, reason string) {
	if scope == tv.ScopeGlobal && isPlainRune(c) {
		return false, "A plain key can't be a global shortcut — text inputs would capture it. Use a Ctrl/Alt combo or a function key."
	}
	return true, ""
}

// isPlainRune reports whether c is an unmodified printable rune (no Ctrl/Alt and no
// named key) — a letter, digit or punctuation the user types literally. Shift is not
// counted as a modifier here because a shifted printable still arrives as a single
// literal rune. Named keys (Esc, F1, arrows) and Ctrl/Alt combos are NOT plain runes.
func isPlainRune(c tv.Chord) bool {
	return c.Key == tui.KeyUnknown && c.Rune != 0 && !c.Ctrl && !c.Alt
}

// validateCapture runs the non-interactive gate a captured chord must pass before it
// can be committed (issue #269): the toolkit's terminal-deliverability check, then the
// scope rule. ok is true when the chord may be committed (possibly after the
// interactive conflict / self-lockout confirms the dialog handles separately);
// otherwise reason names the first failure. It mutates nothing, so it is the unit a
// tester drives for the deliverability and scope-rejection cases.
func (w *Workbench) validateCapture(a keybindAction, chord tv.Chord) (ok bool, reason string) {
	if deliverable, reason := chord.Deliverable(); !deliverable {
		return false, reason
	}
	if allowed, reason := validateScopeRule(a.scope, chord); !allowed {
		return false, reason
	}
	return true, ""
}

// conflictHolder reports the OTHER action already bound to chord in a's scope, if any.
// It wraps the toolkit's ConflictFor and drops the self-match (an action's own current
// chord is never a conflict), so a non-empty, true result is exactly the holder the
// "⚠ Already bound to <Y>. Reassign?" prompt names. Conflicts are scope-keyed: a Focus
// 'a' and a Fallthrough '?' never collide.
func (w *Workbench) conflictHolder(a keybindAction, chord tv.Chord) (tv.ActionID, bool) {
	reg := w.scopedBindings()
	if reg == nil {
		return "", false
	}
	holder, ok := reg.ConflictFor(chord, a.scope)
	if !ok || holder == a.id {
		return "", false
	}
	return holder, true
}

// --- Mutation (live apply + overrides) --------------------------------------

// scopedBindings returns the desktop's scoped BindingRegistry, or nil when there is no
// desktop yet (a zero-value Workbench). Callers fall back gracefully on nil so the
// override map still updates for persistence even without a live registry.
func (w *Workbench) scopedBindings() *tv.BindingRegistry {
	if w.desktop == nil {
		return nil
	}
	return w.desktop.ScopedBindings()
}

// recordOverride updates the override map for id: it drops the entry when chord equals
// the catalog default (so a reset-to-default leaves no override to persist) and records
// it otherwise. It does not touch the live registry — applyBinding does both.
func (w *Workbench) recordOverride(id tv.ActionID, chord tv.Chord) {
	if w.keybindings == nil {
		w.keybindings = make(map[tv.ActionID]tv.Chord)
	}
	if sameChord(chord, keybindDefault(id)) {
		delete(w.keybindings, id)
		return
	}
	w.keybindings[id] = chord
}

// applyBinding assigns chord to id and applies it live (issue #269). It rebinds the
// registered binding through registry.Rebind — so the new key works at once and the
// cheatsheet/palette (which derive their hints from the registry) update on next open —
// and records the override for persistence. It returns false only when Rebind reports a
// genuine same-scope ConflictError, which the caller must resolve first (via swap or a
// reassign confirm); a "no binding registered yet" outcome (e.g. a Focus action with no
// window open) is not a failure — the override is still recorded so the next window to
// register picks it up.
func (w *Workbench) applyBinding(id tv.ActionID, chord tv.Chord) bool {
	if reg := w.scopedBindings(); reg != nil {
		if _, registered := reg.BindingFor(id); registered {
			if err := reg.Rebind(id, chord); err != nil {
				var conflict *tv.ConflictError
				if errors.As(err, &conflict) {
					return false
				}
				// Any other error (only "no binding registered for action", which the
				// guard above already excludes) is non-fatal: fall through and record
				// the override so a future registration applies it.
				//
				// Rebind updates the FIRST entry carrying id. Fallthrough actions
				// ('?'/':') have exactly one entry, so they update fully; the per-window
				// transcript Focus actions have one entry per open window, so a rebind
				// while several windows are open updates the first and is picked up by
				// the others as they reopen (their registration reads chordFor). This
				// mirrors the pre-existing per-window registration model (see
				// registerTranscriptBindings) and keeps the override map the durable
				// source of truth.
			}
		}
	}
	w.recordOverride(id, chord)
	return true
}

// fKeyChords is the pool of parking chords used to break the transient conflicts a swap
// or a reset-all would otherwise hit (issue #269): function keys are deliverable and no
// catalog action binds them, so any subset is mutually free and free of the letters /
// punctuation the actions use. There are more than the catalog has actions, so a unique
// park is always available.
var fKeyChords = []tv.Chord{
	{Key: tui.KeyF1}, {Key: tui.KeyF2}, {Key: tui.KeyF3}, {Key: tui.KeyF4},
	{Key: tui.KeyF5}, {Key: tui.KeyF6}, {Key: tui.KeyF7}, {Key: tui.KeyF8},
	{Key: tui.KeyF9}, {Key: tui.KeyF10}, {Key: tui.KeyF11}, {Key: tui.KeyF12},
}

// firstFreeChord returns a chord currently unbound in scope, scanning the function-key
// pool. It backs the swap's parking step; with at most a dozen catalog actions and
// twelve function keys one is always free in practice, but it falls back to F12 if the
// scan somehow exhausts.
func (w *Workbench) firstFreeChord(scope tv.Scope) tv.Chord {
	reg := w.scopedBindings()
	for _, c := range fKeyChords {
		if reg == nil {
			return c
		}
		if _, taken := reg.ConflictFor(c, scope); !taken {
			return c
		}
	}
	return tv.Chord{Key: tui.KeyF12}
}

// swapBindings reassigns chord (currently held by holder) to target, giving holder
// target's previous chord in return (issue #269's "Reassign?" path). The swap is
// lossless — both actions stay bound — and uses only the toolkit's conflict-checked
// Rebind: holder is first parked on a free function key so chord frees up, target then
// takes chord, and finally holder takes target's vacated chord. Callers persist once
// afterwards.
func (w *Workbench) swapBindings(target keybindAction, holder tv.ActionID, chord tv.Chord) {
	oldTarget := w.chordFor(target.id)
	park := w.firstFreeChord(target.scope)
	w.applyBinding(holder, park) // holder off `chord`; `chord` now free
	w.applyBinding(target.id, chord)
	w.applyBinding(holder, oldTarget) // `oldTarget` now free; holder lands there
}

// resetBinding restores id to its catalog default and clears its override (issue #269,
// per-row Reset). It returns false when the default is currently held by a DIFFERENT
// action — a clash the user created by overriding that other action onto this default —
// in which case nothing changes and the caller surfaces the holder; reset-all avoids
// this by parking everything first.
func (w *Workbench) resetBinding(id tv.ActionID) bool {
	return w.applyBinding(id, keybindDefault(id))
}

// resetAllBindings restores every catalog action to its default (issue #269,
// dialog-level "Reset all"). It parks every action on a distinct function key first so
// no default is still held by another action, then assigns each its default — the
// defaults are mutually conflict-free by construction, so every assignment succeeds —
// and clears the override map. Callers persist once afterwards.
func (w *Workbench) resetAllBindings() {
	actions := keybindActions()
	for i, a := range actions {
		w.applyBinding(a.id, fKeyChords[i%len(fKeyChords)])
	}
	for _, a := range actions {
		w.applyBinding(a.id, a.deflt)
	}
}

// --- Persistence ------------------------------------------------------------

// buildKeybindingsConfig captures the current overrides as a serialisable config: every
// catalog action bound to something other than its default contributes one
// actionID -> chord-spec entry, so a pristine set of bindings persists nothing and the
// stored form round-trips through parseChordSpec on the next launch.
func (w *Workbench) buildKeybindingsConfig() config.KeybindingsConfig {
	overrides := map[string]string{}
	for _, a := range keybindActions() {
		cur := w.chordFor(a.id)
		if sameChord(cur, a.deflt) {
			continue
		}
		overrides[string(a.id)] = formatChordSpec(cur)
	}
	cfg := config.KeybindingsConfig{}
	if len(overrides) > 0 {
		cfg.Overrides = overrides
	}
	return cfg
}

// persistKeybindings writes the current overrides through the SetKeybindings handler
// (when wired). It is called after every committed rebind / reset so the customisation
// survives a restart, mirroring the theme editor's Save.
func (w *Workbench) persistKeybindings() {
	if w.handlers.SetKeybindings != nil {
		w.handlers.SetKeybindings(w.buildKeybindingsConfig())
	}
}

// LoadKeybindings installs the persisted overrides at startup (issue #269): it rebuilds
// the override map from cfg — ignoring unknown actionIDs and unparseable specs so a
// hand-edited or stale config can't break startup — and re-applies each override to any
// already-registered scoped binding (the '?'/':' fallthrough keys, registered at
// construction). Transcript Focus bindings are registered per window and pick their
// override up from chordFor when their window opens, so a load before sessions are
// restored is sufficient. Call it once, after SetHandlers and before windows open.
func (w *Workbench) LoadKeybindings(cfg config.KeybindingsConfig) {
	w.keybindings = make(map[tv.ActionID]tv.Chord)
	for idStr, spec := range cfg.Overrides {
		id := tv.ActionID(idStr)
		if _, ok := keybindByID(id); !ok {
			continue // unknown action — ignore rather than fail
		}
		chord, ok := parseChordSpec(spec)
		if !ok {
			continue
		}
		if sameChord(chord, keybindDefault(id)) {
			continue // redundant override; leave the action at its default
		}
		w.keybindings[id] = chord
	}
	reg := w.scopedBindings()
	if reg == nil {
		return
	}
	for id, chord := range w.keybindings {
		if _, registered := reg.BindingFor(id); registered {
			// Ignore a conflict: overrides were validated when saved, so this only
			// fails on a hand-edited config — in which case the binding keeps its
			// default, which is a safe degradation.
			_ = reg.Rebind(id, chord)
		}
	}
}

// --- Chord helpers ----------------------------------------------------------

// chordFromEvent builds the Chord a captured key event represents, mirroring how the
// registration sites spell chords: a printable key becomes a Rune chord (lower-cased so
// it canonicalises like the catalog defaults and matches case-insensitively), a named
// key becomes a Key chord, and the Ctrl/Shift/Alt flags carry over.
func chordFromEvent(ev tui.TypeEvent) tv.Chord {
	c := tv.Chord{Ctrl: ev.Ctrl, Shift: ev.Shift, Alt: ev.Alt}
	if ev.Key == tui.KeyRune {
		c.Rune = lowerRune(ev.Rune)
	} else {
		c.Key = ev.Key
	}
	return c
}

// sameChord reports whether two chords are the same binding, using turbotui's matching
// semantics: the rune is compared case-insensitively, a rune-bearing chord constrains
// the KeyRune axis regardless of whether its Key was left unset, and the modifiers are
// exact. It is the equality behind the customizer's "default vs custom" tag and the
// override/default bookkeeping, kept consistent with Chord.conflictsWith (unexported in
// the toolkit) so a chord that conflicts with the default is also reported as equal to
// it.
func sameChord(a, b tv.Chord) bool {
	return effectiveKey(a) == effectiveKey(b) &&
		lowerRune(a.Rune) == lowerRune(b.Rune) &&
		a.Ctrl == b.Ctrl && a.Shift == b.Shift && a.Alt == b.Alt
}

// effectiveKey is the named-key axis a chord actually constrains: KeyRune whenever it
// carries a rune (the toolkit forces KeyRune events for rune chords), else its Key. It
// keeps sameChord aligned with the toolkit's own normalisation.
func effectiveKey(c tv.Chord) tui.KeyCode {
	if c.Rune != 0 {
		return tui.KeyRune
	}
	return c.Key
}

// lowerRune lower-cases an ASCII letter and leaves every other rune untouched.
func lowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// --- Chord serialization (config round-trip) --------------------------------

// keyCodeNames maps the named keys the customizer can capture to their stable spec
// tokens, and nameToKeyCode is its inverse. They are the serialisation tables behind
// formatChordSpec / parseChordSpec, kept separate from the toolkit's display-only
// keyNames so the persisted form is parseable (the toolkit exposes no chord parser).
var keyCodeNames = map[tui.KeyCode]string{
	tui.KeyEnter: "Enter", tui.KeyTab: "Tab", tui.KeyBackspace: "Backspace",
	tui.KeyEscape: "Esc", tui.KeyBackTab: "BackTab",
	tui.KeyUp: "Up", tui.KeyDown: "Down", tui.KeyLeft: "Left", tui.KeyRight: "Right",
	tui.KeyHome: "Home", tui.KeyEnd: "End", tui.KeyPageUp: "PageUp", tui.KeyPageDown: "PageDown",
	tui.KeyInsert: "Insert", tui.KeyDelete: "Delete",
	tui.KeyF1: "F1", tui.KeyF2: "F2", tui.KeyF3: "F3", tui.KeyF4: "F4",
	tui.KeyF5: "F5", tui.KeyF6: "F6", tui.KeyF7: "F7", tui.KeyF8: "F8",
	tui.KeyF9: "F9", tui.KeyF10: "F10", tui.KeyF11: "F11", tui.KeyF12: "F12",
}

var nameToKeyCode = func() map[string]tui.KeyCode {
	m := make(map[string]tui.KeyCode, len(keyCodeNames))
	for k, v := range keyCodeNames {
		m[strings.ToLower(v)] = k
	}
	return m
}()

// formatChordSpec serialises a chord to a stable, human-readable spec string for the
// config file: modifiers in Ctrl, Alt, Shift order joined to the key token by '+', e.g.
// "Ctrl+Shift+R", "a", "Esc", "F1", "/". Letters are upper-cased for readability (the
// match is case-insensitive, so the case is cosmetic); a named key uses its
// keyCodeNames token. parseChordSpec inverts it.
func formatChordSpec(c tv.Chord) string {
	var parts []string
	if c.Ctrl {
		parts = append(parts, "Ctrl")
	}
	if c.Alt {
		parts = append(parts, "Alt")
	}
	if c.Shift {
		parts = append(parts, "Shift")
	}
	parts = append(parts, chordKeyToken(c))
	return strings.Join(parts, "+")
}

// chordKeyToken is the bare key portion of a spec: the named-key token, or the rune
// upper-cased for an ASCII letter. An empty chord renders "?" so a malformed binding is
// at least visible.
func chordKeyToken(c tv.Chord) string {
	if c.Key != tui.KeyUnknown && c.Key != tui.KeyRune {
		if name, ok := keyCodeNames[c.Key]; ok {
			return name
		}
		return fmt.Sprintf("Key%d", int(c.Key))
	}
	if c.Rune == 0 {
		return "?"
	}
	r := c.Rune
	if r >= 'a' && r <= 'z' {
		r -= 'a' - 'A'
	}
	return string(r)
}

// parseChordSpec parses a spec string written by formatChordSpec (or hand-edited)
// back into a Chord; ok is false for an empty or unrecognised spec. A single rune is
// taken literally (so "+", "/", "?" and ":" round-trip), letters are lower-cased to the
// canonical form, and a multi-token spec is split on '+' into Ctrl/Alt/Shift modifiers
// plus a final key token (a single rune or a named-key token).
func parseChordSpec(s string) (tv.Chord, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return tv.Chord{}, false
	}
	// A lone character is a literal rune, including '+' which would otherwise split.
	if utf8.RuneCountInString(s) == 1 {
		return tv.Chord{Rune: lowerRune([]rune(s)[0])}, true
	}
	parts := strings.Split(s, "+")
	c := tv.Chord{}
	for _, p := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "ctrl":
			c.Ctrl = true
		case "alt":
			c.Alt = true
		case "shift":
			c.Shift = true
		default:
			return tv.Chord{}, false
		}
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	switch {
	case utf8.RuneCountInString(last) == 1:
		c.Rune = lowerRune([]rune(last)[0])
	default:
		k, ok := nameToKeyCode[strings.ToLower(last)]
		if !ok {
			return tv.Chord{}, false
		}
		c.Key = k
	}
	return c, true
}
