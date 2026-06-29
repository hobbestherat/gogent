# Design — Models dialog: 1-row footer buttons + blank separator line above (gogent #585)

## Issue
In the Models… dialog the footer action buttons (Add from Catalog… / Add Empty… /
Edit… / Remove / Set Default / Done) currently render **2 rows tall** (introduced by
#529 via `footerButtonRectsH(..., 2)`). Revert them to **1 row tall** and insert a
single **blank separator row directly above** the button row so the buttons are
visually detached from the list/hint above. The dialog's total height must stay the
same as today (`paneRows + 8`): the button row shrinks by one row, the blank
separator adds one row back — they cancel.

This is a **FIX** (revert + cosmetic separator), gogent-only, no new deps, no turbotui
change.

## Current state (verified in source)
`ui/tui/model_dialog.go`, `showModelsDialog()`:
- `height0 := paneRows + 8` (line ~200)
- `hintY := height - 5` (line ~220)
- `buttonY := height - 4` (line ~221) — 2-row buttons occupy rows `height-4` and `height-3`
- `paneH := height - listY - 6` (line ~222), `listY = 2`
- `footer := footerButtonRectsH(labels, 2, width-3, buttonY, tv.DefaultButtonGap, 2)` (line ~261)

Layout today (height = paneRows+8, listY=2, paneH=paneRows):
```
row 0           : top border
rows 2..paneRows+1 : list pane (paneH = paneRows rows)
height-5 (=paneRows+3) : hint text
height-4 (=paneRows+4) : button row, top half   ┐ 2-row buttons
height-3 (=paneRows+5) : button row, bottom half ┘ (caption on lower row)
height-2 (=paneRows+6) : bottom border
height-1 (=paneRows+7) : drop shadow (when enabled)
```

## Target state
```
row 0           : top border
rows 2..paneRows+1 : list pane (paneH = paneRows rows)  ← UNCHANGED
height-5 (=paneRows+3) : hint text                       ← UNCHANGED (hintY = height-5)
height-4 (=paneRows+4) : BLANK separator (not painted)   ← NEW empty line above buttons
height-3 (=paneRows+5) : 1-row action buttons            ← buttonY = height-3
height-2 (=paneRows+6) : bottom border                   ← UNCHANGED
height-1 (=paneRows+7) : drop shadow (when enabled)      ← UNCHANGED
```
`height0 = paneRows + 8` is **unchanged** — the shrink (−1 button row) and the add
(+1 blank row) cancel exactly. `paneH = height - listY - 6` is unchanged, so the list
still shows `paneRows` rows. The blank row at `height-4` is simply a row on which
nothing is added to the dialog window — turbotui paints it as dialog interior background.

Reference convention: `ui/tui/statistics_dialog.go` `showStatisticsDialog()` already
uses the 1-row `footerButtonRects(...)` with `buttonY = height-3` (buttons flush on the
last interior row). Models… differs only in that Statistics has `hintY = height-4`
(hint directly above buttons, no gap), whereas Models keeps `hintY = height-5` so that
`height-4` is the requested blank separator. Border convention is shared: interior runs
rows `1 .. height-3` inclusive; bottom border at `height-2`; `height-1` is the shadow.

## Exact edits

### gogent — `ui/tui/model_dialog.go` (`showModelsDialog()` only)
1. **Footer call → 1 row.** Replace
   `footer := footerButtonRectsH(labels, 2, width-3, buttonY, tv.DefaultButtonGap, 2)`
   with
   `footer := footerButtonRects(labels, 2, width-3, buttonY, tv.DefaultButtonGap)`.
   Rewrite the preceding comment (currently "2-row-tall footer buttons (issue #529)…")
   to describe the new shape: 1-row footer flush on the last interior row with a blank
   separator row above it, referencing #585.
2. **`buttonY := height - 3`** (was `height - 4`).
3. **`hintY := height - 5`** stays. Update the multi-line comment above `hintY`
   (currently explaining the 2-row footer geometry) to describe: hint at `height-5`,
   blank separator at `height-4`, 1-row buttons flush at `height-3`, list keeps
   `paneRows` rows, dialog height unchanged at `paneRows+8`. Explicitly note that
   `height-4` is intentionally left unpainted (the requested empty line) — no
   `dialogLabel` is added there.
4. **`paneH := height - listY - 6`** stays (list height unchanged).
5. **`height0 := paneRows + 8`** stays. Update its comment (currently "footer buttons
   render 2 rows tall (issue #529)…") to explain the height is unchanged because the
   1-row footer + blank separator together occupy the same two rows the 2-row footer
   used.

No other functions in this file change. `footerButtonRectsH` in
`ui/tui/dialog_buttons.go` is **kept** (general primitive, unit-tested elsewhere); only
the Models… call site flips to the 1-row `footerButtonRects` wrapper.

### gogent — `ui/tui/models_dialog_issue529_test.go`
Flip the 2-row assertions to 1-row, keep all height expectations, add a blank-separator
test. The file's header comment block (criteria 1–4) must be updated to describe the
1-row-footer-with-blank-separator design:
- `TestModelsDialogFooterButtonsAreTwoRowsTall` (5-button, no catalog): assert
  `H == 1`. Rename to `...AreOneRowTall` (and the all-six variant likewise) for clarity;
  at minimum fix the assertion + message.
- `TestModelsDialogAllSixFooterButtonsTwoRowsTall` (full 6-button): assert `H == 1`.
- `TestModelsDialogHeightIsPaneRowsPlusEight`: **unchanged** expectation (`paneRows + 8`);
  re-verify the comment rationale (height is preserved because shrink+add cancel).
- `TestModelsDialogFooterGeometryNoBorderClashNoOverlap`: change `H != 2` → `H != 1`;
  verify each button's single row lies inside the interior (`topInterior..lastInterior`)
  and above the bottom border; verify the list bottom row ends above the blank/hint rows
  (`listBottom < buttonY`, and ideally `listBottom < hintY`).
- `TestModelsDialogEmptyListTwoRowButtons` (empty list → Add Empty + Done): each button
  `H == 1`, dialog height still `9` (paneRows 1 + 8), placeholder present. Consider
  renaming to drop "TwoRow".
- `TestModelsDialogTwoRowButtonsSurviveResize`: buttons stay `H == 1`, inside interior,
  height pinned across resizes; recentering check unchanged.
- `TestModelsDialogTwoRowFooterOnTinyTerminal`: buttons `H == 1`, inside interior, no
  panic at tiny terminals; height still pinned to 9.
- `TestPeerDialogsKeepOneRowFooter`: **UNCHANGED** (peer dialogs already 1-row).
- **NEW** `TestModelsDialogBlankSeparatorRowAboveButtons` (acceptance criterion): assert
  the row directly above the button row (`buttonY - 1 == height - 4`) contains **no
  button** (no footer button has that Y), **no hint label** (hint is at `height-5`), and
  **no list content** (list bottom row < `height-4`) — i.e. a blank row strictly inside
  the dialog interior. Derive `buttonY` from the footer buttons' bounds and `height`
  from the dialog bounds so the test is robust to recentering.

### turbotui
**No change.** `footerButtonRects` and the `H == 1` `Button.draw` path already exist and
are the default. The seam between repos is respected: gogent chooses geometry (1-row
rects via the gogent-local `footerButtonRects` wrapper); turbotui renders whatever
`Rect.H` it is handed.

## Design criteria

### (1) Goal match
Exactly the issue's ask: Models… footer buttons 1 row tall via `footerButtonRects`
(not `footerButtonRectsH`), `buttonY = height-3`, blank separator at `height-4`, hint at
`height-5`, dialog height `paneRows+8` unchanged. Pure revert + one cosmetic blank row —
no scope creep, no feature work, the `footerButtonRectsH` primitive is preserved.

### (2) Usability
The blank line creates clear visual separation between the list/hint and the single-row
action button strip, matching the established 1-row-footer dialog look (Statistics,
Sessions, Watchers). Buttons sit flush on the last interior row, their captions on a
single line as users expect from every other dialog. No interaction/keyboard behaviour
changes (focus order, Enter=Edit, Esc=close, Tab navigation all untouched).

### (3) No regressions
- `footerButtonRectsH` kept and still exercised by `dialog_buttons_issue529_test.go` /
  `dialog_buttons_test.go` (these stay green unchanged).
- Only the Models… call site changes; Statistics/Sessions/Watchers/peer dialogs
  untouched and already 1-row (`TestPeerDialogsKeepOneRowFooter` unchanged).
- Dialog height invariant (`paneRows+8`, `MinH==MaxH`) preserved, so resize/tiny-terminal
  pinning behaviour is identical; list pane height (`paneRows`) preserved.
- Empty-list state (2 buttons, height 9, placeholder) preserved.
- gofmt/build/vet/golangci-lint clean; `go test ./...` green (known-acceptable:
  pre-existing `TestUserSessionSendMessage` 404; load-induced
  `internal/daemon TestStopGracefulAndForced` flake passes in isolation). No forbidden
  imports added to `ui/tui`.
- Test command: `go test ./ui/tui/ -run 'ModelsDialog|FooterButton|PeerDialog'` and
  `go vet ./ui/tui/`.

### (4) Holistic design across both repos
Change lives entirely in gogent's `ui/tui` lane (`model_dialog.go` +
`models_dialog_issue529_test.go`) — the correct place, since dialog geometry is a
gogent concern. The repo seam is honoured: turbotui already supports both 1- and
N-row buttons via `Rect.H`; gogent simply selects `H==1` again. No downstream effect on
turbotui, no new dependency. Conflict-free with internal/model+agent+tool lanes;
collides only with other ui/tui work, so dispatch when the ui/tui lane is clear; rebase
onto current `origin/main` at the gate. PR body: "Closes #585".

## Regression risks & mitigations
- **Stale comments referencing #529's 2-row footer.** Three comment blocks
  (height0, hintY geometry, footer call) describe the old design; all must be rewritten
  or they will mislead future readers. Mitigation: update all three as part of the edit.
- **Off-by-one putting buttons on the bottom border or list overlapping the blank row.**
  Mitigated by `TestModelsDialogFooterGeometryNoBorderClashNoOverlap` (interior + no-overlap
  checks) and the new blank-separator test.
- **Tiny-terminal clamp.** `footerButtonRects` delegates to `footerButtonRectsH(...,1)`,
  whose `clampDialogRect` safety net is identical to the 2-row path, so no new panic
  surface; covered by `TestModelsDialogTwoRowFooterOnTinyTerminal`.
- **Blank row vs. the existing gap between list and hint.** Row `height-6`
  (`paneRows+2`) is already blank in both old and new layouts (list ends at
  `paneRows+1`, hint at `paneRows+3`); the *new* separator the issue asks for is
  specifically `height-4` (between hint and buttons). The new test pins `height-4`, not
  `height-6`, so it asserts the intended row.

## Open questions
- **Test renames vs. assertion-only fixes.** The task allows either renaming the
  "TwoRow" tests to "OneRow"/"AreOneRowTall" or just flipping the assertions. Plan:
  rename for clarity (the names would otherwise lie), keeping the same coverage. If the
  maintainer prefers minimal churn, fall back to assertion-only edits — non-blocking.
- **None affecting layout.** The geometry resolves cleanly to `paneRows+8`; no height0
  adjustment is needed.
