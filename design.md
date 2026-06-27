# Design — Batched first-connect transcript restore (gogent issue #519)

## Issue
`First-connect transcript restore renders one message at a time instead of batched.`
Reopening a session replays its saved transcript via `SessionWindow.restore()`, which calls
`transcript.add()` once **per message**. When not filtering, `add()` calls `renderOne()` per
record — an incremental UI-thread append, plus a per-assistant-answer Markdown parse on first
render. For an M-message session that is M synchronous appends on the UI thread. Reconnect's
`reload()` already batches into a single `render()`; first-connect `restore()` does not.

This is a **rendering-only fix**: same final transcript, computed with one compose instead of M.
No feature, no behaviour change, pure `ui/tui`.

## Evidence (current code)
- `ui/tui/session_window.go:2717` `restore(msgs)` — per-message `switch` → `addUser` /
  `addThought` / `addAssistant` / `transcript.add(...)`; each routes through `transcriptModel.add`.
- `ui/tui/transcript_model.go:260` `add(r)` — appends to `m.records`; over limit → `trim()` (one
  render); else `m.filtering()` ? `render()` : `renderOne(r)` (per-record incremental append).
- `ui/tui/transcript_model.go:308` `renderOne(r)` — `AddColored`/`AddStyled` per record;
  rich assistant records trigger `markdownSpans()` parse.
- `ui/tui/transcript_model.go:350` `render()` — `Clear()`, reset entries, re-`renderOne` every
  **visible** record, `ScrollToBottom()`. Honours filter via `visible()`.
- `ui/tui/transcript_model.go:285` `trim()` — keeps `limit - limit/10` (= 900 for the default
  1000) newest records, then `render()` once.
- `ui/tui/session_window.go:2710` `reload(msgs)` — `records=nil; render(); restore(msgs);
  render()`. Today this double-renders (clear-render, then restore's per-record appends, then a
  final full render).
- First-connect callers that rely on `restore()` itself to paint (no surrounding render):
  `ui/tui/tui.go:1561` (fork pre-fill), `:1605` (resume), `:1654`. So the helper must end in a
  render itself.

## Change — Direction B (shared `addAll` helper)

### 1. New `transcriptModel.addAll` (`transcript_model.go`)
Append all records, then render exactly once (mirroring `add`'s limit/trim contract):

```go
// addAll appends every record in one batch and rebuilds the view once, instead of
// the per-record incremental append add() does. Restoring a saved transcript is M
// appends of already-built records with no streaming in between, so a single
// render() is the same final view at O(1) composes rather than O(M) (issue #519).
// It honours the same limit/trim contract as add(): over the cap, the oldest batch
// is dropped and the single render() shows the retained tail.
func (m *transcriptModel) addAll(records []*transcriptRecord) {
    m.records = append(m.records, records...)
    if m.limit > 0 && len(m.records) > m.limit {
        m.trim() // drops oldest to keep, renders once
        return
    }
    m.render() // single compose; honours active filter/search via visible()
}
```

Properties:
- One `render()` (or one `trim()`→`render()`) regardless of M.
- `render()` already routes through `visible()`, so **filtering active during restore** yields the
  correct filtered view + match count in one pass — strictly cleaner than `add`'s per-record branch.
- Markdown cache (`markdownSpans`) is still populated lazily, now in the single render pass.
- Over-limit restore (>1000): `trim()` keeps 900 newest and renders once — identical to the live
  streaming over-cap path; no new trimming semantics, no panic, correct tail.

### 2. Extract record builders so `restore()` can build a slice (`session_window.go`)
`restore()` currently calls `addUser/addThought/addAssistant`, which each embed `transcript.add(...)`
/ `addAndReveal(...)`. To batch, `restore()` must produce `[]*transcriptRecord` and hand it to
`addAll`. Extract the record literals into pure builders returning `*transcriptRecord` (returning
`nil` for blank text, matching today's skip guards), and have the live `addUser/addThought/
addAssistant` call them:

```go
func userRecord(text string) *transcriptRecord { /* kindUser, "You:", roleUser */ }
func thoughtRecord(text string) *transcriptRecord { /* nil if blank; kindThinking, collapsed */ }
func assistantRecord(text string) *transcriptRecord { /* nil if blank; kindAssistant, rich:true */ }

func (sw *SessionWindow) addUser(text string)      { sw.transcript.add(userRecord(text)) }
func (sw *SessionWindow) addThought(text string)   { if r := thoughtRecord(text); r != nil { sw.transcript.add(r) } }
func (sw *SessionWindow) addAssistant(text string) { if r := assistantRecord(text); r != nil { sw.transcript.addAndReveal(r) } }
```

This keeps the **live** paths byte-identical in behaviour (same records, same
`add`/`addAndReveal`), and gives `restore()` a single source of truth for record shape — no literal
drift between live and restored records.

> Note: `addAssistant`'s live path keeps `addAndReveal` (scroll-to-bottom then add) so a streamed
> answer reveals itself. Restore does **not** need per-record reveal: `addAll`→`render()` ends with
> `ScrollToBottom()`, pinning the view to the newest record — the same end state restore reaches
> today.

### 3. Refactor `restore()` to build records and call `addAll` once
Replace the per-message `add` calls with appends to a local slice, preserving order and the existing
tool-call / tool-result / system inline records, then one `addAll`:

```go
func (sw *SessionWindow) restore(msgs []ChatMessage) {
    records := make([]*transcriptRecord, 0, len(msgs))
    for _, m := range msgs {
        switch strings.ToLower(m.Role) {
        case "user":
            if r := userRecord(m.Content); r != nil { records = append(records, r) }
        case "assistant":
            if r := thoughtRecord(m.Reasoning); r != nil { records = append(records, r) } // issue #402
            if r := assistantRecord(m.Content); r != nil { records = append(records, r) }
            if m.Tool != "" { records = append(records, toolCallRecord(m.Tool, m.Args)) }
        case "tool":
            records = append(records, toolResultRecord(m.Tool, m.Content))
        default:
            if strings.TrimSpace(m.Content) != "" { records = append(records, systemRecord(m.Content)) }
        }
    }
    sw.transcript.addAll(records)
}
```

(The tool-call / tool-result / system record literals already live inline in `restore` today; they
move verbatim into small builders or stay inline within the loop — either way the constructed record
is identical.)

Net: **exactly one** `render()` per `restore()`, regardless of M.

### 4. Refactor `reload()` to use the same helper, ending in one render
`restore()` now renders itself, so the intermediate clear-render is redundant. `reload` becomes:

```go
func (sw *SessionWindow) reload(msgs []ChatMessage) {
    sw.transcript.records = nil // discard the frozen last-known transcript
    sw.restore(msgs)            // builds records, addAll → exactly one render()
}
```

Setting `records = nil` first means `addAll` appends onto a fresh slice; the single render rebuilds
the whole view. **Exactly one render, no double.** (Old `reload` did clear-render + per-record + full
render; new does one.)

## Files / functions touched
- `ui/tui/transcript_model.go` — **new** `addAll(records []*transcriptRecord)`.
- `ui/tui/session_window.go` — **new** builders `userRecord`/`thoughtRecord`/`assistantRecord`
  (and `toolCallRecord`/`toolResultRecord`/`systemRecord` or inline equivalents); rewrite
  `addUser`/`addThought`/`addAssistant` to delegate; rewrite `restore()` to build a slice + `addAll`;
  simplify `reload()` to `records=nil; restore()`.
- **No** turbotui change. **No** new deps, **no** `go.mod` bump. `ui/tui` keeps its current import
  set (no `internal/daemon`/`server`).

## Tests (new, mirroring existing transcript_model/session_window style)
- **Single render on restore**: add a render/compose counter spy on `transcriptModel` (e.g. a
  `renderCount` field bumped in `render()`, or count `renderOne` calls) — restoring M (e.g. 50)
  mixed messages bumps `render()` once and `renderOne`-per-record-during-add zero times. Assert the
  final `view.AllText()` equals the per-record result for the same input.
- **restore == reload record set/order**: same `[]ChatMessage` through `restore` (fresh window) and
  `reload` (window with prior records) yields identical `records` kinds/headers/bodies in order.
- **>1000-record restore trims once**: restore 1500 records → `len(records) == limit - limit/10`
  (900), newest-tail retained, no panic, single render. (Asserts the existing trim semantics, not a
  new "exactly 1000".)
- **Mixed types order preserved**: user/assistant/tool-call/tool-result/system land in input order
  (extends existing `TestRestoreIndexesTranscript`).
- **Filtering active during restore**: set a query before restore → filtered view + correct
  `matchCount()` after one render.
- Existing `TestRestoreIndexesTranscript`, `TestRestoreReasoningOnlyAssistantRendersThoughtIssue402`,
  and the issue #204 theme test must stay green unchanged (record order/body/AllText identical).

## Design criteria

**(1) Goal match.** Exactly the ask: restore appends all records then a single `render()`
(O(1) compose), not one `renderOne()` per message; `reload()` shares the same helper and still ends
in one render. No feature, no UI change — rendering-only. No scope creep (no new limit, no streaming
rework).

**(2) Usability.** The restored transcript is byte-identical to today: same records, order, fold
state, Markdown, and final scroll-to-bottom. The user-visible change is purely that the UI thread is
no longer busy per-record while a large session restores — it appears in one paint instead of
visibly building up. Nothing is silenced; the same content surfaces. This is a non-interactive
render path, so there is no dialog/input to drive.

**(3) No regressions.** Live `add`/`addAndReveal` paths are untouched in behaviour (builders return
the same records). `addAll` reuses the existing `trim()`/`render()`/`visible()` machinery, so
limit/trim/filter invariants are preserved by construction. `reload` still ends in exactly one
render. First-connect `tui.go` callers (fork/resume) keep working because `restore()` now renders
itself. Risk: (a) blank-text skip guards must be preserved in the builders (assistant/thought/user
empty → no record) — covered by returning `nil`; the #402 reasoning-only test guards this. (b) The
>1000 test must assert 900, not 1000 — documented above so it isn't mistaken for a regression.
gofmt/build/vet/lint/test gates apply; pre-existing `TestUserSessionSendMessage` 404 is the only
accepted failure.

**(4) Holistic / cross-repo.** The fix lives entirely in gogent's `ui/tui` view layer
(`github.com/hobbestherat/gogent`), the correct place: this is how gogent composes records into its
own `transcriptModel`/`TextView`. turbotui (`github.com/hobbestherat/turbotui`) supplies the
`TextView` primitive (`AddColored`/`AddStyled`/`Clear`/`ScrollToBottom`); we call its existing API
with the same operations, just batched behind one `render()` — no new turbotui surface, the
gogent↔turbotui seam is unchanged. No `go.mod` bump, no new dependency. Downstream: orchestration
note flags collision with #509 and #520 (both `ui/tui`/`session_window.go`); file-disjoint from
#518 — at the gate, rebase onto current `origin/main` and resolve any `session_window.go`/
`transcript_model.go` overlap before merging.

## Open questions
- **Builder granularity for tool/result/system records.** They're already inline literals in
  `restore` today. I lean toward keeping them inline within the loop (smallest diff) rather than
  extracting `toolCallRecord`/etc., and only extract `userRecord`/`thoughtRecord`/`assistantRecord`
  (which the live `add*` methods also need). Either is acceptable; flag if a reviewer prefers full
  extraction for symmetry.
- **Render-spy mechanism for the single-render test.** Preference between a `renderCount` field on
  `transcriptModel` (tiny production field, test-only reads) vs. asserting `renderOne` is not called
  per-record via observable view state. I lean to a `renderCount` counter as the most direct,
  style-consistent assertion; confirm that's acceptable to add to the production struct.
- **>1000 expected count.** Confirming the trim-to-900 (`limit - limit/10`) semantics is the desired
  ">1000 case" behaviour to preserve (it matches the live over-cap path), rather than a literal
  trim-to-1000.
