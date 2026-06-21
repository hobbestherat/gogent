# Issue #203 — Up/Down prompt-history recall in the session input

## Goal
Shell/readline-style command history on the session input (`tv.MultiLineInput`):
- **Up** → previous (older) submitted prompt, caret at end.
- **Down** → next (newer) submitted prompt, caret at end; Down past the newest restores the stashed in-progress draft.
- Edge-sensitive so multi-line editing and the @-mention popup still work.

## State (on `SessionWindow`)
- `promptHistory []string` — user-typed submitted prompts, oldest→newest (in-memory, per session).
- `historyNav int` — navigation cursor. `historyNav == len(promptHistory)` means "not navigating / at the draft". `len-1` is newest, `0` is oldest.
- `historyDraft string` — the in-progress text stashed when navigation begins, restored by Down past the newest.

## Capture (submit path)
One append point at the top of the `submit` closure, after the empty-text guard:
`if !sw.nudgingSend && !sw.draining { sw.recordHistory(text) }`
- Excludes supervisor nudges (`nudgingSend`).
- `draining` guard prevents double-recording a queued message (it is recorded once when the user presses Enter while busy; the drain re-entry is skipped).
- Records the raw typed text **before** mention expansion; **includes** slash commands.
- `recordHistory` appends (skipping a consecutive duplicate), then resets `historyNav` to `len` and clears the draft.

## Conflict resolution / precedence (in the input `OnTypeFn` wrapper)
1. `completer.handleKey` first (popup keeps Up/Down) — unchanged.
2. `handleHistoryKey(event)`: Up recalls older only when the caret is on the **first visual line**; Down recalls newer only on the **last visual line**. Consumes the event when it acts.
3. Otherwise `baseType` moves the caret between visual lines (unchanged).

## Visual-line gating
`caretOnFirstVisualLine` / `caretOnLastVisualLine` use the input width
(`contentWidth = Bounds.W-1`) and char-wrap math (the input's mode; WordWrap is off).
When the width is unset (Bounds.W < 2, e.g. before layout / under test) logical
lines are treated as unwrapped, so single-line prompts always recall.

## Navigation
- `historyPrev`: begin nav (stash draft, jump to newest) or step older; stop at oldest (no wrap). `SetText` puts the caret at end.
- `historyNext`: step newer; at the newest, restore the draft and leave nav mode; no-op (fall through) when not navigating.

## Tests (GLM partner writes these)
recall order newest→oldest, caret-at-end, draft restore on Down, edge-line gating,
completer-popup precedence, nudge/queue exclusion, reset on new submit.
