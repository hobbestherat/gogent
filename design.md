# Design — Issue #461: Customize Keybindings dialog is far too wide

## Problem (one paragraph)

`showKeybindingCustomizer` in `ui/tui/keybinding_customizer.go` sizes its modal with
`tv.DialogSpec{MinW: 58, MinH: 16, MaxH: rows + 9}`. It leaves `PreferredW`, `MaxW`
and `PrefH` at zero, so turbotui's `ResolveDialogRect` (see
`turbotv/dialog_spec.go`) falls back to the **80%-wide / 85%-tall** percentage
default — the dialog balloons to 160 cols on a 200-wide terminal, 240 on a 300-wide
one, even though one row needs only ~51 inner cols. A second, independent bug: the
row text in `keybindRowText` (`ui/tui/keybindings.go`) pads the chord column to a
fixed **10 cells via `padName`, which truncates first**, so a real catalog default
like `Ctrl+Shift+V` (12 cells) renders as `Ctrl+Shift`. The Window-tiling actions
(`actionWindowTileVertical/Horizontal/Grid`, `actionWindowCascade`,
`actionWindowMaximizeAll`) ship with `Ctrl+Shift+V/H/G/D/M` defaults
(`ui/tui/command_palette.go:167-175`), so out-of-the-box bindings are already clipped.

This is exactly the balloon-by-default pattern already fixed for the Sessions
(#322), Watchers (#329), Statistics (#345) and Commands (#448/#455) dialogs in
`ui/tui/dialog_sizing.go`. The keybindings customizer was the one content-driven
dialog that was missed. **This is a FIX, not a feature** — no new capability, no new
flow; it brings one dialog into the house style and stops a column from clipping.

## Scope

gogent-only. turbotui already exposes every primitive needed
(`DialogSpec.PreferredW`/`MaxW`/`PrefH`, `ResolveDialogRect`, `ButtonLabelWidth`,
the percentage-default policy). **No turbotui change.** Verified against the
read-only clone at `$HOME/work/turbotui`: `turbotv/dialog_spec.go` honours a
content-driven `PreferredW` as the size (with the percentage as an upper cap) and
floors at `MinW` last — precisely the behaviour the other gogent dialogs already
rely on. The seam is respected: gogent declares *intent* (a spec), turbotui owns
*policy* (resolution).

---

## Changes

### 1. `ui/tui/keybinding_customizer.go` — named footer labels

Extract the inline footer captions into a package var, mirroring
`commandsFooterLabels`/`watchersFooterLabels`, so `footerRowMinWidth` can measure
them and the spec can floor on them:

```go
// keybindFooterLabels are the customizer's footer action captions in display order.
var keybindFooterLabels = []string{"&Reset", "Reset &All", "Close"}
```

Replace the local `labels := []string{"&Reset", "Reset &All", "Close"}` (line 291)
with a reference to this var. The three `newButton` calls keep indexing
`keybindFooterLabels[0..2]` exactly as today, so footer behaviour is unchanged.

### 2. `ui/tui/dialog_sizing.go` — content-driven spec helper

Add `keybindingsDialogSpec()` next to the other `*DialogSpec` helpers, in the same
house style (content-driven `PreferredW`, hard `MaxW` cap, `MinW` floored on the
footer width):

```go
func (w *Workbench) keybindingsDialogSpec() tv.DialogSpec {
	spec := tv.DialogSpec{
		MinW: 58, MaxW: 76, PreferredW: 62,
		MinH: 16, MaxH: 40, PrefH: 24,
	}
	if need := footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	return spec
}
```

Rationale, matching the issue's measured table:
- `PreferredW: 62` — content size (row inner width ~55 + chrome), well under the
  80% cap so it is honoured as the size, not the balloon.
- `MaxW: 76` — hard ceiling so it never sprawls on an ultrawide terminal, with
  headroom above 62 for the widened chord column (change 3) and longer/translated
  action names.
- `MinW: 58`, raised to `footerRowMinWidth(keybindFooterLabels, …)` if larger — the
  three buttons can never overlap. The customizer footer measures **41 cells**
  (well under 58), so `MinW` stays 58 today; the floor expresses the invariant so a
  future label change is picked up automatically (same as `commandsDialogSpec`).
- `PrefH: 24` default height (~16 visible rows), `MaxH: 40` caps growth — replaces
  the current `MaxH: rows + 9` which, with ~50 actions + ~5 category headers,
  resolves to ~64 and then balloons to the 85% height.

The spec is **static** (no terminal-share term), so the existing `dialog.Fit(spec)`
resize path stays correct — like `sessionsDialogSpec`/`commandsDialogSpec`, and
unlike `browserDialogSpec` which must use `installResizeReflow`. No resize-wiring
change is needed.

### 3. `ui/tui/keybinding_customizer.go` — use the spec, keep `rows` only as a `MaxH` input

Replace line 72:

```go
spec := tv.DialogSpec{MinW: 58, MinH: 16, MaxH: rows + 9}
```

with:

```go
spec := w.keybindingsDialogSpec()
```

The `rows := len(actions) + categories` computation and the `categories` loop
become dead and are removed (the static spec's `MaxH: 40` supersedes `rows + 9`).
`dialog.Fit(spec)` at the end (line 312) is untouched. All inner-layout math —
`listY`, `listH := height - listY - 4`, the status row at `height-3`, the footer at
`height-2`, the label/list/status rects derived from `width` — is driven off the
*resolved* `width`/`height`, so it adapts automatically to the new (smaller) rect
with no edits.

> Note: I will keep change 3 strictly to swapping the spec. The issue's option of
> feeding `MaxH: rows + 9` into the spec for short catalogs is **not** adopted —
> a static helper that returns the same spec regardless of catalog size is the
> cleaner mirror of the other dialogs and is what the sizing tests below lock. With
> ~55 content rows the catalog always exceeds `PrefH: 24`/`MaxH: 40` anyway, so
> `rows + 9` would never bite; dropping it removes a moving part.

### 4. `ui/tui/keybindings.go` — stop truncating the chord column

`keybindRowText` currently hardcodes a 10-cell chord column. Widen it so the
longest **real** chord fits. Two options:

**(a) Minimal — hardcoded 14** (exactly the issue's proposal):
```go
const keybindChordColWidth = 14
...
padName(chordLabel(w.chordFor(a.actionID)), keybindChordColWidth)
```
14 holds `Ctrl+Shift+V` (12 cells) with breathing room; row inner width goes 51→55,
still inside `PreferredW: 62`.

**(b) Recommended — runtime-derived, floored at 14:**
```go
// keybindChordColWidth is the chord column width: wide enough for the widest chord
// currently bound across the catalog (so no label is ever truncated, including a
// user override longer than any default), floored so short catalogs still align.
func (w *Workbench) keybindChordColWidth() int {
	width := 14
	for _, a := range w.rebindable() {
		if n := tv.StringWidth(chordLabel(w.chordFor(a.actionID))); n > width {
			width = n
		}
	}
	return width
}
```
then `padName(chordLabel(w.chordFor(a.actionID)), w.keybindChordColWidth())`.

I recommend **(b)**. The hardcoded 14 fixes the shipped defaults but a *user* can
bind a named-key chord longer than 14 (e.g. `Ctrl+Shift+PageDown` = 19,
`Ctrl+Alt+Shift+Backspace` = 24), which would silently reintroduce the very
truncation we are fixing — a regression risk under criterion (3). Option (b)
eliminates truncation for every binding including overrides, keeps
`keybindRowText`'s single-argument signature (so the existing test call
`w.keybindRowText(a)` and the new truncation test both compile unchanged), and the
worst-case widths above (24 → inner 65 + 4 chrome = 69) still fit under `MaxW: 76`.
Cost is an O(n) scan per row, O(n²) per render (~2500 `StringWidth` calls for ~50
actions) — trivial for a one-shot modal render. `tv.StringWidth` is the same
display-width measure `ButtonLabelWidth`/`footerRowMinWidth` already use, so it is
consistent with `chordLabel`'s rendered width (handles the `—` unbound sentinel
correctly too).

---

## Files to change

| File | Change |
|------|--------|
| `ui/tui/dialog_sizing.go` | Add `keybindingsDialogSpec()` (content-driven, `MaxW` cap, footer-floored `MinW`). |
| `ui/tui/keybinding_customizer.go` | Add `keybindFooterLabels` var; use it for the three footer buttons; replace the inline spec with `w.keybindingsDialogSpec()`; drop the now-dead `rows`/`categories` computation. |
| `ui/tui/keybindings.go` | Widen the chord column in `keybindRowText` via `keybindChordColWidth()` (runtime-derived, floor 14). |

No other files. No turbotui files.

---

## User-facing behavior (before → after)

| Terminal | Before | After (resolved via `ResolveDialogRect`) |
|----------|--------|-------------------------------------------|
| 80×24    | 64×20 (80%) | 58×20 (floor) |
| 120×40   | 96×34 (80%) | 62×34 |
| 200×50   | **160×42** | 62×42 |
| 300×80   | **240×68** | 62×68 (wait: width 62, height capped at MaxH 40) → **62×40** |

(Width settles at `PreferredW` 62 once above the 58 floor; height grows to `MaxH`
40 then stops — no 85% balloon.) The dialog is compact and centred on every
terminal, resize-aware via `dialog.Fit`. Chord labels render in full —
`Ctrl+Shift+V`, `Ctrl+Shift+M`, and any user-bound long chord — never clipped. The
list, status hint, idle-hint, capture flow, conflict/swap/self-lockout confirms,
Reset / Reset All / Close buttons, and persistence are all visually and behaviorally
identical, just inside a right-sized frame.

---

## Design criteria

**(1) Goal match.** The issue asks for a sizing FIX plus the chord-truncation fix.
The change does exactly that and nothing more: a content-driven spec mirroring the
four prior-art dialogs, a `MaxW` cap, a footer-floored `MinW`, and a wider chord
column. No new dialog, command, flow, or capability. No scope creep into the
category grouping, capture pipeline, conflict/swap logic, or persistence — all
explicitly out of scope and untouched.

**(2) Usability.** The dialog now occupies a natural share (~58–62 cols) instead of
sprawling across a wide terminal — the single-column list no longer floats in empty
space. The user still drives every input identically (Enter to rebind, Reset / Reset
All / Close, ↑↓, Esc). The previously-silent truncation that mis-showed
`Ctrl+Shift+V` as `Ctrl+Shift` is surfaced in full, so what the dialog displays now
matches what the key actually is — the right thing is surfaced, not hidden. Footer
buttons are guaranteed to fit (floor on `footerRowMinWidth`). Small terminals keep a
usable 58×16 floor and stay centred.

**(3) No regressions.** The spec is static, so `dialog.Fit` remains the correct
resize path (no `installResizeReflow` needed) — matching `sessionsDialogSpec`’s
contract; a sizing test pins terminal-independence. Inner layout math is derived
from the resolved `width`/`height`, so a smaller rect cannot break it; `listH`’s
existing `if listH < 3` guard already covers the floor. Removing `rows`/`categories`
is safe — those locals fed only the old `MaxH`. The chord-column change keeps
`keybindRowText`’s signature, so existing tests
(`TestKeybindingCustomizerDiscoverabilityAndRows` etc.) compile and still pass
(they assert *substring* presence — `"a"`, `"(default)"`, `"(custom)"` — which wider
padding preserves). Option (b) specifically avoids re-introducing truncation for
user overrides. Persistence/round-trip, conflict-swap, self-lockout and
LoadKeybindings paths are not touched.

**(4) Holistic design across both repos.** The fix lives entirely on the correct
side of the seam: gogent owns *what this dialog wants* (the spec + row text);
turbotui owns *how a spec resolves* and *how a button measures*. We reuse turbotui's
existing primitives (`ResolveDialogRect`, `ButtonLabelWidth` via `footerRowMinWidth`,
`StringWidth`) rather than duplicating or extending them — confirmed sufficient by
reading `$HOME/work/turbotui/turbotv/dialog_spec.go`. No downstream effect on
turbotui; no new coupling. The change is consistent with the four sibling dialogs
already converted, so the codebase converges on one pattern rather than adding a
variant.

---

## Tests (gogent) — mirror the commands/watchers dialog tests

1. **`TestKeybindingsDialogSpecShape`** (in `dialog_sizing_test.go` or a new
   `keybinding_customizer_issue461_test.go`) — lock the spec shape: `PreferredW: 62`,
   `MaxW: 76`, `MinW: 58`, `PrefH: 24`, `MaxH: 40`; ordering `MinW ≤ PreferredW ≤
   MaxW` and `MinH ≤ PrefH ≤ MaxH`; caps under the balloon (`MaxW < 160`, `MaxH <
   42`); and `MinW ≥ footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap)`.
2. **`TestKeybindingsDialogSpecIsTerminalIndependent`** — resolve the spec at
   80×24 / 120×30 / 200×50 / 300×80; assert the *spec* is identical across sizes
   (static → `dialog.Fit` is valid), as `TestCommandsDialogSpecIsTerminalIndependent`
   does.
3. **`TestKeybindingsDialogSizeIsContentDriven`** — drive the real spec through
   `tv.ResolveDialogRect` on 80×24 / 200×50 / 300×80; assert width ≤ `MaxW` (62 on
   the wide terminals, not 160/240) and height ≤ 40, never the 80%/85% balloon, and
   never below the floor.
4. **`TestKeybindingsDialogFooterFitsAtMinWidth`** — at the resolved floor width,
   assert `footerButtonRects(keybindFooterLabels, 2, width-3, y, tv.DefaultButtonGap)`
   yields three non-overlapping, non-clamped rects (mirror
   `TestCommandsDialogFooterFitsAtMinWidth`).
5. **`TestKeybindRowTextDoesNotTruncateLongChord`** — `applyBinding` an action to
   `tv.Chord{Rune: 'v', Ctrl: true, Shift: true}` (or use the shipped
   `actionWindowTileVertical` default), then assert `w.keybindRowText(a)` contains
   the full `"Ctrl+Shift+V"`, not `"Ctrl+Shift"` followed by a column boundary.
   (Optionally also assert a user-bound longer chord, e.g. `Ctrl+Shift+PageDown`,
   renders in full — the case option (b) covers and option (a) would fail.)

All use the existing `newTestWorkbench(t)` helper. Run the dev gate from
[[dev-gate]] (build / vet / gofmt / golangci-lint v2 / `go test` **without** `-race`
on the Pi5).

---

## Open questions

1. **Chord column width — option (a) vs (b).** I recommend (b) (runtime-derived,
   floor 14) because it also covers user-bound long chords and matches the
   issue's "optional refinement"; (a) is the strict minimal fix the issue's body
   shows. If the maintainer prefers the literal minimal diff, (a) is a one-line
   change and test 5 still passes for the shipped defaults — but drop the
   `Ctrl+Shift+PageDown` sub-assertion. **Defaulting to (b)** unless told otherwise.
2. **`PreferredW: 62` vs the issue's headroom.** 62 reproduces the issue's measured
   table exactly. With the runtime-derived column the *content* may momentarily want
   a hair more than 62 only when a user binds an unusually long chord; `MaxW: 76`
   absorbs that. I am keeping `PreferredW: 62` as specified (the dialog will simply
   not grow past 62 for the default catalog, which is the desired compact size). No
   action needed unless the maintainer wants the dialog to auto-widen to the widest
   row — that would mean a content-derived `PreferredW`, a larger change I am not
   proposing.
