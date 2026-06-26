# Issue #487 — Persist failure context (errors, tool usage, token usage) on failed turns

## Summary

Today a session file is a faithful record only of **successful** turns. When a turn
fails, three things go wrong, all of which this change fixes:

1. **The file is never written at all** — `persistSession` sits *after* the early
   `return` on the error path in the synchronous turn entrypoint, so a failed turn
   leaves its partial transcript only in memory.
2. **The typed model error is flattened and dropped** — `sendCtx` collapses the
   classified `*ModelError` (`Type`/`HTTPStatusCode`/`RawResponse`) into a bare
   `&ModelError{Message: err.Error()}` stored in the in-memory `History`, which is
   never persisted.
3. **Per-round-trip token usage is never persisted** — `History[i].Usage` holds it
   but the store only ever writes `Kind:"message"` records and an aggregate index.

The fix is **additive and confined to gogent**: preserve the typed error in
`sendCtx`, persist on the error path, add two new JSONL record kinds (`usage` and
`turn_error`) carrying per-round-trip usage and the structured failure, and
reconstruct that state on restore. No on-disk *layout* change; old message-only
shards still load; turbotui is untouched; no new dependencies.

> **Scope honesty up front (see §1.1):** for the *real* connectors a failed
> round-trip returns a **nil** `resp`, so no prompt-token usage is available to
> record on the error path — the streaming path even assembles `resp.Usage` and
> then discards it at `connection.go:1018`, but `connection.go` is **out of scope**
> for this issue. We therefore persist usage on the **success** path (the criterion
> that is real in production) and keep a defensive, forward-compatible guard for the
> error path; the streaming-discard fix is documented as a separate follow-up.

---

## Exact files & functions touched

### 1. `internal/model/model_session.go` (the typed-error + timestamp + restore-seed seam)

- **`Turn` struct (L10–15) — add `Timestamp time.Time`.** This is the source for
  the per-record `At` field (§1.2 of the critique: `Turn` carried no time, so the
  previous design's `at` was unpopulated). `sendCtx` stamps it once per round-trip
  with `time.Now()` (success and error alike), so the timestamp reflects when the
  turn happened and **survives a compaction full-rewrite** (it lives in `History`,
  re-emitted verbatim) — unlike a write-time stamp, which would re-date old records
  on every rewrite. (`time` is added to the imports.)
- **`sendCtx` error branch (~L540–545).** Replace the flattening
  `s.History[len-1].Error = &ModelError{Message: err.Error()}` with the **preserved
  typed error** plus the timestamp:
  ```go
  var me *ModelError
  if !errors.As(err, &me) {
      me = &ModelError{Type: ErrorGeneric, Message: err.Error()}
  }
  last := len(s.History) - 1
  s.History[last].Error = me
  s.History[last].Timestamp = time.Now()
  // Defensive/forward-compatible only: every real connector returns resp==nil on
  // error (blocking complete() returns nil,&ModelError; the streaming path
  // assembles usage then discards it at connection.go:1018), so this branch does
  // not fire in production today. It is correct if a future connector ever returns
  // usage alongside an error. See §1.1.
  if resp != nil && resp.Usage != nil {
      s.History[last].Usage = resp.Usage
  }
  ```
  The connector returns a real `*ModelError` (`complete()` →
  `connection.go:1292/1334/1357`, `ctxError` → `1740`), wrapped once by `sendCtx`'s
  `fmt.Errorf("complete with tools: %w", err)`, so `errors.As` recovers
  `Type`/`HTTPStatusCode`/`RawResponse` intact. The only non-typed source is
  `configErr`, correctly caught by the `ErrorGeneric` fallback. `connection.go` is
  **not edited** (read-only), so whatever `Type` the classifier returns — including
  #485's new `ErrorEmptyResponse` — is preserved verbatim.
- **`sendCtx` success branch (~L562–564).** Also stamp
  `s.History[last].Timestamp = time.Now()` next to the existing
  `Response`/`Usage` assignment, so successful turns carry a timestamp too.
- **`HistoryLen() int` (new, cheap).** Mirrors `TranscriptLen()`; used by the
  persistence frontier without copying.
- **`GetHistoryFrom(off int) []Turn` (new).** Returns a copy of `History[off:]`
  only — the fix for critique §3.1. `History` is **never compacted** (it grows for
  the life of the session), so the previous design's `GetHistory()` full-copy in
  `encodeTurnMeta` was O(N) per save → O(N²) over a session, on the Pi5 target.
  `GetHistoryFrom` copies only the 0–few-element delta, matching the
  `encodeMessages`/`from` pattern. (`GetHistory()` stays for existing callers.)
- **`RestoreHistoryMeta(turns []Turn, seedTokenCount bool)` (new).** Restore-seed:
  replaces `s.History` with the reconstructed per-round-trip `Turn`s (only
  `Usage`/`Error`/`Timestamp` populated; `Request` left nil — never persisted).
  When `seedTokenCount` is true it also sets
  `s.CurrentTokenCount = lastUsageTotal(s.History)` (existing helper, L390). The
  caller passes `seedTokenCount=false` for multi-shard sessions — see §2.1 (the
  restore-drives-compaction guard). This is the meta-stream analogue of
  `ReplaceTranscript`; restoring `History` to the persisted length is **required**,
  not cosmetic, for the next delta to be correct (see "Restore alignment").

`Turn.Usage`/`Turn.Error` already exist; the only struct change is the added
`Timestamp`.

### 2. `internal/gogent/session_store.go` (the new record kinds + frontier)

- **`jsonlRecord` (L62–69).** Add three optional fields (`omitempty`, so a message
  record is byte-identical to today and old readers ignore the new ones):
  ```go
  Usage *model.TokenUsage `json:"usage,omitempty"`
  Err   *turnError        `json:"error,omitempty"`
  At    string            `json:"at,omitempty"` // RFC3339; from Turn.Timestamp
  ```
  New `turnError` payload (store-local): `Type string`, `Message string`,
  `HTTPStatusCode int`, `RawResponse string` (**truncated** to a cap, e.g. 8 KiB,
  to bound shard growth from a large error body). Kinds: `"usage"` and
  `"turn_error"`. `TokenUsage` already has clean JSON tags (its own doc says they
  exist "to keep it round-tripping through gogent's persistence"), so it serializes
  directly.
- **`persistState` (L193–198).** Add `metaPersisted map[string]int` (agentID →
  count of `History` turns already emitted as meta records) — the meta-stream twin
  of `persisted`. No epoch needed: `History` is append-only (compaction rewrites
  `Transcript`, never `History`), so the meta stream only grows.
- **`encodeTurnMeta(enc, agents, from func(aid string) int) (int, error)` (new).**
  For each agent, iterate `a.ThoughtTrain.GetHistoryFrom(from(aid))` (the bounded
  copy) and emit **one record per turn that has usage or an error**: a `turn_error`
  record when `Turn.Error != nil` (embedding `Usage` too on the off chance it is
  present), otherwise a `usage` record when `Turn.Usage != nil`; `At` =
  `Turn.Timestamp.Format(time.RFC3339)`. Marshal errors are `errors.Join`-aggregated
  exactly like `encodeMessages` (issue-#17 "never lose a line"). Called from **both**
  write paths into the **same buffer** as `encodeMessages`, so meta lines ride the
  existing shard-append/roll machinery unchanged:
  - `Save` delta path (L398–443): after `encodeMessages`,
    `encodeTurnMeta(..., func(aid) int { return st.metaPersisted[aid] })`.
  - `writeFullTranscript` (L467): after `encodeMessages`,
    `encodeTurnMeta(..., func(string) int { return 0 })` — first save and every
    compaction rewrite re-emit the full meta stream (idempotent; timestamps
    preserved because they live in `History`).
- **`recordFrontier` (L604) / `newPersistFrontier` (L620).** Also set
  `st.metaPersisted[aid] = a.ThoughtTrain.HistoryLen()`.
- **`shardRecord` (L1000) + `loadShard` (L961–997).** `shardRecord` gains `kind`
  plus optional `usage`/`turnErr`/`at`. `loadShard` today `continue`s on any
  non-`"message"` record; extend it to **also** collect `usage`/`turn_error`
  records, in file order. Unknown kinds still skipped → forward/backward compatible.
- **`loadTranscripts` (L939) — must be made kind-aware (critique §3.3).** Its
  current unconditional `out[agentID] = append(out[agentID], r.msg)` (L954) would
  push zero-value `Message`s into the transcript once `shardRecord` carries meta.
  Guard it: append **only** `kind=="message"` records to the transcript map; route
  meta records to a parallel `meta map[string][]model.Turn` (built from the
  `usage`/`turn_error` records, in order). Return both.
- **`Adopt` (L252).** Set `st.metaPersisted[aid] = len(loaded meta records)` per
  restored agent (twin of the existing `st.persisted` line) so the next save is a
  correct meta delta.
- **`LoadedSession` (L201).** Add `RootHistory []model.Turn` (root agent's
  reconstructed `Turn`s — Usage/Error/Timestamp only) and `ShardCount int` (from
  `len(idx.Shards)`, for the §2.1 guard). Only root is reconstructed, matching the
  existing transcript restore which seeds only `Transcripts["root"]`. Populated in
  `ListActive` and `LoadSession`.

**`shardMeta.Events` (critique §3.2 — corrected reasoning).** `Events` now counts
message **and** meta records. This is correct for shard rolling: `writeLinesToShards`
rolls on `Events >= shardMaxEvents` **or** `Bytes+rec > shardMaxBytes` (L519) — the
two caps are **independent**, so counting meta lines in `Events` does *not* desync
the byte cap (the previous design's justification was wrong on this point). The only
visible effect is that the Sessions browser's `Messages` figure (sum of shard
`Events`) now includes per-turn meta lines — a small, monotonic cosmetic shift on
records written *after* this change. We accept it (the figure is more precisely a
record count); keeping `Events` as the total is what the roll logic needs. Splitting
out a pure message count would mean an additive index field, which is unnecessary
for this issue.

### 3. `internal/gogent/gogent.go` (persist on the error path + restore)

- **Call site (L2588–2604), `SendMessageToSessionWithModelAndEffort`.** Move the
  persist out of the success-only tail so it runs on **both** outcomes:
  ```go
  ag.SetState(agent.StateIdle)
  // Persist the turn — success OR failure — so the partial transcript and the
  // usage/turn_error records are durably captured (issue #487). Previously the
  // early error return below skipped this entirely.
  g.persistSession(sessionID)
  if err != nil {
      g.NotifyHooks(HookEvent{Type: HookError, SessionID: sessionID, AgentID: agentID,
          Error: lastModelError(ag)}) // typed error from History; falls back to wrapped string
      return nil, fmt.Errorf("process message: %w", err)
  }
  ```
  This is the single synchronous entrypoint every caller funnels through —
  `DispatchMessage` (`dispatch.go:81`, the **#481** daemon goroutine under
  `context.Background()`), `ExecuteApprovedPlan`, and the watcher path
  (`watcher.go:714`) — so persisting at its end captures the turn for **all** paths
  exactly when the turn goroutine completes, as #481 requires. (`lastModelError(ag)`
  reads `ag.ThoughtTrain`'s last `Turn.Error` so the live `HookError` event also
  carries the typed classification; the persisted record is correct regardless.)
- **`adoptLoaded` (L1807–1853).** After `sess.ReplaceTranscript(msgs)`, also
  `sess.RestoreHistoryMeta(ls.RootHistory, ls.ShardCount <= 1)` so a restored
  session reconstructs the failure indicator (last `Turn.Error`) and — for the
  common **single-shard** case — token accounting (`CurrentTokenCount`). See §2.1
  for why multi-shard passes `false`.

### 4. Read-only context (NOT edited)

- `internal/model/connection.go` — `ModelError` (L593), `ModelErrorType` (L580),
  `analyzeError` (L1593), `TokenUsage` (L441), and the **streaming usage discard**
  at L1018. Out of scope for this issue; `Type` persisted verbatim
  (#485-compatible). See "Known limitations / follow-ups."
- `internal/agent/user_session.go` — emission sites stay live-only. **No edit
  needed**: the typed error reaches persistence through `ModelSession.History`, not
  the `SessionEvent` stream or the returned error, so `runLoop`/`modelRoundTrip` are
  untouched.

---

## §1.1 — Error-path token usage: honest scope

The issue asks to "capture prompt-token usage on the error path **where the provider
returns it**." In the current code **no real connector returns it on error**:

- Blocking `complete()` returns `nil, &ModelError{…}` on every error branch
  (`connection.go:1292/1312/1334/1344/1357/1372`).
- The streaming path *assembles* `resp.Usage` from the terminal `ev.Done` event
  (`connection.go:1013`) and then **throws it away**:
  `if err := <-errCh; err != nil { return nil, err }` (`1018`).

So the `resp != nil && resp.Usage != nil` guard in `sendCtx` is **inert in
production today**. We keep it because (a) it is zero-cost and forward-compatible —
correct the moment a connector returns usage-with-error — and (b) it makes the
intent explicit. We do **not** claim production failed turns record cost. The clean
way to make the criterion true in production is a one-line change at
`connection.go:1018` (`return resp, err` — returning the already-assembled response
alongside the error; it touches neither the classifier nor the `Type` set, so it is
#485-safe), but `connection.go` is explicitly out of scope here, so it is filed as a
follow-up rather than done in this issue. Tests reflect this split (success-path
usage is the production assertion; the error-path guard is exercised only by a fake
connector and labelled as forward-compatible — see Tests).

---

## §2.1 — Restore must not spuriously trigger compaction

`compactIfNeeded` (`user_session.go:1721`) runs `NeedsCompression()`
(`model_session.go:272`) at the **top of every turn** (`1315`/`1352`), testing
`CurrentTokenCount >= 80% of the window`. Today a restored session has
`CurrentTokenCount == 0`, so it never compacts until real usage accumulates.
Restoring the count changes that, so we bound it:

- **Single-shard session (the common case):** the restored transcript is the
  *complete* transcript, so `CurrentTokenCount` and the transcript agree. Seeding
  the count is correct, and if the session is genuinely near capacity, compacting on
  the first new message is the **right** behavior (it pre-empts an overflow). We
  seed it (`seedTokenCount=true`).
- **Multi-shard session:** restore is current-shard-only (`session_store.go:944`),
  so the live transcript is a *partial* recent slice while `lastUsageTotal` reflects
  the *full* pre-rollover context. Seeding the full count against a short transcript
  could fire a spurious summarization on the first message (which would then
  summarize only the recent slice and self-correct the count downward). To avoid
  this mismatch we pass `seedTokenCount=false`, preserving today's behavior (count
  starts at 0, re-measured by the first real round-trip). The failure indicator and
  `History` are still reconstructed; only the count seed is withheld.

`ShardCount` on `LoadedSession` (from the index's shard table) drives the choice in
`adoptLoaded`. This is the only behavioral interaction the count-restore has with
the rest of the system, and it is now explicitly handled.

---

## Restore alignment (why `RestoreHistoryMeta` is mandatory, not cosmetic)

The meta frontier (`metaPersisted`) is a `History`-index boundary, exactly as the
message frontier (`persisted`) is a transcript-index boundary. The message frontier
survives restore only because `adoptLoaded` re-seeds the transcript
(`ReplaceTranscript`). The meta stream needs the same: if `History` were left empty
while `metaPersisted` was set to N (from the active shard), the next live turn would
land at `History[0]` while the delta read `GetHistoryFrom(N)` on a length-1 slice —
empty — and its usage/error would **never** persist. Reconstructing `History` to
length N keeps the next delta correct *and* doubles as the indicator/token-count
restore the acceptance criteria require. Restore reads only the **active shard**
(issue #26), so the reconstructed transcript and meta stream are both
active-shard-scoped and mutually consistent.

---

## On-disk shape (illustrative active shard after one failed turn)

```jsonl
{"kind":"message","agent_id":"root","message":{"role":"user","content":"…"}}
{"kind":"message","agent_id":"root","message":{"role":"assistant","tool_calls":[…]}}
{"kind":"message","agent_id":"root","message":{"role":"tool","content":"…"}}
{"kind":"usage","agent_id":"root","at":"2026-06-26T12:00:01Z","usage":{"prompt_tokens":1234,"completion_tokens":56,"total_tokens":1290}}
{"kind":"message","agent_id":"root","message":{"role":"user","content":"…(2nd step)…"}}
{"kind":"turn_error","agent_id":"root","at":"2026-06-26T12:00:03Z","error":{"type":"context_overflow","http_status_code":400,"message":"…","raw_response":"…(≤8KiB)…"}}
```

The partial transcript (user + tool-call + tool-result + 2nd user message) is
present, the first round-trip's usage is present, and the failing round-trip's
classified error is present. **Note:** the `turn_error` record above carries **no**
`usage` block — that matches production, where the failed round-trip's `resp` is nil
(§1.1). A `usage` block inside a `turn_error` record only appears if a future
connector returns usage-with-error.

---

## The four design gates

### (1) Goal match — fix, not feature creep — **addressed**
Closes the three named gaps: persist-on-error (call-site move), preserve the typed
`*ModelError` (`sendCtx`), persist per-round-trip usage + structured failure (new
additive kinds), restore both (adopt seed). Tool args/results are already transcript
messages — they persist automatically once the error-path persist runs. Error-path
usage is now scoped honestly (§1.1): we persist success-path usage (real in
production) and keep a forward-compatible guard for the error path, with the
streaming-discard fix documented as a follow-up rather than overclaimed.
Out-of-scope items (auto-compaction, refusal handling, index layout, live event
stream) are untouched.

### (2) Usability — the right thing is surfaced — **addressed**
A previously-failed session reopens with its error and (single-shard) token figures
reconstructed instead of a bare idle prompt. A worker can debug from the `.jsonl`
alone — error class, HTTP status, message, the tool calls/results that led there,
and the timestamped per-round-trip usage. The one behavioral interaction of
count-restore (compaction) is now bounded (§2.1): legitimate for single-shard
near-capacity sessions, suppressed for the multi-shard partial-transcript case.
Drawing a TUI marker from the restored state is the optional follow-up the issue
scopes out; the state needed to draw it is present.

### (3) No regressions — **addressed**
- **Happy path unchanged on disk:** message records byte-identical (new fields
  `omitempty`); `.index` layout unchanged. A successful turn additionally writes one
  tiny `usage` line per round-trip.
- **Backward compatible both directions:** old message-only shards load (zero meta →
  `RestoreHistoryMeta(nil,*)` no-op); a new shard read by an old binary still works
  (skips non-`"message"` kinds).
- **Hot-path efficiency (§3.1):** `encodeTurnMeta` uses `GetHistoryFrom(off)` so each
  save copies only the meta delta, not the whole never-compacted `History` — no
  O(N²) on Pi5.
- **`loadTranscripts` kind-aware (§3.3):** only message records enter the transcript
  map; meta records route to the meta map — no zero-value `Message` injection.
- **`Events` count (§3.2):** corrected reasoning — byte/event caps are independent,
  so the `Messages`-figure shift is purely cosmetic, not a roll desync.
- **Compaction on restore (§2.1):** analyzed and guarded.
- **Invariants preserved:** delta/full-rewrite/epoch logic, shard rolling, and the
  issue-#17 `errors.Join` aggregation all extend to meta records unchanged. No new
  lock ordering: `encodeTurnMeta`'s `GetHistoryFrom` takes `s.mu` under the store
  lock, the same pattern as the existing `GetTranscript()` in `encodeMessages`.
- **Tests:** `go build/vet/gofmt/golangci-lint` clean; `go test ./...` (no `-race`,
  Pi5) green except the pre-existing environmental `TestUserSessionSendMessage` 404.
  Any existing test asserting `CurrentTokenCount == 0` after restore now sees a real
  count for single-shard sessions (intended improvement; update if present).

### (4) Holistic across both repos — **OK**
Confined to gogent's persistence + model-session seam. **turbotui owns no session
persistence** (verified: `github.com/hobbestherat/turbotui` is a pure rendering
library — cells/lines/screen/clipboard; no `SessionStore`/`jsonl`/`persistSession`),
so no `go.mod` bump and no cross-repo coupling. The wire/persistence seam is
respected: usage/error are persisted as **separate store-layer records**, never
bolted onto `model.Message` (whose custom `MarshalJSON` is *also* the provider wire
shape, with `Reasoning` stripped on send), so nothing new leaks into an outbound
request. `Type` persisted verbatim → #485-compatible. Persist-on-error works with
#481's dispatch.

---

## Known limitations / follow-ups (out of scope here)

1. **Streaming usage on error is discarded at `connection.go:1018`.** The streaming
   connector assembles `resp.Usage` then drops it when returning the error. A
   one-line `return resp, err` would let `sendCtx`'s guard record prompt-token cost
   on streamed failures in production. `connection.go` is read-only for this issue →
   filed as a separate follow-up.
2. **Optional gap #6 (BUDGET_EXCEEDED fold).** Folding the budget-exceeded notice
   onto the transcript lives in the agent loop (`internal/agent`,
   `budgetExceededMarker`, the success/`break` path at `user_session.go:1419`), which
   the task asks to keep minimal. Recommend deferring — it is explicitly optional and
   token-budget terminations that surface as model errors are already captured.
3. **TUI rendering of the restored error indicator** is the optional follow-up the
   issue scopes out; the reconstructed state makes it possible.

---

## Regression risks (called out)

- **Browser `Messages` count** now includes meta lines (cosmetic; new sessions
  only; not a roll desync — §3.2).
- **Shard size** grows ~1 small line per round-trip plus, on failures, a truncated
  raw-response blob (≤8 KiB). Bounded, far below the 10 MiB roll cap.
- **Restored token count** is recovered only for single-shard sessions (§2.1);
  multi-shard and pre-change sessions keep today's 0-until-first-turn behavior.
- **`RawResponse` may contain sensitive provider output.** Truncation bounds size;
  full redaction is out of scope but the cap is the hook for a later policy.

---

## Tests to add

1. **Failed-turn persistence.** Fake connector returning a classified `*ModelError`
   (`Type`/`HTTPStatusCode`/`RawResponse`): assert the session file **is written**
   and contains a `turn_error` record with all three fields + message + non-empty
   `at`, and that the partial transcript (user message + earlier tool results) is
   persisted.
2. **Usage persistence (split for honesty, §1.1):**
   - (a) *Production behavior:* a successful round-trip persists a `usage` record
     (prompt/completion/cached/reasoning) with a timestamp.
   - (b) *Forward-compatible guard:* a fake connector returning
     `(resp-with-usage, err)` records prompt-token usage on the error path —
     labelled in the test as exercising the guard, **not** current real-provider
     behavior (real connectors return nil `resp` on error).
3. **Restore — failure.** A session whose last turn failed reloads with `History`
   reconstructed: last `Turn.Error` set; for a single-shard fixture
   `CurrentTokenCount` non-zero; for a multi-shard fixture `CurrentTokenCount`
   stays 0 (the §2.1 guard) — not a bare idle session either way.
4. **Backward-compat.** A hand-written shard containing only `Kind:"message"`
   records loads cleanly (no meta → no-op seed; no zero-value messages injected).
5. **Happy-path no-regression.** A successful turn persists + restores its
   transcript exactly as before (plus the now-recovered single-shard token count).

---

## Open questions

1. **One kind or two?** Plan uses two human-readable kinds (`usage`, `turn_error`)
   for a legible debug artifact, ≤1 per round-trip. A single `turn` kind carrying
   both optional fields is simpler but less self-describing. Proceeding with two
   unless you prefer one.
2. **`RawResponse` truncation cap** — proposed 8 KiB. Confirm size and whether any
   redaction is wanted now or deferred.
3. **Streaming-discard follow-up (limitation #1).** Confirm it is acceptable to
   leave error-path usage inert in production for this issue (the guard is in place;
   only the one-line `connection.go:1018` change is deferred), or whether you want
   that one line pulled into scope despite the "do not edit connection.go" constraint.
4. **`HookError` typed error.** Surfacing the typed `*ModelError` via
   `lastModelError(ag)` improves live hooks but is not required by acceptance —
   include or drop?
