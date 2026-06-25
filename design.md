# Design — gogent issue #464 (gogent half)

**Ctrl+Shift+G silently degrades to Ctrl+G in the Customize Keybindings dialog.**
Root cause lived in turbotui (already fixed + merged). This is the gogent-side
half: bump to the fixed turbotui, keep the pin guard honest, and make the capture
flow *honest* — bind Ctrl+Shift+<letter> on a capable terminal, and refuse with a
clear, actionable message (never a silent Ctrl+<letter> downgrade) on a legacy one.

## What turbotui already gives us (the seam)

The bump pulls in two merged turbotui behaviours this change relies on:

1. **Extended keyboard protocol.** `setupTerminal` pushes the Kitty disambiguate
   flag (`CSI > 1 u`) and queries the terminal (`CSI ? u`). A capable terminal's
   reply flips the package-private `extendedKeyboardActive` atomic; teardown clears
   it. On such a terminal, Ctrl+Shift+G now decodes to a real
   `TypeEvent{Rune:'g', Ctrl:true, Shift:true}` (CSI 103;6u) instead of a bare `^G`.
2. **Capability-aware deliverability.** `tui.Deliverability` / `Chord.Deliverable()`
   return `true` for Ctrl+Shift+<letter> only while `extendedKeyboardActive` is set;
   in legacy mode they return `false` with the reason
   *"Ctrl+Shift+letter is indistinguishable from Ctrl+letter on most terminals"*.
   `Chord.Matches` compares Shift exactly, so a Ctrl+Shift+G binding fires on the
   capable event and never on the legacy `^G` event.

**No turbotui changes here** — it is read-only and already merged. The flag is
package-private to `tui` and is `false` in every test binary except `tui`'s own, so
gogent's tests observe the legacy verdict (this matters for the test plan below).

## Key realisation about the gogent flow

gogent's capture path is *already structurally correct*:

- `chordFromEvent` (keybindings.go:517) copies `ev.Shift` verbatim — it never strips
  Shift.
- `validateCapture` (keybindings.go:137) calls `chord.Deliverable()` and returns the
  reason on failure.
- `commit` (keybinding_customizer.go:136) does `setStatus("✗ "+reason); return` on a
  failed validation — it does **not** fall through to `applyBinding`.

So on a capable terminal the fixed turbotui makes Ctrl+Shift+G flow through and bind
correctly with Shift intact, *with no gogent code change at all*. The gogent work is
therefore: (a) the dependency/pin bookkeeping, (b) sharpening the refusal **message**
so it is chord-specific and actionable rather than a raw toolkit string, (c) closing a
**persist/reload regression** that capability-awareness introduces, and (d) an optional
"extended keyboard active" affordance. Plus the tests that lock all of this in.

## Files / functions to touch (gogent only)

### 1. `go.mod` / `go.sum` — DONE (driver step)
Bumped to `github.com/hobbestherat/turbotui v0.3.1-0.20260625231311-3b1fbf19235f`
(commit `3b1fbf1…`); `go mod tidy`; `go build ./...` passes.

### 2. `ui/tui/keybindings_issue401_test.go` — pin guard (REQUIRED)
`TestIssue401GoModHasRequestedTurbotuiAndNoReplace` (line ~175) hard-asserts the OLD
pseudo-version `v0.3.1-0.20260625201405-b5a0f5b31982`. Update the assertion string and
its "52604ee / b5a0f5b" comment to the new pin
`v0.3.1-0.20260625231311-3b1fbf19235f`. The no-`replace` half stays. (Confirmed this is
the only hard-coded turbotui pin in gogent — `grep -rn "v0.3.1-0" --include=*.go` hits
only this file.)

### 3. `ui/tui/keybinding_customizer.go` — actionable refusal message (REQUIRED, the #464 ask)

**Hard constraint discovered in review: the status line is one row.** `status` is a
`H:1` `Label` with `Wrap=true` (keybinding_customizer.go:101); turbotui's label
word-wraps then draws **only row 0** — anything that wraps past the first line is
**invisible**. The dialog spec is `MinW:58, PreferredW:62` (dialog_sizing.go:92), so the
status inner width (`width-4`) is **58 cells at preferred size and floors at 54** (MinW
58; the footer floor doesn't raise it). Any refusal copy therefore **must fit ≤54 cells
including the `"✗ "` prefix**, i.e. the helper body must be **≤52 cells**. (My first
draft's ~113-char copy clipped the entire actionable remedy — corrected here. Open
question 3's "length is acceptable" claim was wrong and is withdrawn.)

The issue names the existing `✗ <reason>` status-line channel, and that is also the
right UX: capture is a tight retry loop, so a terse non-modal line beats popping a modal
on every failed keypress. So: keep the status-line channel, but make the capability-gated
message a **chord-specific, actionable, one-line** string that fits. Add a pure helper:

```go
// captureRefusalMessage turns validateCapture's raw reason into a user-facing,
// chord-specific status line. For the capability-gated Ctrl+Shift+<letter> case it
// names the exact chord and gives the actionable remedy in ONE line that fits the
// 1-row status label (≤52 cells of body); every other reason passes through unchanged.
func captureRefusalMessage(chord tv.Chord, reason string) string {
    if isCapabilityGated(chord) { // Ctrl+Shift+<letter>; see keybindings.go
        // e.g. "Ctrl+Shift+G unavailable here — pick another key." (49 cells;
        // +"✗ " = 51 ≤ 54 floor). "here" = this terminal can't deliver it;
        // "pick another key" is the action the user takes without leaving the dialog.
        return fmt.Sprintf("%s unavailable here — pick another key.", displayChord(chord))
    }
    return reason
}
```

`commit`'s guard becomes:
```go
if ok, reason := w.validateCapture(a, chord); !ok {
    setStatus("✗ " + captureRefusalMessage(chord, reason))
    return
}
```

`validateCapture` stays pure and keeps returning turbotui's reason (single source of
truth); the customizer owns presentation. No other commit-path behaviour changes — the
early `return` already prevents any `applyBinding`, so a Shift-dropped chord can never be
silently bound as Ctrl+<letter>.

**Pre-existing, out of #464 scope (noted, not fixed here):** non-capability turbotui
reasons (Ctrl+S/M/[/H/…) are passed through raw and a couple run ~58-72 chars, so they
too clip at row 0 — but they are front-loaded (the chord + cause lead), they predate this
change, and the worst offender (the 72-char Ctrl+Shift+letter reason) is exactly the one
this helper replaces with a fitting line. A general "shorten every reason" pass would
re-duplicate turbotui's reason table in gogent (the coupling §4 warns against) and is left
as a possible follow-up.

### 4. `ui/tui/keybindings.go` — `LoadKeybindings` reload regression (REQUIRED for goal-match)
**The subtle one.** `LoadKeybindings` runs **before** `Run`/`setupTerminal` at **all
three** entry points — `cmd/main.go:238` (then `Run` at 242), `cmd/handoff.go:342`, and
`cmd/attach.go:129` — so `extendedKeyboardActive` is still `false` at load time on every
launch path, even on a capable terminal. `LoadKeybindings` (line 437-440) drops any
override whose `chord.Deliverable()` is false:

```go
if deliverable, _ := chord.Deliverable(); !deliverable {
    continue // a chord the terminal can't deliver
}
```

With capability-aware deliverability this means: a user on a Kitty terminal binds
Ctrl+Shift+G (works live, persists fine), and on next launch the binding is **silently
dropped** — because at config-load time the handshake hasn't happened yet. That breaks
the very goal of #464 ("Ctrl+Shift+G is bindable") across a restart, and the turbotui
doc explicitly warns deliverability "should be consulted while the app is running …
not when reloading persisted config before the terminal exists."

Fix: in `LoadKeybindings`, do not drop a chord *solely* because of the
capability-gated verdict. A persisted chord was already proven deliverable when the
user saved it; whether it can fire is a runtime/terminal property, not a reason to
discard the user's config. Narrowly:

```go
if chord != unboundChord {
    if deliverable, _ := chord.Deliverable(); !deliverable && !isCapabilityGated(chord) {
        continue // a chord no terminal can deliver (Ctrl+M, Ctrl+S, …)
    }
    if allowed, _ := validateScopeRule(a.scope, chord); !allowed {
        continue
    }
}
```

where `isCapabilityGated(c) == c.Ctrl && c.Shift && isLetterRune(c.Rune)` — the *only*
chord class whose deliverability depends on `extendedKeyboardActive`. Permanently
undeliverable chords (Ctrl+M/S/Q/Z/[/H/I/J) stay filtered; scope and conflict passes
are untouched. On a legacy terminal the kept Ctrl+Shift+G simply never matches (the
wire delivers `^G`, and `Matches` is Shift-exact), so it is harmless; on a capable
terminal it fires. The binding survives the round-trip either way.

`isCapabilityGated` / `isLetterRune` (one-liner: `r >= 'a' && r <= 'z'`) live in
keybindings.go next to `isPlainRune`, shared with the customizer helper in §3. Because
this predicate **duplicates** turbotui's single source of truth (the Ctrl+Shift+letter
branch at `app.go:1248`), its doc comment must point at that line so a future turbotui
change to the gated set is flagged to keep gogent in sync — see §(4) below for why the
duplication is accepted (turbotui is read-only/merged and exposes no "is this verdict
terminal-dependent?" query).

### 5. `ui/tui/keybinding_customizer.go` — optional "extended keyboard" affordance (OPTIONAL, low-risk)
gogent has no direct view of `extendedKeyboardActive` (package-private, no accessor),
but can *probe* it cheaply through the public seam: a Ctrl+Shift+<letter> chord is
deliverable iff the protocol is active.

```go
func extendedKeyboardAvailable() bool {
    ok, _ := tv.Chord{Key: tui.KeyRune, Rune: 'g', Ctrl: true, Shift: true}.Deliverable()
    return ok
}
```

**Same 1-row clip constraint applies.** `capturePrompt` already runs to ~63 cells for a
long action name (`Press a key for "Rename session"…  (Esc cancel · Backspace clear)`),
so **appending** to it would push the tail off row 0. So do **not** append. If shipped,
put the indicator in the **idle hint** instead (a `H:1` line shown while browsing, not
during capture), and only when it fits — e.g. swap `keybindCustomizerIdleHint`
(47 cells) for a variant ending `· Ctrl+Shift+ ok` (~64 cells > 54 floor → still risky),
so realistically it belongs as a one-cell **glyph** (e.g. a leading `⌨ `) rather than
prose. Read-only probe, no new API. **Recommendation: drop §5 for this PR** — it is not
required for goal-match and every place to put it is width-constrained; revisit as a
sized follow-up if users ask "is Ctrl+Shift available?".

## User-facing behaviour

| Terminal | User presses Ctrl+Shift+G in capture | Result |
|---|---|---|
| Capable (Kitty/modifyOtherKeys) | event carries Shift; `Deliverable`=true | binds **Ctrl+Shift+G**, Shift intact; persists & reloads |
| Legacy, event carries Shift but flag off* | `Deliverable`=false | status (one line, fits ≤54): **"✗ Ctrl+Shift+G unavailable here — pick another key."** — nothing bound |
| Legacy, wire delivers bare `^G` | event is Ctrl+G (no Shift; gogent cannot know Shift was pressed) | binds Ctrl+G — the honest best-effort; gogent never *fabricates* a downgrade, it just receives Ctrl+G |

\* The reachable real-world refusal case: a terminal that emits the Shift modifier but
didn't confirm the disambiguate flag. gogent's guarantee is the one it can make —
**it never strips Shift to coerce a chord into a deliverable Ctrl+<letter>.**

## Design-criteria assessment

**(1) Goal match.** Exactly #464: Ctrl+Shift+<letter> becomes bindable on capable
terminals (via the bump) and is refused with a clear message on incapable ones, never
silently downgraded. The `LoadKeybindings` fix is in-scope because without it the goal
("bindable") fails across a restart. No scope creep — no new key features, no catalog
changes.

**(2) Usability.** The refusal names the *actual* chord and states the remedy ("pick
another key") in a single line that **provably fits the 1-row status label** (49-cell
body + `"✗ "` = 51 ≤ the 54-cell floor), so nothing the user needs is clipped — the
review-caught defect that the longer copy buried its remedy on an unrendered row 2 is
resolved by fitting the line, not by a heavier surface. The non-modal status line suits
the rapid retry loop better than a per-keypress dialog. The user still drives every
capture; normal chords (Ctrl+N, F-keys, plain letters) are unaffected. Nothing is silent:
success → `"name → chord."`, failure → `"✗ …"`.

**(3) No regressions.** Capture/commit gains only message text; the existing early
`return` already blocked silent binds. `validateCapture` stays pure (existing callers —
`LoadKeybindings`, tests — unchanged in shape). The `LoadKeybindings` change *narrows*
a drop condition (keeps strictly more user bindings), and only for Ctrl+Shift+letter;
permanently-undeliverable and scope/conflict filtering are intact, so
`TestIssue401LoadKeybindingsAppliesGlobalsAndRejectsBadGlobalPlainRune` and the #463
load tests still hold. Pin-guard test updated to match `go.mod`. `displayChord`,
`formatChordSpec`/`parseChordSpec` already round-trip Shift (`"Ctrl+Shift+R"`), so
persistence is unchanged. Whole-repo `go build`, `go vet`, `go test ./...` are the gate.

**(4) Holistic / cross-repo.** The decode + deliverability fix lives in turbotui (right
place — it owns the terminal). gogent consumes it through the public `Chord.Deliverable`
/ `Chord.Matches` seam and adds only presentation + config-resilience, never
re-deriving terminal knowledge. The one cross-repo hazard capability-awareness creates
— a runtime-dependent verdict consulted at pre-terminal config-load time — is handled
on the gogent side (where the load-ordering lives) rather than asking turbotui to change.
turbotui's own `binding_deliverable_capability_test.go` already pins the seam and the
active-mode behaviour gogent can't exercise. The one accepted coupling is §4's
`isCapabilityGated` predicate, which re-derives turbotui's gated set (`Ctrl && Shift &&
a-z`, `app.go:1248`); it is duplicated only because turbotui is read-only/merged and
exposes no "is this verdict terminal-dependent?" query, and its doc comment links
`app.go:1248` so a future turbotui change to the gated set is caught. If turbotui later
adds an accessor, this predicate is the line to delete.

## Tests to add (gogent) — `ui/tui/keybindings_issue464_test.go`

Test binary observes the legacy verdict (flag is `false` outside package `tui`), which
is exactly what lets us assert the refusal path deterministically:

1. **Refusal, no silent downgrade.** `w.validateCapture(action, Chord{Rune:'g',
   Ctrl:true, Shift:true})` → `ok=false`, non-empty reason; and
   `captureRefusalMessage` names "Ctrl+Shift+G" and "Ctrl+G". Drive `commit` through the
   customizer (or its extracted logic) and assert the override map gains **no** entry
   for the action and **no** Ctrl+G binding is recorded — `chordFor` is unchanged.
2. **Shift survives a successful bind (capable-path proxy).** Use a Shift-bearing chord
   that is deliverable independent of the flag — `Chord{Key: tui.KeyF5, Shift:true}` (or
   `Ctrl+Shift+<digit>`, which the toolkit's letter-only Shift gate leaves deliverable):
   `validateCapture` → ok; `applyBinding`; assert `chordFor` keeps `Shift:true` and
   `formatChordSpec` → `"Shift+F5"` round-trips via `parseChordSpec`. This proves the
   gogent pipeline never drops Shift on the success path. (The true capable
   Ctrl+Shift+G bind is covered by turbotui's in-package capability tests — the seam.)
3. **Reload survives the capability boundary.** `LoadKeybindings` with a persisted
   `{"session.new":"Ctrl+Shift+G"}` keeps the override in `w.keybindings` even though the
   test-binary verdict is `false` — guarding the regression in §4. **Must use an action
   whose default is NOT a Ctrl+Shift+letter** (session.new defaults to Ctrl+N): a chord
   equal to the action's own default is stored implicitly (`sameChord(cur,default)` skips
   it at keybindings.go:467), so `window.tileGrid:"Ctrl+Shift+G"` — its existing default —
   would never land in `w.keybindings` and would not exercise §4 at all. Pair with a
   permanently-undeliverable spec (e.g. `session.new:"Ctrl+S"`) that is still correctly
   dropped, to show the narrowing is precise.
4. **Pin guard** already covered by the updated `keybindings_issue401_test.go`.

## Open questions

1. **`LoadKeybindings` relaxation — in scope?** I judge yes (goal-match + no-regression
   demand the binding survive a restart). If the maintainer prefers to keep #464
   strictly to the capture flow, this becomes a follow-up and the reload-drop behaviour
   stands — but then a Ctrl+Shift+G bound on a capable terminal is lost on next launch.
   Recommend including it.
2. **Optional affordance (§5) — ship it?** Every place to put it is width-constrained
   (capture prompt and idle hint are both 1-row and already near the 54-cell floor), so I
   now **recommend dropping it** for this PR and revisiting as a sized glyph follow-up.
3. **Message wording.** Settled on the fitting one-liner *"Ctrl+Shift+G unavailable here
   — pick another key."* (the earlier longer copy is withdrawn — it clipped). If the
   maintainer wants the fuller "indistinct from Ctrl+G / use a capable terminal"
   explanation, the clip-proof channel is `showConfirm(title, msg, nil)` — the existing
   multi-line informational dialog (single OK, wraps + scrolls) — but that trades the
   non-modal status line for a modal on each failed keypress, which I judge worse for the
   retry loop. Flagging the choice rather than deciding it unilaterally.
