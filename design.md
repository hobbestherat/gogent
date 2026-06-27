# Design — PASTECHIP-GOGENT (consumer half of issue #501)

Collapse multi-line pastes into an atomic, highlighted chip in the prompt box.
This is the **gogent consumer half** (~10% of the feature). The heavy lifting —
the atomic `[pasted N lines]` chip in `MultiLineInput`/`TextBox`, its
caret/backspace/selection/word-motion/wrap semantics, and the verbatim
`GetText()` expansion — is **already merged in turbotui**
(`54dc4b7`, PR #43). gogent only has to (a) theme the chip and (b) prove the
existing prompt-box interactions still hold.

---

## 0. Dependency bump (first implementation step, not done in the design phase)

```
go get github.com/hobbestherat/turbotui@54dc4b757884af611c16435085970b8ec516873c
go mod tidy
```

Current pin (`go.mod:8`) is `v0.3.1-0.20260627085642-0ff08b27a8c2` (commit
`0ff08b2`, the MenuBar status-slot PR #42). The target `54dc4b7` is one commit
later and adds, on `turbotv.Theme`, two new roles:

```go
// turbotui turbotv/theme.go:48-54
PasteChipBG tui.Color
PasteChipFG tui.Color
```

Confirmed field names: **`PasteChipBG` / `PasteChipFG`** (verified in the
read-only clone at `$HOME/work/turbotui/turbotv/theme.go`). turbotui's own
defaults: `DefaultTheme` → BG magenta `ANSI 5`, FG white `ANSI 15`;
`HighContrastTheme` → BG bright-yellow `ANSI 11`, FG black `ANSI 0`. The widget
reads `activeTheme.PasteChipFG/BG` at draw time
(`widget_multiline_input.go:208`), so installing the roles onto the active
`tv.DefaultTheme` (which `tv.SetTheme` propagates) is all that is needed for the
chip to pick up gogent's palette.

---

## Files touched (gogent only — no turbotui change)

| File | Change |
|------|--------|
| `go.mod` / `go.sum` | bump turbotui to `54dc4b7`, `go mod tidy` |
| `ui/tui/theme.go` | add `PasteChipFG/BG` as first-class gogent theme roles, mirroring `TextSelectionFG/BG` (issue #279) |
| `ui/tui/theme_issue501_test.go` *(new)* | theme-wiring assertions (defaults distinct + readable, `ApplyTheme` install, NO_COLOR degrade, override round-trip) |
| `ui/tui/pastechip_prompt_issue501_test.go` *(new)* | one integration test: multi-line paste → chip, `GetText()` verbatim, submit sends verbatim, @mention/slash/history unaffected |

No other production file changes. The submit/interject path, the mention
completer, slash detection, history recall and the typing-idle drain are all
**unchanged** — section 3 shows why each is already correct via `GetText()`
expansion / the sentinel rune being an ordinary non-`@`, non-`/` rune.

---

## 1. Theme wiring (`ui/tui/theme.go`) — the only production change

gogent has its **own** `Theme` struct (separate from `turbotv.Theme`);
`ApplyTheme` copies resolved roles onto the shared `tv.DefaultTheme`. I follow
the established `TextSelectionFG/BG` (#279) pattern exactly, because that pattern
already gives us level-degradation, NO_COLOR handling, config override and the
install seam for free.

### 1a. Struct field (next to `TextSelectionFG/BG`, ~L321)
```go
// PasteChipFG/BG paint the collapsed "[pasted N lines]" chip inside the prompt
// box (turbotui MultiLineInput/TextBox; issue #501). Like TextSelection* the chip
// is drawn over the focused-input fill, so it must contrast with InputFocusBG; it
// is kept distinct from TextSelectionBG too so a *selected* chip still reads as a
// chip. ApplyTheme installs them onto tv.DefaultTheme's PasteChip* slots.
PasteChipFG tui.Color
PasteChipBG tui.Color
```

### 1b. Per-palette values (the "two PasteChip lines per theme")

Chosen so each is a **muted accent distinct from both `InputFocusBG` (the focus
fill) and `TextSelectionBG`**, and clears the WCAG AA body-text tier
(`minContrastText`, 4.5:1) FG-on-BG. Contrast figures computed with the file's
own `relativeLuminance`/`contrastRatio` over the canonical ANSI table.

- **default** (`defaultPalette`, ~L450): `InputFocusBG = ANSI 6` (cyan),
  `TextSelectionBG = ANSI 0` (black).
  → `PasteChipBG: tui.ANSIColor(5)` (dark magenta), `PasteChipFG: tui.ANSIColor(15)` (white).
  White-on-magenta ≈ **6.4:1** ✓. Distinct from cyan focus and black selection ✓.
  Matches turbotui's own default chip hue, so 16-colour fidelity-invariant.

- **high-contrast** (`highContrastPalette`, ~L536): `InputFocusBG = okabeYellow`,
  `TextSelectionBG = black`.
  → `PasteChipBG: okabePurple` (`0xCC79A7`), `PasteChipFG: black` (`tui.RGBColor(0,0,0)`).
  Black-on-purple ≈ **6.9:1** ✓. Purple ≠ yellow focus, ≠ black selection;
  Okabe–Ito hue → colour-blind-safe. (White FG would fail at 3.1:1 — BG is mid-luminance, so **black** FG.)

- **dark** (`darkPalette`, ~L610): `InputFocusBG = amber 0xE0AF68`,
  `TextSelectionBG = black`.
  → `PasteChipBG: tui.RGBColor(0xC6,0x8F,0xD6)` (soft mauve, the palette's Result tone),
  `PasteChipFG: black`. Black-on-mauve ≈ **8.3:1** ✓, cohesive with the dark
  aesthetic, distinct from amber focus and black selection. (White FG ≈ 2.6:1 — fails — so **black** FG.)

All three degrade cleanly: the two RGB picks both quantise toward light grey at
`Color16` (`rgbTo16` nearest = `ANSI 7`), keeping black FG ≈ 9:1, so no
16-colour terminal renders an illegible chip.

### 1c. `ResolveTheme` degrade (~L671, beside the TextSelection lines)
```go
t.PasteChipFG = degrade(t.PasteChipFG, level)
t.PasteChipBG = degrade(t.PasteChipBG, level)
```
Required, not optional — without it an RGB chip colour would be emitted raw to a
16-/256-colour terminal.

### 1d. `applyOverrides` (~L779) — `case "paste_chip_fg":` / `case "paste_chip_bg":`
Keeps the config-override map complete and consistent with every other role.

### 1e. `ApplyTheme` install (~L1173, right after the TextSelection* install)
```go
tv.DefaultTheme.PasteChipFG = t.PasteChipFG
tv.DefaultTheme.PasteChipBG = t.PasteChipBG
```
This single unconditional assignment is sufficient for **all three chrome
paths**: it runs *after* the `switch` has chosen `baseTVTheme` /
`blackCanvasTVTheme` / `neutralTVTheme`, so it overwrites whatever (if anything)
those builders left in the slot — exactly as the Window*/MenuBar*/TextSelection*
installs already do. `tv.SetTheme(tv.DefaultTheme)` at the end of `ApplyTheme`
propagates it to live widgets, so a runtime theme switch recolours the chip
without a restart. Under NO_COLOR both roles have degraded to the terminal
default, so the install is colour-neutral.

> **Footprint:** I deliberately do **not** touch
> `neutralTVTheme`/`blackCanvasTVTheme` (the unconditional install already covers
> them). Whether to add a `paste-chip` finding to `paletteContrast` is a separate
> judgment call deferred to Open Q 1 (the rationale there is the
> findings-contract-test churn, **not** the #500g overlap, which is negligible for
> an additive line). Every gogent edit sits adjacent to the existing
> TextSelection* lines, so the (low-likelihood) #500g rebase is mechanical.

---

## 2. Behaviour the user sees

- Paste a single line → inserted literally, unchanged (turbotui only chips a
  paste *containing a newline* — `widget_textbox.go:440`,
  `widget_multiline_input.go:527`).
- Paste multi-line text → collapses to one highlighted `[pasted N lines]` chip,
  atomic to caret/backspace/selection (turbotui), now painted in gogent's
  palette (magenta / Okabe purple / mauve per theme).
- Submit / Interject send the original text with **newlines intact** — because
  `submit()` (`session_window.go:401`) and `interject()`
  (`session_window.go:~1343`) read `sw.input.GetText()`, which expands every chip
  (`widget_multiline_input.go:111-117`). **No submit-path change.**

> **"Verbatim" caveat.** turbotui's `handlePaste` stores `sanitizePaste(text)`
> (`paste_chip.go:167-187`): CRLF→LF (`\r` dropped), other control runes `<0x20`
> stripped, any pre-existing PUA sentinels removed. So a `\r\n` paste round-trips
> as `\n`. This is turbotui's behaviour and is **identical for the chip and the
> literal single-line paste paths**, so it is not a gogent regression and is out
> of gogent's scope (merged + tested in turbotui). "Verbatim" throughout this doc
> means "verbatim modulo turbotui's paste sanitisation", inheriting the wording
> from the acceptance criteria.

---

## 3. Regression analysis (gate 3) — why each interaction is already correct

The chip is a single **sentinel rune** in the Private-Use plane
(`IsPasteChipRune`, range `0xF0000–0xFFFFD`); it is *not* `@`, not `/`, occupies
**one rune** in `Lines[CursorY]`, and `GetText()` expands it.

1. **@-file mention completer** (`mention_completer.go`,
   `mentions.go::mentionToken`): `update()` parses the *raw* buffer line
   `in.Lines[in.CursorY]` and rune-index `in.CursorX` — not `GetText()`. Be
   precise about how `mentionToken` (`mentions.go:60-69`) scans: walking back from
   `cursorX-1` it special-cases **only** two runes — `@` opens a token, any
   `unicode.IsSpace` closes (returns `ok=false`). Every *other* rune is **folded
   into the partial query** `string(line[start+1:cursorX])`, **not** treated as a
   boundary. The chip sentinel is neither `@` nor whitespace, so:
     - **Lone chip / chip not after an active `@`** → no `@` precedes the cursor →
       completer inactive. Correct, and the common case.
     - **Chip *adjacent inside* an active mention** (e.g. type `@fi`, paste a
       block, keep typing, cursor at end → `@fi⧉le`) → `mentionToken` returns a
       query string **containing the PUA sentinel**, `filterPaths` finds no match,
       and the popup shows empty. This is a **new, cosmetic, popup-only** effect
       (no PUA rune could appear in `Lines` before #501): no crash, no corruption,
       and `accept()` rune-slices around `[spanStart:spanEnd]` so the sentinel
       outside the replaced span is preserved (`mention_completer.go:286-289`) and
       `GetText()` still expands. It is self-correcting (more typing / a cursor
       move re-resolves) and requires contrived chip-mid-mention adjacency.
   `slashMatches` is unaffected (`line[0] != '/'`; sentinel ≠ `/`). Cursor math is
   in rune units and the chip is one rune, so no off-by-N. → **no production
   change**; covered by two assertions (lone-chip inactive *and* adjacent-chip
   yields a non-matching/sentinel-bearing query, not a real path completion).

2. **Slash commands** (`/stop`, `/undo`, `/review-*`): detected from
   `GetText()` (and `slashMatches` from the raw line). A chip at the start is a
   PUA rune, never `/`. → confirmed, no change.

3. **History recall** (`session_window.go:1470-1497`): prompts are stored via
   `GetText()` (verbatim, newlines intact) and restored via **`SetText()`**.
   `SetText` splits on `\n` into editable lines and **deliberately does not
   chip-ify** (`widget_multiline_input.go:119-133`). So a recalled multi-line
   prompt round-trips **content-faithfully** (`GetText` after `SetText` ==
   stored text) but is shown as editable lines, **not** re-collapsed into a
   chip.
   > **Correction to the task brief:** the brief says "SetText with newlines
   > recreates a chip" — it does **not**. Re-chipping on recall is
   > `SetTextChip`'s job, and turbotui *intentionally* keeps `SetText` literal so
   > hand-typed multi-line history stays editable. Leaving gogent on `SetText`
   > is therefore both correct and the documented design intent; it is **not** a
   > regression (pre-#501 there were no chips, and multi-line recall always
   > spilled into lines). Switching recall to `SetTextChip` would wrongly chip
   > every hand-typed multi-line prompt — explicitly out of scope. → no change.
   > (Verified: `SetTextChip` has **0 call sites anywhere in gogent**.)

4. **submit() / interject()** read `GetText()` (`session_window.go:401` and
   `~1343`) → chips expand → original text sent (newlines intact; see the
   "verbatim" caveat in §2). No change expected or made.

5. **Typing-idle / deferred-modal drain** (`session_window.go:383-397`): the
   trigger is the non-empty→empty edge `before != "" && input.GetText() == ""`.
   Deleting a chip (backspace removes the whole chip atomically, turbotui) empties
   the buffer; `before` held the chip's expanded (non-empty) text, so the edge
   fires exactly once. → confirmed, no change.

6. **WordWrap** (`session_window.go:269`, `input.WordWrap = true`): turbotui
   guarantees a chip is never split across a wrap
   (`TestPasteChipWrapDoesNotSplitChip`). → no gogent change.

---

## 4. Tests (gate 1 scope-faithful, not over-engineered)

**`ui/tui/theme_issue501_test.go`** — the real gogent change:
- `for each palette {default,high-contrast,dark}`: `PasteChipBG != InputFocusBG`,
  `PasteChipBG != TextSelectionBG`, and
  `contrastRatio(PasteChipFG, PasteChipBG) >= minContrastText`
  (mirrors `TestRoles279DefaultsInvertInputFocusFill`).
- `ApplyTheme` installs `PasteChipFG/BG` onto `tv.DefaultTheme` for every palette
  (mirrors `TestRoles279_291ApplyThemeInstalls`; wrap in `withThemeRestore(t)`).
- NO_COLOR → both roles degrade to `tui.DefaultColor()`.
- override round-trip for `paste_chip_fg`/`paste_chip_bg` through
  `buildThemeConfig`/`editedTheme` (mirrors `TestRoles279_291OverrideRoundTrip`).

**`ui/tui/pastechip_prompt_issue501_test.go`** — one focused integration test on
a real `sw.input`:
- Drive a multi-line paste (via the input's `OnPasteFn`/paste event); assert the
  buffer holds exactly one `IsPasteChipRune` rune **and** `GetText()` returns the
  verbatim original (incl. newlines).
- Assert the gogent **submit path** delivers that verbatim text (capture via the
  session's submit handler / queued-input seam).
- Assert a chip at line start is **not** taken as a slash command, and a **lone**
  chip is **not** parsed as an @mention (drive `completer.update()`, expect
  inactive).
- Assert the **adjacent-chip** edge (§3.1): with a buffer like `@fi⧉le` and the
  cursor at end, `mentionToken(line, cursorX)` returns a query that **contains the
  sentinel rune** (or, equivalently, `filterPaths` yields no real path) — i.e. the
  completer never offers a bogus completion and never crashes. This pins the
  cosmetic-only nature of the new behavior rather than leaving it unasserted.
- History round-trip: `GetText()` → store → `SetText()` → `GetText()` equals the
  original (content faithful; not asserting a chip is recreated, per §3.3).

Keep both test files free of `internal/*` imports beyond what `ui/tui` tests
already use (`gogent/internal/config`).

---

## 5. The four gates

1. **Goal match** — exactly the consumer half: theme the merged turbotui chip
   for all gogent palettes + verify interactions. No new widget, no submit-path
   change, no scope creep into turbotui.
2. **Usability** — chip themed consistently with `InputFocus*`/`TextSelection*`
   (muted accent, AA-readable, distinct so a selected chip still reads as a
   chip); paste shows a chip, submit sends the original (newlines intact);
   @mention / slash / history / submit / typing-idle all behave as before.
3. **No regressions** — only additive theme roles + tests; `gofmt`/`build`/`vet`
   clean; new contrast assertions enforce legibility; `go test ./...` green
   (pre-existing `TestUserSessionSendMessage` 404 the sole accepted failure);
   `go.mod` bumped, `go mod tidy` clean; no `-race`. The one genuinely new
   behaviour #501 introduces in the prompt box — a chip sentinel landing *inside*
   an active `@`-mention query (§3.1) — is **cosmetic and popup-only** (empty
   completion list, self-correcting, no crash/corruption/submit impact), not a
   functional regression, and is pinned by an explicit test.
4. **Holistic across both repos** — the seam is respected: turbotui owns the chip
   *mechanics* and exposes `PasteChip*` roles; gogent owns the *palette* and only
   maps those roles, exactly as it already does for every turbotui role
   (#243/#260/#265/#279/#291/#327). The install reuses the existing
   `ApplyTheme`→`tv.SetTheme` path. Downstream effect on turbotui: none (read-only
   consumer). The one cross-repo subtlety — `SetText` vs `SetTextChip` on recall —
   is resolved in favour of turbotui's documented intent (§3.3).

---

## Open questions

1. **`paletteContrast` audit finding.** Every prior role added a finding to the
   central audit *and* updated `TestIssue202DefaultPaletteFindingsContract` (an
   exact-set test) plus the all-fidelities default audit. I propose **not** adding
   a `paste-chip` finding — the dedicated contrast assertion in
   `theme_issue501_test.go` already enforces `>= minContrastText` for all three
   palettes, so legibility is guaranteed; skipping the central finding only avoids
   the findings-contract-test churn. The reason is that test churn, **not** #500g
   overlap (an additive `finding(...)` line does not collide with the
   status-indicator work — I withdraw that argument). If the maintainer prefers
   full parity, it is one line —
   `finding("paste-chip", t.PasteChipFG, t.PasteChipBG, minContrastText)` — plus
   the contract-count bump, and my chosen colours already clear it at every
   fidelity (default ANSI is fidelity-invariant; the HC/dark RGB picks both
   quantise to `ANSI 7` grey at `Color16`, keeping black-FG ≈ 9:1).
   **Recommend: skip; trivially reversible if requested.**
2. **Theme editor exposure.** I propose **not** adding `paste_chip_*` to
   `themeRoles` (the scrolling editor); config override still works (precedented —
   `button_focus_*`/`input_focus_*` are config-only, carried by
   `carryUnexposedOverrides`). Add later if desired (also needs an editor label
   string). **Recommend: skip for now.**
3. **Recall-as-chip UX.** Confirmed out of scope (§3.3) — flag only if the
   maintainer actually wants recalled pastes to re-collapse (would require
   `SetTextChip` and contradicts turbotui's editable-recall intent).
