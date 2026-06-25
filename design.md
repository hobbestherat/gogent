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

Also align the gap (D4): the layout call at `keybinding_customizer.go:292` currently
hardcodes the gap — `footerButtonRects(labels, 2, width-3, height-2, 2)`. Change the
trailing `2` to `tv.DefaultButtonGap` so the **layout** uses the same gap the **floor**
(`footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap)`) measures with. They
are equal today (`DefaultButtonGap == 2`), so this is behaviour-neutral now, but it
keeps the floor invariant and the layout in lockstep if the toolkit default ever
changes — matching how `commands_dialog.go:487` already passes `tv.DefaultButtonGap`.

### 2. `ui/tui/dialog_sizing.go` — content-driven spec helper

Add `keybindingsDialogSpec()` next to the other `*DialogSpec` helpers, in the same
house style (content-driven `PreferredW`, hard `MaxW` cap, `MinW` floored on the
footer width):

```go
func (w *Workbench) keybindingsDialogSpec() tv.DialogSpec {
	spec := tv.DialogSpec{
		MinW: 58, MaxW: 76, PreferredW: 62,
		MinH: 16, MaxH: 40, PrefH: 34,
	}
	if need := footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap); spec.MinW < need {
		spec.MinW = need
	}
	return spec
}
```

**How `resolveDimension` actually resolves this** (re-read from
`$HOME/work/turbotui/turbotv/dialog_spec.go:79-103`, correcting an earlier misread):
the *preferred* value is the settling size; `Max*` is an upper cap that only ever
pulls the value **down** (it binds only when `preferred > cap`); `Min*` is the only
thing that pushes the value **up**. So with `PreferredW < MaxW` and `PrefH < MaxH`,
the dialog settles at **`PreferredW`/`PrefH`** on any roomy terminal — `MaxW`/`MaxH`
are inert ceilings there, biting only to stop sprawl if a future change raises the
preferred above them. This is exactly what `commands_dialog_issue448_test.go:139,173`
pins for the commands dialog (`200×50 → 112×34`, with `gotH >= 40` an explicit
failure because `PrefH 34`, not `MaxH 40`, is the bound).

Rationale per field:
- `PreferredW: 62` — the settling width: row inner width ~54 (see change 3) + chrome,
  well under the 80% cap. The dialog settles here, **not** at `MaxW`.
- `MaxW: 76` — inert ceiling at the default catalog; it only binds if the content
  ever needs > 76 (it won't), preventing sprawl on an ultrawide terminal. Kept for
  house-style consistency and as a guard.
- `MinW: 58`, raised to `footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap)`
  if larger — the three buttons can never overlap. The footer measures **41 cells**
  (`&Reset`=10 + `Reset &All`=13 + `Close`=10, +2 gaps of 2, +4 edge — `ButtonLabelWidth`
  clamps each to `minButtonWidth` 10), well under 58, so `MinW` stays 58 today; the
  floor expresses the invariant so a future label change is picked up automatically
  (same as `commandsDialogSpec`).
- `PrefH: 34` — the settling height (was 24 in the earlier draft, which would have
  shown only `listH = 34−3−4 = … ` → with 24 only ~17 of ~55 rows, a visibility
  regression vs the current dialog's ~35). At 34 the list shows **`listH = 34 − 3 − 4
  = 27` rows** — a deliberate compact height that keeps most of a typical screenful
  visible while killing the 85% balloon. Mirrors `commandsDialogSpec`'s `PrefH 34`.
- `MaxH: 40` — inert ceiling (`PrefH 34 < 40`), kept as the house-style sprawl guard;
  it does **not** make the dialog 40 tall.

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
> ~55 content rows the catalog always exceeds `PrefH: 34`/`MaxH: 40` anyway, so
> `rows + 9` would never bite; dropping it removes a moving part.

### 4. `ui/tui/keybindings.go` — stop truncating the chord column

`keybindRowText` currently hardcodes a 10-cell chord column, and `padName`
truncates before padding (`resources_dialog.go:360-367`), so any chord wider than 10
is cut. Widen the column to **14** via a named constant:

```go
// keybindChordColWidth is the chord column width in the customizer list. 14 holds
// the widest chord the catalog ships — Ctrl+Shift+V/H/G/D/M (12 cells) — with slack,
// so no default binding is clipped (issue #461).
const keybindChordColWidth = 14
...
return "  " + padName(a.name, 26) + "  " +
	padName(chordLabel(w.chordFor(a.actionID)), keybindChordColWidth) + " (" + tag + ")"
```

14 holds `Ctrl+Shift+V` (12 cells) with breathing room. Row inner width becomes
`2 + 26 + 2 + 14 + len(" (default)")` = `2+26+2+14+10` = **54 cols**, which fits the
Tree's inner width of `width − 4 = 58` at `PreferredW: 62` with 4 cols to spare — so
the row is clipped by neither `padName` nor the Tree.

**Why the hardcoded 14, not a runtime-derived width (resolving the earlier
contradiction).** The earlier draft recommended a runtime
`keybindChordColWidth()` scanning every binding for the widest chord, claiming it
"eliminates truncation for every binding including overrides." Against a *static*
`PreferredW: 62` that is false: widening the column past 13 cells pushes the row past
the 58-cell Tree inner width, so the Tree clips the row's **tail** — the
`(default)`/`(custom)`/`(unbound)` tag, which is *more* informative than the chord the
user just pressed. So the runtime column alone is the worst of both: an O(n²)
`StringWidth` scan per render *and* it still truncates (just the tag) for a user chord
like `Ctrl+Shift+PageDown` (19). The two honest, self-consistent options are:

- **(a) hardcoded 14 + static `PreferredW: 62`** — the chosen fix. Fixes every shipped
  default and every chord ≤ 14 cells (which covers Expected #3's named examples
  `Ctrl+Shift+V`/`Ctrl+Shift+M`, both 12). A *user*-bound chord wider than 14 cells
  (rare; the catalog ships none) still clips the chord — the same failure mode as
  today, just at a higher threshold and outside the bug the issue reports.
- **(b) runtime column + content-derived `PreferredW`** — the only way "never truncated"
  holds for arbitrary user chords: `PreferredW = max(62, 4 + 2+26+2 + chordCol + 10)`,
  capped by `MaxW: 76`. Still terminal-independent (it depends on bindings, not screen
  size), so `dialog.Fit` stays valid. **Not chosen**: it trades the simple, exactly
  pinnable static spec (and the issue's stated "minimal fix") for an edge case the
  issue does not report, and it makes the sizing tests compute expected sizes from the
  catalog rather than asserting constants.

The chosen (a) keeps `keybindRowText`'s single-argument signature, so existing tests
(`keybinding_customizer_phase4b_test.go:184-196`) compile and pass unchanged.

> Honest scope note on Expected #3 ("render in full, never truncated"): the fix
> guarantees this for all **shipped** bindings and the issue's named examples. An
> arbitrary user override exceeding 14 cells is out of the reported scope; option (b)
> above is the documented path if the maintainer wants the literal "never" to hold.

---

## Files to change

| File | Change |
|------|--------|
| `ui/tui/dialog_sizing.go` | Add `keybindingsDialogSpec()` (content-driven `PreferredW 62`/`PrefH 34`, `MaxW`/`MaxH` ceilings, footer-floored `MinW`). |
| `ui/tui/keybinding_customizer.go` | Add `keybindFooterLabels` var; use it for the three footer buttons; change the footer gap arg to `tv.DefaultButtonGap`; replace the inline spec with `w.keybindingsDialogSpec()`; drop the now-dead `rows`/`categories`/`seen` computation. |
| `ui/tui/keybindings.go` | Widen the chord column in `keybindRowText` from 10 → `keybindChordColWidth` (const 14). |

No other files. No turbotui files.

---

## User-facing behavior (before → after)

Resolved sizes computed by hand-tracing `resolveDimension` for the chosen spec
(`PreferredW 62, MaxW 76, MinW 58 / PrefH 34, MaxH 40, MinH 16`, margin 2):

| Terminal | Before | After | Notes |
|----------|--------|-------|-------|
| 40×16    | 32×13 (80/85%) | **58×16** | both floors win past the screen edge |
| 80×24    | 64×20 (80%)    | **62×20** | width = `PreferredW` 62 (≤ 80%·80=64, floor 58 doesn't bind); height = 85%·24 = 20 caps `PrefH` 34 down |
| 120×40   | 96×34 (80%)    | **62×34** | width 62; height = `PrefH` 34 (< 85%·40=34 cap, < `MaxH` 40) |
| 200×50   | **160×42**     | **62×34** | width 62 (not 160); height 34 (not 42) |
| 300×80   | **240×68**     | **62×34** | settles at `PreferredW`/`PrefH`; `MaxW`/`MaxH` never bind |

Key correction over the earlier draft: the height settles at **`PrefH` 34**, not
`MaxH` 40 — `MaxH` is an inert ceiling here. At height 34 the list shows
`listH = 34 − 3 − 4 = 27` rows (`keybinding_customizer.go:83`). That is fewer than the
old over-tall balloon's ~35 but is a deliberate compact choice that still shows most
of a screenful; it is the conscious cost of not ballooning, and it matches
`commandsDialogSpec`'s height. The dialog is compact and centred on every terminal,
resize-aware via `dialog.Fit`. Chord labels for every shipped binding —
`Ctrl+Shift+V`, `Ctrl+Shift+M` and the rest of the `Ctrl+Shift+*` tiling set — render
in full, never clipped. The list, status hint, idle-hint, capture flow,
conflict/swap/self-lockout confirms, Reset / Reset All / Close buttons, and
persistence are all visually and behaviorally identical, just inside a right-sized
frame.

---

## Design criteria

**(1) Goal match.** The issue asks for a sizing FIX plus the chord-truncation fix.
The change does exactly that and nothing more: a content-driven spec mirroring the
four prior-art dialogs, a `MaxW` cap, a footer-floored `MinW`, and a wider chord
column. No new dialog, command, flow, or capability. No scope creep into the
category grouping, capture pipeline, conflict/swap logic, or persistence — all
explicitly out of scope and untouched.

**(2) Usability.** The dialog now occupies a natural width (62 cols) instead of
sprawling across a wide terminal — the single-column list no longer floats in empty
space. Height settles at 34 (27 visible rows), a deliberate compact size: fewer than
the old balloon's ~35 rows but enough to show most of a typical catalog screenful,
and the list scrolls for the rest exactly as it does today. The user still drives
every input identically (Enter to rebind, Reset / Reset All / Close, ↑↓, Esc). The
previously-silent truncation that mis-showed `Ctrl+Shift+V` as `Ctrl+Shift` is
surfaced in full, so what the dialog displays matches the actual key — the right
thing is surfaced, not hidden. (Honest edge: a user override wider than 14 cells —
none ship — still clips the chord; see change 4's scope note and option (b).) Footer
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
padding preserves). The hardcoded 14-cell column (change 4, option a) keeps the row
inner width at 54 ≤ the 58-cell Tree inner width, so no row's tag is clipped for any
shipped binding. The footer-gap alignment (D4) is behaviour-neutral today
(`DefaultButtonGap == 2`). Persistence/round-trip, conflict-swap, self-lockout and
LoadKeybindings paths are not touched.

**(4) Holistic design across both repos.** The fix lives entirely on the correct
side of the seam: gogent owns *what this dialog wants* (the spec + row text);
turbotui owns *how a spec resolves* and *how a button measures*. We reuse turbotui's
existing primitives (`ResolveDialogRect`, `ButtonLabelWidth` via `footerRowMinWidth`,
`DefaultButtonGap`) rather than duplicating or extending them. Sufficiency was
re-verified against the read-only clone for **both** axes of `resolveDimension`
(`$HOME/work/turbotui/turbotv/dialog_spec.go:79-103`): width *and* height settle at
the preferred value, with `Max*` a downward-only ceiling and `Min*` the only upward
force — the height half of this is what the earlier draft misread, now corrected in
the spec, the behavior table, and the tests. No downstream effect on turbotui; no new
coupling. The change is consistent with the four sibling dialogs already converted,
so the codebase converges on one pattern rather than adding a variant.

---

## Tests (gogent) — mirror the commands/watchers dialog tests

1. **`TestKeybindingsDialogSpecShape`** (in `dialog_sizing_test.go` or a new
   `keybinding_customizer_issue461_test.go`) — lock the spec shape: `PreferredW: 62`,
   `MaxW: 76`, `MinW: 58`, **`PrefH: 34`**, `MaxH: 40`; ordering `MinW ≤ PreferredW ≤
   MaxW` and `MinH ≤ PrefH ≤ MaxH`; caps under the balloon (`MaxW < 160`, `MaxH <
   42`); and `MinW ≥ footerRowMinWidth(keybindFooterLabels, tv.DefaultButtonGap)`.
2. **`TestKeybindingsDialogSpecIsTerminalIndependent`** — resolve the spec at
   80×24 / 120×30 / 200×50 / 300×80; assert the *spec* is identical across sizes
   (static → `dialog.Fit` is valid), as `TestCommandsDialogSpecIsTerminalIndependent`
   does.
3. **`TestKeybindingsDialogSize`** — mirror `TestCommandsDialogSize` exactly: drive the
   real spec through `tv.ResolveDialogRect` and assert **exact** resolved sizes per
   terminal, not loose bounds (the loose `height ≤ 40` of the earlier draft would have
   masked the `PrefH`-vs-`MaxH` misread). Cases: `40×16 → 58×16`, `80×24 → 62×20`,
   `120×40 → 62×34`, `200×50 → 62×34`, `300×80 → 62×34`. Plus two guard assertions
   that document the policy: at 200×50, `gotW >= 160 || gotH >= 42` fails ("the
   percentage balloon is back"), and `gotH >= spec.MaxH` fails ("PrefH 34, not MaxH
   40, should bound the height") — the direct analogue of
   `commands_dialog_issue448_test.go:173`.
4. **`TestKeybindingsDialogFooterFitsAtMinWidth`** — at the resolved floor width,
   assert `footerButtonRects(keybindFooterLabels, 2, width-3, y, tv.DefaultButtonGap)`
   yields three non-overlapping, non-clamped rects (mirror
   `TestCommandsDialogFooterFitsAtMinWidth`).
5. **`TestKeybindRowTextDoesNotTruncateLongChord`** — `applyBinding` an action to
   `tv.Chord{Rune: 'v', Ctrl: true, Shift: true}` (or use the shipped
   `actionWindowTileVertical` default), then assert `w.keybindRowText(a)` contains
   the full `"Ctrl+Shift+V"`, not `"Ctrl+Shift"` followed by a column boundary. Also
   assert the rendered row's display width ≤ the Tree inner width (`62 − 4 = 58`), so
   the tag is never Tree-clipped for a shipped binding. (No `Ctrl+Shift+PageDown`
   sub-assertion — that case is option (b)'s territory and is out of scope for the
   chosen option (a).)

All use the existing `newTestWorkbench(t)` helper. Run the dev gate from
[[dev-gate]] (build / vet / gofmt / golangci-lint v2 / `go test` **without** `-race`
on the Pi5).

---

## Open questions

1. **Chord column — confirm option (a) is acceptable.** I am **defaulting to (a)**
   (hardcoded `keybindChordColWidth = 14` + static `PreferredW: 62`): the minimal,
   house-style fix that resolves the reported bug (all shipped `Ctrl+Shift+*` defaults)
   and keeps the spec exactly testable. Its one edge is a *user* override wider than 14
   cells — none ship — which still clips the chord. If the maintainer wants Expected
   #3's literal "never truncated" to hold for arbitrary user chords, switch to
   **option (b)** (runtime column + content-derived `PreferredW = max(62, 4 + 30 +
   chordCol + len(tag)+3)`, capped at `MaxW 76`); that is a larger change with
   catalog-derived test expectations. Default (a) unless told otherwise.
2. **`PrefH: 34` vs a shorter `24`.** I chose 34 (27 visible rows) to avoid a
   list-visibility regression vs the current dialog's ~35 rows. A shorter `PrefH: 24`
   (17 rows, matching `watchersDialogSpec`) is a defensible alternative if the
   maintainer prefers a tighter modal and is comfortable relying on scroll. Either is
   correct; the choice is purely the intended visible-row count. Width (`PreferredW:
   62`) is not in question — it reproduces the issue's measured table and is the
   desired compact size; the dialog does not auto-widen to the catalog (that is
   option (b)'s content-derived `PreferredW`).
