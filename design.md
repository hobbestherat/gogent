# Design — Issue #548: Theme editor "Save As…" button caption truncated to "Save A…"

## Summary
The Themes editor's "Save As…" footer button is declared one column too narrow.
turbotui's `Button.draw` wraps every caption in `"[ … ]"` (2 cols left + 2 cols
right) and ellipsises any caption that doesn't fit the remaining width. With the
button at `W:11` and label `"Save As…"` measuring 8 columns
(`…` U+2026 is a single column), only `11-4 = 7` columns remain for the caption,
so it is truncated to `"Save A…"` and rendered as `[ Save A… ]`. The fix widens
the button to `W:12` (`8 + 4 = 12`) in the three gogent sites that hard-code the
geometry. This is a pure UI fix — no behaviour change, no new dependency, no
turbotui change.

## Root cause (verified against pinned turbotui)
`$HOME/work/turbotui/turbotv/widget_button.go` `Button.draw` (lines 99–141):

```
boxLeft, boxRight := "[ ", " ]"          // unfocused chrome
left, right := boxLeft, boxRight
if component.Focused() { left, right = "►", "◄" }   // focused: 1 col each
leftW  := StringWidth(left)              // 2 unfocused / 1 focused
rightW := StringWidth(right)             // 2 unfocused / 1 focused
avail  := face.W - leftW - rightW
captionW := StringWidth(clean)           // "Save As…" = 8
if captionW > avail { captionW = avail } // -> drawMnemonicClipped truncates
```

The **unfocused** state (`avail = W-4`) is the binding constraint:
| W  | avail (unfocused) | caption fits? | rendered |
|----|-------------------|---------------|----------|
| 11 | 7                 | no (8 > 7)    | `[ Save A… ]` |
| 12 | 8                 | yes           | `[ Save As… ]` |
| 13 | 9                 | yes           | `[ Save As… ]` |

The focused state uses single-column chevrons (`avail = W-2 = 10` at W=12), which
is strictly more forgiving, so W:12 satisfies both states. turbotui's truncation
is correct *by design* (`drawMnemonicClipped` -> `Truncate`); the bug is purely
that gogent hands the button a Rect one column too narrow.

## Files & exact changes (gogent only)

### 1. `ui/tui/theme_editor.go` — initial button creation (line 1205)
```go
saveAsBtn = newButton("Save As…", tv.Rect{X: 12, Y: height - 3, W: 11, H: 1}, saveAs)
```
→ change `W: 11` to `W: 12`.

### 2. `ui/tui/theme_editor.go` — `relayout` resize handler (line 1264)
```go
saveAsBtn.Root().SetBounds(tv.Rect{X: 12, Y: nh - 3, W: 11, H: 1})
```
→ change `W: 11` to `W: 12`. (Keeps a resized dialog identical to a freshly-opened
one — issue #317 invariant.)

### 3. `ui/tui/theme_issue462_test.go` — `themeEditorFooterButtonRect` (line 130)
```go
case "Save As…":
    return tv.Rect{X: 12, Y: y, W: 11, H: 1}
```
→ change `W: 11` to `W: 12`. This helper *pins* the expected laid-out rect; the
#462 tests match buttons by rect (because `"Save"` is a substring of `"Save As…"`),
so the helper must track the new width or `clickThemeButton`/`TestIssue462*` fail.

No other edits. `X:12` and `Y` are unchanged in all three; only `W` moves 11→12.

## Layout / collision check (verified)
Footer buttons (window-relative), after the change:
| Button   | X        | W  | Columns occupied |
|----------|----------|----|------------------|
| Reset    | 2        | 9  | 2–10  |
| Save As… | 12       | 12 | 12–23 |
| Delete   | 24       | 10 | 24–33 |
| Save     | W-24     | 9  | (right-anchored) |
| Cancel   | W-13     | 10 | (right-anchored) |

Save As… now spans columns 12–23 inclusive; Delete starts at X:24 → **flush
adjacent, zero overlap, zero gap**. Previously (W:11) cols were 12–22 with a
1-col gap at col 23; the fix consumes that slack gap, which is exactly the column
the caption needed. No change to Delete is required. The left-anchored group
(Reset/SaveAs/Delete ends at col 33) and the right-anchored group (Save/Cancel)
do not meet at the 83-col dialog floor (right group starts at 83-24 = col 59),
so there is no left/right collision either. No other footer button is affected
(all satisfy `captionW + 4 ≤ W`).

## User-facing behavior
- Before: button reads `[ Save A… ]` at the 83×22 floor and every larger size —
  illegible, and the copy-theme workflow (issue #462: use a built-in or saved ★
  theme as the base for a new named theme) is effectively invisible.
- After: button reads `[ Save As… ]` at all sizes. Clicking it still opens the
  name-input dialog; a fresh name still creates a new ★ saved theme copied from
  the current selection, selects it, and applies it live. **No interaction change
  — only the rendered caption width changes.**

## Design criteria

### (1) Goal match
Issue #548 asks for a *fix*: the truncated caption. The change is exactly the
3-column-width correction needed to stop truncation — no scope creep (no new
button, no relabel, no behaviour change) and it does not miss the ask (it restores
both legibility *and* the discoverability of the #462 copy-theme path the issue
flags). Minimum correct width `W = captionW(8) + chrome(4) = 12`.

### (2) Usability
The label becomes fully legible (`[ Save As… ]`), making the only non-destructive
copy-and-modify entry point visible. The interaction (click → name input → new ★
theme, applied live) is unchanged and continues to behave as the user expects.
Nothing is silently dropped; the surfaced control now matches its function.

### (3) No regressions
- Only `W` changes (11→12) in three hard-coded geometry sites; X/Y/H and all
  closures (`saveAs`, OnPress wiring) untouched.
- The resize handler is updated in lockstep with creation, preserving the #317
  "resized dialog == freshly-opened dialog" invariant.
- The test helper `themeEditorFooterButtonRect` is updated so `clickThemeButton`
  still resolves Save As… by rect; #462 tests (`TestIssue462SaveAsCreatesNamedCopy`,
  the built-ins-only editor test, `saveAsViaUI`) keep matching and stay green.
- No footer overlap (table above): Delete at X:24 is flush, not overlapping.
- Other footer buttons unchanged. gofmt/build/vet/golangci-lint expected clean
  (numeric-literal-only change). `go test ./...` expected green (the pre-existing
  `TestUserSessionSendMessage` 404 is the only accepted unrelated failure).

### (4) Holistic design across both repos
The bug is in gogent: it sizes the Rect. turbotui's truncation
(`drawMnemonicClipped` → `Truncate`) is correct by design and must NOT change —
altering it would mask undersized buttons everywhere and break the "face.W alone
controls the box width" contract (gogent#259) that other buttons rely on. So the
fix lives entirely on the gogent side of the seam, in the layout code that owns
the geometry. No `go.mod` bump, no new dependency, no turbotui edit. The change is
confined to `ui/tui/theme_editor.go` (3 width edits) + `ui/tui/theme_issue462_test.go`
(1 helper edit). File-disjoint from in-flight #542 and queued #543/#544; at the
gate, rebase onto current `origin/main` (only adjacent ui/tui files may have moved,
no overlap with `theme_editor.go`).

## Verification plan (implementation phase)
1. Apply the three `W:11`→`W:12` edits.
2. `gofmt -l ui/tui/theme_editor.go ui/tui/theme_issue462_test.go` → empty.
3. `go build ./...` and `go vet ./ui/tui/...` → clean.
4. `go test ./ui/tui/...` → green (issue #462 + theme-editor tests).
5. `go test ./...` → green except the accepted pre-existing
   `TestUserSessionSendMessage` 404.
6. golangci-lint whole-repo → 0 new issues.
7. (Optional) assert the rendered caption at the 83×22 floor is `[ Save As… ]`
   if an existing render-harness test makes that cheap; otherwise the width math +
   updated rect helper are sufficient given turbotui's verified draw logic.

## Open questions
- **1-col gap vs flush adjacency between Save As… and Delete.** With W:12 the two
  buttons are flush (cols 12–23 / 24–33); previously there was a 1-col gap at col
  23. The task explicitly states leaving Delete at X:24 (flush) is acceptable and
  collision-free, and matching the spacing by shifting Delete to X:25 is only
  cosmetic. **Recommendation: leave Delete at X:24** — it keeps the diff to the
  three width edits, avoids touching Delete's three geometry sites, and the
  Reset→SaveAs pair (cols 2–10 / 12–23) already has a 1-col gap, so a flush
  SaveAs→Delete pair is the only minor asymmetry; not worth widening the change.
  Flag for maintainer (kloune) if pixel-exact footer spacing is desired.
