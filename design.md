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
- Over-limit restore (>1000): `trim()` keeps `keep = limit - limit/10` (= 900) newest and renders
  once; no panic, correct tail. **This is *not* byte-identical to today's incremental restore.**
  Today restore goes through per-record `add()`, so the slice hovers in `[keep, limit]` and a 1500-
  message restore ends at ≈995 retained; the single end-of-batch `trim()` lands at exactly `keep`
  (900). The newest 900 are always retained either way (correct tail, no data loss — the durable
  transcript lives in the session JSONL), and the brief sanctions asserting `keep`, not a literal
  1000. Reproducing the incremental hover would require M trims, which defeats the batching, so this
  small retained-count shift is the deliberate, accepted trade — called out here and in the test
  (§Tests) so it is not mistaken for an accidental regression.

**Nil contract (mandatory, single rule): `m.records` must never hold `nil`.** The blank-text builders
below return `nil` for blank input (matching today's skip guards), so every append site — the three
`add*` wrappers **and** `restore` — must drop `nil`. To make this fail-safe rather than convention,
`addAll` itself also skips `nil` entries while appending (a cheap guard that removes the latent
nil-deref class entirely):

```go
func (m *transcriptModel) addAll(records []*transcriptRecord) {
    for _, r := range records {
        if r != nil { // builders return nil for blank text; never let nil reach render()
            m.records = append(m.records, r)
        }
    }
    if m.limit > 0 && len(m.records) > m.limit {
        m.trim()
        return
    }
    m.render()
}
```

### 2. Extract record builders so `restore()` can build a slice (`session_window.go`)
`restore()` currently calls `addUser/addThought/addAssistant`, which each embed `transcript.add(...)`
/ `addAndReveal(...)`. To batch, `restore()` must produce `[]*transcriptRecord` and hand it to
`addAll`. Extract the record literals into pure builders returning `*transcriptRecord` (returning
`nil` for blank text, matching today's skip guards), and have the live `addUser/addThought/
addAssistant` call them:

All three builders follow **one** rule: return `nil` for blank (`strings.TrimSpace == ""`) text,
non-nil otherwise. All three wrappers therefore guard identically before adding:

```go
// nil for blank text; otherwise kindUser, "You:", roleUser
func userRecord(text string) *transcriptRecord { /* if blank → nil */ }
// nil for blank text; otherwise kindThinking, "thought", collapsed
func thoughtRecord(text string) *transcriptRecord { /* if blank → nil */ }
// nil for blank text; otherwise kindAssistant, "Gogent:", rich:true
func assistantRecord(text string) *transcriptRecord { /* if blank → nil */ }

func (sw *SessionWindow) addUser(text string)      { if r := userRecord(text); r != nil { sw.transcript.add(r) } }
func (sw *SessionWindow) addThought(text string)   { if r := thoughtRecord(text); r != nil { sw.transcript.add(r) } }
func (sw *SessionWindow) addAssistant(text string) { if r := assistantRecord(text); r != nil { sw.transcript.addAndReveal(r) } }
```

Why `userRecord` gains a nil-for-blank guard it didn't have before: today `addUser` builds a non-nil
literal inline (`session_window.go:1789`), and restore guards blank user content explicitly
(`if strings.TrimSpace(m.Content) != ""`, `session_window.go:2721`). The new uniform contract folds
that restore-side guard *into* `userRecord`, so a whitespace-only saved user turn still produces **no**
`You:` entry on restore (preserving today's behaviour) without a special-case in `restore`. No live
caller passes blank today (submit trims+early-returns, `session_window.go:401`; `sendCommandNow` is
guarded, `:1966`), so guarding `addUser` is behaviour-preserving for the live path. The three
wrappers are now **symmetric** — no caller can append a `nil`, and `addAll`'s own nil-skip (above) is
the belt-and-suspenders backstop.

This keeps the **live** paths behaviour-identical (same records, same `add`/`addAndReveal`; the only
delta is the no-op blank guard on `addUser`, never hit in practice), and gives `restore()` a single
source of truth for record shape — no literal drift between live and restored records.

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
            // userRecord is nil for blank content — folds the old explicit
            // `if TrimSpace(m.Content) != ""` guard (session_window.go:2721) into the builder.
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
  (900), newest-tail retained, no panic, single render. (Asserts the batch trim-to-`keep` semantics,
  **not** a literal 1000 and **not** the incremental path's `keep..limit` hover — see the over-limit
  note in §1; this codifies the deliberate retained-count shift.)
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

**(2) Usability.** The restored transcript is visually identical to today for the common (≤limit)
case: same records, order, fold state, Markdown, and final scroll-to-bottom. The one intentional
difference is the >limit case (retains `keep`/900 rather than the incremental ~995 — see §1), which
only affects how much *trimmed-away history* is held live, not the newest content the user reads. The
user-visible change is otherwise purely that the UI thread is no longer busy per-record while a large
session restores — it appears in one paint instead of visibly building up. An empty restore now does
one no-op `render()` (Clear+ScrollToBottom over zero records) where today the loop body never ran;
end-state (empty view) is unchanged. Nothing is silenced; the same content surfaces. This is a
non-interactive render path, so there is no dialog/input to drive.

**(3) No regressions.** Live `add`/`addAndReveal` paths are behaviour-unchanged (builders return the
same records; the only delta is a no-op blank guard added to `addUser`, never hit in practice).
`addAll` reuses the existing `trim()`/`render()`/`visible()` machinery, so limit/trim/filter
invariants are preserved by construction. `reload` still ends in exactly one render. First-connect
`tui.go` callers (fork/resume) keep working because `restore()` now renders itself. Risks, each
resolved explicitly:
- **(a) Nil records → render-time nil-deref.** `m.records` must never hold `nil`. Resolved with a
  single uniform contract: the three blank-text builders return `nil`, **all three** `add*` wrappers
  guard with `if r != nil`, `restore` appends only non-nil, and `addAll` itself skips `nil` as a
  backstop (§2/§Helper). No append site can introduce a `nil`. The #402 reasoning-only test guards
  the assistant-empty/thought path; a new whitespace-only-user restore test guards the folded
  `userRecord` guard.
- **(b) >limit retained count shifts (~995 → 900).** Real but deliberate and bounded (see §1):
  newest 900 always retained, no data loss, brief-sanctioned. Documented in the over-limit note and
  asserted as `keep` (not 1000) in the test, so it cannot be mistaken for an accidental regression.

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
- **>1000 expected count.** Confirming trim-to-`keep` (900 = `limit - limit/10`) is the desired
  ">1000 case" outcome. Note it is **not** identical to today's incremental restore (which hovers at
  ~995); it is the single-trim result, which the brief sanctions. Flag if a reviewer instead wants
  the retained count held at the incremental `keep..limit` band (would require M trims, defeating the
  batch).
