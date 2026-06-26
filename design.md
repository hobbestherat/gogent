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

---

## Exact files & functions touched

### 1. `internal/model/model_session.go` (the typed-error + restore-seed seam)

- **`sendCtx` error branch (~L540–545).** Replace the flattening
  `s.History[len-1].Error = &ModelError{Message: err.Error()}` with the **preserved
  typed error**:
  ```go
  var me *ModelError
  if !errors.As(err, &me) {
      me = &ModelError{Type: ErrorGeneric, Message: err.Error()}
  }
  s.History[len(s.History)-1].Error = me
  if resp != nil && resp.Usage != nil { // prompt tokens where the provider returns them
      s.History[len(s.History)-1].Usage = resp.Usage
  }
  ```
  The connector returns a real `*ModelError` (`complete()` → `analyzeError()` at
  connection.go:1593, or literal `&ModelError{…}`), wrapped once by `sendCtx`'s
  `fmt.Errorf("complete with tools: %w", err)`, so `errors.As` recovers
  `Type`/`HTTPStatusCode`/`RawResponse` intact. `connection.go` is **not edited**
  (read-only), so whatever `Type` the classifier returns — including #485's new
  `ErrorEmptyResponse` — is preserved verbatim. (`errors` is added to imports.)
- **`HistoryLen() int` (new, cheap).** Mirrors `TranscriptLen()`; used by the
  persistence frontier to avoid copying `History`.
- **`RestoreHistoryMeta(turns []Turn)` (new).** Restore-seed: replaces `s.History`
  with the reconstructed per-round-trip `Turn`s (only `Usage`/`Error` populated;
  `Request` left nil — it is never persisted) and sets
  `s.CurrentTokenCount = lastUsageTotal(s.History)` (the existing helper at L390).
  This is the analogue of `ReplaceTranscript` for the meta stream — see
  "Restore alignment" below for why it is *required*, not cosmetic.

`Turn` (L10–15) already has `Usage *TokenUsage` and `Error *ModelError`; no struct
change. `GetHistory()` (L218) is the existing read side for encoding.

### 2. `internal/gogent/session_store.go` (the new record kinds + frontier)

- **`jsonlRecord` (L62–69).** Add three optional fields (all `omitempty`, so a
  message record is byte-identical to today and old readers ignore the new ones):
  ```go
  Usage *model.TokenUsage `json:"usage,omitempty"`
  Err   *turnError        `json:"error,omitempty"`
  At    string            `json:"at,omitempty"` // RFC3339 timestamp of the record
  ```
  New `turnError` payload struct (store-local): `Type string`, `Message string`,
  `HTTPStatusCode int`, `RawResponse string` (**truncated** to a cap, e.g. 8 KiB,
  to bound shard growth from a large error body). Kinds: `"usage"` and
  `"turn_error"`.
- **`persistState` (L193–198).** Add `metaPersisted map[string]int` (agentID →
  count of `History` turns already emitted as meta records). It is the meta-stream
  twin of `persisted` (the message frontier). No epoch needed — `History` is
  append-only and never compacted, so the meta stream only ever grows.
- **`encodeTurnMeta(enc, agents, from func(aid string) int) (int, error)` (new).**
  For each agent, iterate `a.ThoughtTrain.GetHistory()[from(aid):]` and emit **one
  record per turn that has usage or an error**: a `turn_error` record when
  `Turn.Error != nil` (embedding `Usage` too when present), otherwise a `usage`
  record when `Turn.Usage != nil`. Errors are `errors.Join`-aggregated exactly like
  `encodeMessages` (issue #17 invariant: never report success while a line went
  missing). Called from **both** write paths into the **same buffer** as
  `encodeMessages`, so meta lines ride the existing shard-append/roll machinery
  unchanged:
  - `Save` delta path (L398–443): after `encodeMessages`, also
    `encodeTurnMeta(..., func(aid) int { return st.metaPersisted[aid] })`.
  - `writeFullTranscript` (L467): after `encodeMessages`, also
    `encodeTurnMeta(..., func(string) int { return 0 })` — first save and every
    compaction rewrite re-emit the full meta stream (idempotent regeneration).
- **`recordFrontier` (L604) / `newPersistFrontier` (L620).** Also set
  `st.metaPersisted[aid] = a.ThoughtTrain.HistoryLen()`.
- **`loadShard` (L961–997).** Today it `continue`s on any non-`"message"` record.
  Extend it to **also** collect `usage`/`turn_error` records into the parsed result
  (a `shardRecord` gains a `kind` + optional `usage`/`turnErr`), preserving file
  order. Unknown kinds still skipped → forward/backward compatible.
- **`loadTranscripts` (L939).** Return per-agent **meta** records alongside the
  per-agent message transcripts (split by kind from `loadShard`).
- **`Adopt` (L252).** Set `st.metaPersisted[aid] = len(loaded meta records)` for
  each restored agent (twin of the existing `st.persisted` line), so the next save
  is a correct delta over the meta stream.
- **`LoadedSession` (L201).** Add `RootHistory []model.Turn` — the root agent's
  reconstructed per-round-trip `Turn`s (Usage/Error only), built from the loaded
  meta records. (Only root is reconstructed, matching the existing transcript
  restore which seeds only `Transcripts["root"]`.) Populated in `ListActive` and
  `LoadSession`.

`shardMeta.Events` now counts message **and** meta records (it bounds shard
rolling — fine, and is more accurately a "record count"). The Sessions browser's
`Messages` figure therefore includes meta lines; this is a cosmetic count shift on
the *new* records only, noted under regression risks.

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
          Error: lastModelError(ag)}) // typed error from History, falling back to the wrapped string
      return nil, fmt.Errorf("process message: %w", err)
  }
  ```
  This is the single synchronous entrypoint the **#481 dispatch goroutine** invokes
  (`dispatch.go:81`, under `context.Background()`), so persisting at its end
  captures the turn on the daemon-owned goroutine for **both** embedded and daemon
  paths — exactly when the turn goroutine completes, as #481 requires. (Optional
  polish: `lastModelError(ag)` reads `ag.ThoughtTrain.GetHistory()` last `Error` so
  the `HookError` event also carries the typed classification instead of the
  flattened string; the persisted record is correct regardless.)
- **`adoptLoaded` (L1807–1853).** After `sess.ReplaceTranscript(msgs)`, also
  `sess.RestoreHistoryMeta(ls.RootHistory)` so a restored session reconstructs the
  failure indicator (last `Turn.Error`) and token accounting
  (`CurrentTokenCount`) instead of coming back as a bare idle, zero-token session.

### 4. Read-only context (NOT edited)

- `internal/model/connection.go` — `ModelError` (L593), `ModelErrorType` (L580),
  `analyzeError` (L1593), `TokenUsage` (L441). Preserved as-is; `Type` persisted
  verbatim (#485-compatible).
- `internal/agent/user_session.go` — emission sites stay live-only. **No edit
  needed**: the typed error reaches the persistence layer through
  `ModelSession.History`, not through the `SessionEvent` stream or the returned
  error, so `runLoop`/`modelRoundTrip` are untouched.

---

## Restore alignment (why `RestoreHistoryMeta` is mandatory, not cosmetic)

The meta frontier (`metaPersisted`) is a `History`-index boundary, exactly as the
message frontier (`persisted`) is a transcript-index boundary. The message frontier
survives restore only because `adoptLoaded` re-seeds the transcript
(`ReplaceTranscript`) so live indices line up with on-disk counts. The meta stream
needs the same treatment: if `History` were left empty on restore while
`metaPersisted` was set to N (from the active shard), the next live turn would land
at `History[0]` while the delta read `GetHistory()[N:]` — out of range — and its
usage/error would **never** persist. Reconstructing `History` to length N via
`RestoreHistoryMeta` keeps the next delta correct *and* doubles as the
failure-indicator/token-count restore the acceptance criteria require.

Restore reads only the **active (latest) shard** (issue #26), so both the
reconstructed transcript and the reconstructed meta stream are active-shard-scoped
and mutually consistent — no change to the bounded-restore model.

---

## On-disk shape (illustrative active shard after one failed turn)

```jsonl
{"kind":"message","agent_id":"root","message":{"role":"user","content":"…"}}
{"kind":"message","agent_id":"root","message":{"role":"assistant","tool_calls":[…]}}
{"kind":"message","agent_id":"root","message":{"role":"tool","content":"…"}}
{"kind":"usage","agent_id":"root","at":"2026-06-26T…","usage":{"prompt_tokens":1234,…}}
{"kind":"message","agent_id":"root","message":{"role":"user","content":"…(2nd step)…"}}
{"kind":"turn_error","agent_id":"root","at":"2026-06-26T…","error":{"type":"context_overflow","http_status_code":400,"message":"…","raw_response":"…(≤8KiB)…"}}
```

The partial transcript (user + tool-call + tool-result + 2nd user message) is
present, the first round-trip's usage is present, and the failing round-trip's
classified error is present — debuggable purely from the file.

---

## The four design gates

### (1) Goal match — fix, not feature creep
Directly closes the three named gaps and nothing more: persist-on-error (call-site
move), preserve the typed `*ModelError` (`sendCtx`), persist per-round-trip usage +
structured failure (new additive kinds), restore both (adopt seed). Tool args/results
are already transcript messages — they persist automatically once the error-path
persist runs (no special-casing). Out-of-scope items (auto-compaction, refusal
handling, index layout, live event stream) are untouched. The only behavioural
*improvement* beyond the literal gaps is that restored **successful** sessions now
recover their token count (previously reset to 0) — squarely within "context-size
accounting lost," not creep.

### (2) Usability — the right thing is surfaced, not silent
A previously-failed session reopens with its error and token figures reconstructed
in session state (last `Turn.Error`, `CurrentTokenCount`) instead of looking like a
normal idle session ending on an unanswered prompt. A worker can debug a failed run
from the `.jsonl` alone — error class, HTTP status, message, the tool calls/results
that led there, and the prompt-token cost — with no live process, logs, or TUI.
Drawing a red marker in the TUI from the restored state is the **optional**
follow-up the issue scopes out; the state needed to draw it is now present.

### (3) No regressions
- **Happy path unchanged on disk:** message records are byte-identical (new
  `jsonlRecord` fields are `omitempty`); the `.index` layout is unchanged. A
  successful turn additionally writes one tiny `usage` line per round-trip.
- **Backward compatible both directions:** old message-only shards load (the reader
  simply finds zero meta records → `RestoreHistoryMeta(nil)` is a no-op); a new
  shard read by an old binary still works (it skips non-`"message"` kinds, as it
  always has).
- **Invariants preserved:** the delta/full-rewrite/epoch logic, shard rolling, and
  the issue-#17 "never lose a line" `errors.Join` aggregation all extend to meta
  records unchanged. No new lock ordering: `encodeTurnMeta` calls `GetHistory()`
  under the store lock, the same pattern as the existing `GetTranscript()` call —
  the store is never re-entered from `ModelSession`, so no inversion.
- **Tests:** `go build/vet/gofmt/golangci-lint` clean; `go test ./...` (no `-race`,
  Pi5) green except the pre-existing environmental `TestUserSessionSendMessage`
  404. Watch for any existing test asserting `CurrentTokenCount == 0` after restore
  — that now reconstructs a real count (an intended improvement; update if present).

### (4) Holistic across both repos
The change lives entirely in gogent's persistence + model-session seam
(`session_store.go` + `model_session.go` + the `gogent.go` call-site/adopt), which
is where session persistence is owned. **turbotui is untouched** — it is a pure
rendering library that owns no session state, so there is no `go.mod` bump and no
cross-repo coupling. The wire/persistence seam the codebase guards (`model.Message`
has a custom `MarshalJSON` that is *also* the provider wire shape, with `Reasoning`
deliberately stripped on send) is **respected**: usage/error are persisted as
separate store-layer records, **not** by bolting fields onto `model.Message`, so
nothing new ever leaks into an outbound provider request. Works on a `main` that
already includes #481 (persist runs when the dispatch goroutine's synchronous
entrypoint returns) and #485 (the classifier's `Type` is persisted verbatim, set of
types not hardcoded).

---

## Regression risks (called out)

- **Shard `Events`/browser `Messages` count** now includes meta lines. Cosmetic;
  affects only sessions written after this change. Acceptable (arguably more
  accurate as a record count). If undesirable, `encodeTurnMeta` could be excluded
  from the `Events` tally — not recommended (it would desync the byte/line caps).
- **Shard size** grows by ~1 small line per round-trip plus, on failures, a
  truncated raw-response blob (capped at 8 KiB). Bounded and far below the 10 MiB
  roll cap.
- **Restored token count for old successful sessions** stays 0 (no usage records on
  disk) — unchanged from today; only sessions saved after this change recover it.
- **`RawResponse` may contain sensitive provider output.** Truncating bounds size;
  full redaction is out of scope but the cap is the hook if a policy is added later.

---

## Tests to add (per the issue)

1. Fake connector returning a classified `*ModelError`
   (`Type`/`HTTPStatusCode`/`RawResponse`): assert the session file **is written**
   and contains a `turn_error` record with all three fields + message, and that the
   partial transcript (user message + earlier tool results) is persisted.
2. `usage` record persisted per successful round-trip; prompt-token usage captured
   in the `turn_error` record when the fake provides `resp.Usage` on the error path.
3. Restore: a session whose last turn failed reloads with `History` reconstructed —
   last `Turn.Error` set and `CurrentTokenCount` non-zero — not a bare idle session.
4. Backward-compat: a hand-written shard containing only `Kind:"message"` records
   loads cleanly (no meta → no-op seed).
5. Happy-path no-regression: a successful turn persists + restores its transcript
   exactly as before (plus the now-recovered token count).

---

## Open questions

1. **One kind or two?** Plan uses two human-readable kinds (`usage`, `turn_error`)
   for a legible debugging artifact, emitting at most one per round-trip. A single
   `turn` kind carrying both optional fields is simpler but less self-describing in
   the raw file. Proceeding with two unless you prefer one.
2. **`RawResponse` truncation cap.** Proposed 8 KiB. Confirm the size, and whether
   any redaction (e.g. stripping obvious auth echoes) is wanted now or deferred.
3. **Optional gap #6 (BUDGET_EXCEEDED fold).** Folding the budget-exceeded notice
   onto the transcript for parity with the truncation path lives in the agent loop
   (`internal/agent`, `budgetExceededMarker`), which the task asks to keep minimal.
   Recommend **deferring** it as a separate small change unless you want it in
   scope, since it is explicitly optional and the failure-record path already
   captures token-budget terminations that surface as model errors.
4. **`HookError` typed error (optional polish).** Surfacing the typed
   `*ModelError` on the `HookError` event (`lastModelError(ag)`) is a one-liner that
   improves live hooks but is not required by the acceptance criteria — include or
   drop?
