# Design — Issue #485: Empty 200 OK model response is not detected, retried, or surfaced

## Summary

When an OpenAI-compatible backend returns **HTTP 200 with an empty / whitespace-only
body**, gogent's blocking completion path (`complete`) treats it as success, breaks the
retry loop, and hands the empty bytes to the wire adapter, which `json.Unmarshal`s `""` →
`unexpected end of JSON input`. That is wrapped as a non-retryable `ErrorGeneric`, so the
turn dies on the **first** attempt even though `maxAttempts` defaults to 3. The streaming
path has the mirror bug: an empty stream parses to `("", nil, nil)` — a silently-empty
assistant turn with **no** error.

The fix introduces a dedicated, retryable error class and a guard on both paths:

- **Blocking** (`complete`): detect an empty/whitespace-only 200 *before* parsing, and
  **retry it inline** like the existing transient-failure paths (backoff + `continue`).
  After attempts are exhausted, return a clear `ErrorEmptyResponse` instead of the opaque
  unmarshal error.
- **Streaming** (`parseOpenAIStream`): detect a stream that produced **literally nothing**
  — no content, no tool calls, **no reasoning/thinking deltas**, no finish reason, no usage
  — and surface `ErrorEmptyResponse` instead of returning a silently-empty turn. The
  reasoning channel is part of the conjunction: a reasoning-model stream that streamed
  thinking and was then cut mid-turn is **not** empty and must not be flagged. (Streaming
  intentionally does **not** retry — see Gate 3 — so it surfaces rather than retries.)

The change is confined to `internal/model/connection.go`. No `adapter.go` change is
required; turbotui is untouched; no new dependencies.

---

## Exact files / functions touched

### `internal/model/connection.go` (only production file changed)

1. **New error type** — in the `ModelErrorType` const block (~line 582-591), add:
   ```go
   // ErrorEmptyResponse is returned when the backend replies 200 OK with an empty or
   // whitespace-only body (blocking) or a stream that yields no content/tool-calls/
   // usage/finish-reason at all (streaming). It is a known-transient failure mode of
   // OpenAI-compatible gateways (OpenRouter, Z.AI, vLLM, LiteLLM, …); the blocking path
   // retries it up to maxAttempts before surfacing it.
   ErrorEmptyResponse ModelErrorType = "empty_response"
   ```

2. **Blocking guard** — in `complete()`'s retry loop, the current block (~1350-1352):
   ```go
   if resp.StatusCode == http.StatusOK {
       break
   }
   ```
   becomes:
   ```go
   if resp.StatusCode == http.StatusOK {
       if len(bytes.TrimSpace(bodyBytes)) == 0 {
           // Empty/whitespace-only 200 from an OpenAI-compatible gateway: a transient
           // transport hiccup (early close / zero-length body), NOT a real completion.
           // Retry with backoff while attempts remain rather than break-and-parse, which
           // would otherwise unmarshal "" into `unexpected end of JSON input` (issue #485).
           if attempt < attempts-1 {
               if !sleepCtx(ctx, c.backoff(attempt, retryAfter)) {
                   return nil, ctxError(ctx)
               }
               continue
           }
           return nil, &ModelError{
               Type:    ErrorEmptyResponse,
               Message: fmt.Sprintf("model returned an empty response (HTTP 200, 0 bytes) after %d attempt(s)", attempts),
           }
       }
       break
   }
   ```
   - Reuses the **exact** idiom already used by the network-error and non-200 retry
     branches in the same loop: `attempt < attempts-1` → `sleepCtx(ctx, c.backoff(...))` →
     `continue`; otherwise terminal return. `retryAfter` is already parsed at ~line 1341
     (normally 0 for an empty 200 — harmless).
   - `bytes` is already imported (used by `bytes.NewReader`). `bytes.TrimSpace` covers
     `""`, `"   "`, `"\n"` uniformly, satisfying the whitespace-only acceptance case.

3. **Streaming guard** — in `parseOpenAIStream()` (~line 1476-1591):

   a. **Accumulate reasoning** (the parser currently only *forwards* reasoning deltas on
      `streamCh`; it never records whether any arrived). Next to `var content strings.Builder`
      (~1487) add `var reasoning strings.Builder`, and in the reasoning-delta branch
      (~1508-1512) write the delta into it before forwarding — mirroring how `content` is
      both written and forwarded, and how the Anthropic parser already keeps a `thinkingBuf`
      (adapter.go:621):
      ```go
      if r := ch.Delta.ReasoningContent; r != "" {
          reasoning.WriteString(r)
          streamCh <- StreamResponse{Reasoning: r, Role: ch.Delta.Role}
      } else if r := ch.Delta.Reasoning; r != "" {
          reasoning.WriteString(r)
          streamCh <- StreamResponse{Reasoning: r, Role: ch.Delta.Role}
      }
      ```

   b. **Detect the truly-empty stream** — after the tool calls are assembled (~line 1580)
      and **before** the terminal `streamCh <- StreamResponse{… Done: true}` send (~1583):
      ```go
      // An OpenAI-compatible gateway can answer 200 then send a zero-length / immediately-
      // closed stream. parseOpenAIStream would otherwise return ("", nil, nil) — a silently
      // empty assistant turn. Treat a stream that produced LITERALLY NOTHING — no content,
      // no tool calls, no reasoning, no finish reason, no usage — as an empty-response
      // failure (issue #485). The reasoning term is essential: a reasoning-model stream that
      // streamed thinking and was then cut before any finish/usage chunk is NOT empty
      // (see TestCompleteWithToolsStreamCtxPartialStreamDeliversDeltas); omitting it would
      // turn a previously-usable reasoning-only partial turn into a spurious error.
      if content.Len() == 0 && reasoning.Len() == 0 && len(toolCalls) == 0 &&
          finishReason == nil && usage == nil {
          return "", nil, &ModelError{
              Type:    ErrorEmptyResponse,
              Message: "model returned an empty response (streaming: no content, reasoning, tool calls, usage, or finish reason)",
          }
      }
      ```
   Returning before the terminal `Done` send means the consumer
   (`CompleteWithToolsStreamCtx` / `CompleteStream`) sees the error on `errCh` and does
   **not** assemble a spurious empty completion. With the reasoning term, a reasoning-only
   cut stream still falls through to the existing return and is delivered as before.

### No change required

- **`internal/model/adapter.go`** — the connection-layer guard catches the empty body
  before `parseResponse` is ever reached, so the optional "defensive empty check in
  `parseResponse`" is unnecessary. Leaving it out keeps the change localized. (A
  malformed-but-**non-empty** 200 still yields `ErrorGeneric` exactly as today — see Open
  Questions.)
- **`isRetryableStatus` / `analyzeError`** — both operate on HTTP **status codes** for the
  non-200 path. The empty-200 retry is realized **inline** in the loop (`continue`), so
  neither needs to learn about `ErrorEmptyResponse`. `analyzeError` is never reached for a
  200.
- **Downstream switches** — a repo-wide grep for `ModelErrorType` switches found none in
  `internal/agent`, `internal/server`, or `ui/tui` that branch on the error *type* for
  retry/recovery; those layers render `err.Error()` as a string. `ModelError.Error()`
  already formats `"empty_response: model returned an empty response …"`, which is the
  actionable message the issue asks for — so **no UI/agent code change is needed**.

### New test file

- **`internal/model/connection_empty_response_test.go`** (see Tests).

---

## User-facing behavior

Before: turn aborts mid-stream (typically right after a batch of tool calls) with
```
error: send message: 500 Internal Server Error: process message: model round-trip:
complete with tools: generic: failed to parse response: unmarshal response:
unexpected end of JSON input
```

After:
- A transient empty-200 now **self-recovers**: the loop retries (up to `maxAttempts`,
  with backoff) and the turn completes normally if any attempt returns a valid body.
- A persistent empty-200 surfaces, after retries, as:
  ```
  empty_response: model returned an empty response (HTTP 200, 0 bytes) after 3 attempt(s)
  ```
  which states the real cause instead of `unexpected end of JSON input`.
- A streaming turn that yields nothing now produces a clear `empty_response` error rather
  than a silent empty assistant message.

---

## Design criteria

### (1) Goal match
A pure **bug fix**, no feature/refactor scope creep. Implements exactly the four asks:
(a) detect empty/whitespace-only 200; (b) retry it via the existing backoff machinery up
to `maxAttempts`; (c) surface a distinct, actionable `ErrorEmptyResponse`; (d) mirror the
detection on the streaming path. The optional malformed-nonempty-200 retry and the
optional `adapter.go` defensiveness are deliberately **not** taken on (issue marks them
optional), keeping the change minimal.

### (2) Usability
The surfaced error is actionable and names the cause and attempt count
(`"… (HTTP 200, 0 bytes) after 3 attempt(s)"`). Transient hiccups become invisible
(auto-retry). The streaming path no longer **silently** swallows an empty turn — the right
thing is surfaced rather than hidden. The error type `empty_response` is distinct and
greppable for future telemetry. The streaming message is **accurate** because the guard
fires only when *nothing at all* arrived: it enumerates every channel checked (content,
reasoning, tool calls, usage, finish reason), so it can never mislabel a stream that did
deliver thinking-only deltas as "empty".

### (3) No regressions
- **Happy path unchanged**: a non-empty 200 still `break`s and parses exactly as before;
  the new branch only fires when `bytes.TrimSpace(bodyBytes)` is empty.
- **Existing retry/parse tests**: `TestCompleteRetryByStatus` and friends drive non-200
  statuses and valid 200 bodies — untouched by this change. The new empty case is additive.
- **Streaming false-positive guard**: the streaming detection requires **all five** of
  (no content, no reasoning, no tool calls, no finish reason, no usage) to be empty/nil, so
  a legitimate empty-content turn (which still carries a finish reason and/or usage) and a
  **reasoning-only cut stream** (which carries reasoning) are both **not** misclassified.
  Existing stream tests (`stream_test.go`, `stream_truncation_issue390_test.go`,
  `reasoning_stream_test.go`) all produce at least content, a tool call, reasoning, a
  finish reason, or usage, so they remain green.
- **Reasoning-only partial stream preserved (the critical regression to avoid)**:
  `TestCompleteWithToolsStreamCtxPartialStreamDeliversDeltas`
  (`reasoning_stream_test.go:639`) feeds exactly one `reasoning_content` chunk then an
  abrupt `Hijack`+`Close` — no content, tool calls, finish reason, or usage. Today this
  returns a `*CompletionResponse{Reasoning:"partial reasoning"}` with **no error**. Because
  the guard now includes `reasoning.Len() == 0` (step 3a accumulates it), the conjunction is
  **false** for this stream, so it falls through to the existing
  `return content.String(), usage, nil` path and the reasoning-bearing response is delivered
  unchanged — the call still returns `(resp, nil)`, not `(nil, ErrorEmptyResponse)`. A guard
  that omitted the reasoning term would silently flip this to `(nil, err)`, discarding the
  thinking; that is the regression this design explicitly prevents.
- **Streaming retry semantics preserved**: streaming deliberately does not retry (per the
  documented "a streamed response cannot be safely replayed mid-stream" contract on
  `CompleteWithToolsStreamCtx`); the fix **surfaces** `ErrorEmptyResponse` there rather
  than introducing a replay, so that invariant is respected.
- **Stats**: like the sibling early-return branches (network error, ctx cancel), the
  terminal empty-200 return happens before the post-loop `RequestCount`/`SuccessCount`
  bookkeeping, so it does not inflate success counters. No stats-test assertions touch this
  path. (Intentionally not adding new stat mutation to avoid widening the blast radius.)
- `gofmt`/`go vet`/`go build`/`golangci-lint` clean; `go test ./...` green except the
  known-environmental `TestUserSessionSendMessage` 404.

### (4) Holistic design across both repos
- **Right seam / right place**: the bug is a model-HTTP-transport concern, and the fix
  lives entirely in the transport layer (`internal/model/connection.go`), reusing the
  existing `maxAttempts`/`backoff`/`sleepCtx`/`ctxError` machinery rather than inventing a
  parallel retry mechanism. The error classification (`ModelErrorType`) is the established
  vocabulary for model failures and is the correct abstraction to extend.
- **turbotui untouched and provably decoupled**: turbotui (read-only clone at
  `$HOME/work/turbotui`) has **zero** `.go` references to `ModelError`, `ModelErrorType`,
  `empty_response`, or `hobbestherat/gogent` — it does not import gogent's model layer at
  all, so there is no compile-time seam to break. The new error type is internal to
  `internal/model`; `ModelError.Error()` formats it automatically for any string-rendering
  UI, with **no** API change, `go.mod` bump, or new exported symbol to adopt. No new
  dependency in either repo.
- **Downstream wrap chain** (`model_session.go` → `user_session.go` → `gogent.go` →
  `server/messages.go` → `ui/tui/api_client.go`) is pure `%w`/`%s` wrapping and needs no
  change; it simply now carries the clearer leaf message.

---

## Tests (`internal/model/connection_empty_response_test.go`)

All use `httptest.NewServer` and `newTestConn(server.URL)` (backoff already disabled:
`retryBaseDelay=0`, `retryMaxDelay=0`) so no real sleeping. `maxAttempts` defaults to 3.

1. **Empty 200 exhausts retries** — server always returns 200 + empty body. Assert the
   handler is hit `maxAttempts` (3) times and `Complete` returns a `*ModelError` with
   `Type == ErrorEmptyResponse`.
2. **Whitespace-only bodies** — table over `""`, `"   "`, `"\n"`, `"\t \n"`; each behaves
   identically to (1).
3. **Empty-then-valid recovers** — server returns empty on the first call(s) then a valid
   `CompletionResponse`; assert `Complete` ultimately **succeeds**, returns the valid
   content, and the request count matches the retry count.
4. **Streaming empty stream detected** — server returns 200 with an empty SSE body; drive
   `CompleteStream` / `parseOpenAIStream` and assert it yields an `ErrorEmptyResponse`
   (not `("", nil, nil)` / a silent empty turn).
5. **Non-empty happy path unaffected** — a single valid 200 parses and returns with
   exactly **one** request (no extra retries), guarding against accidental retry of good
   responses.
6. **(Streaming sanity — finish reason)** — a stream that carries a finish reason but empty
   content is **not** flagged as empty (guards the conjunction's `finishReason` term).
7. **(Streaming sanity — reasoning-only cut, the Defect-A guard)** — a stream that delivers
   only a `reasoning_content` delta then is cut (no content, tool calls, finish reason, or
   usage) must **not** be flagged `ErrorEmptyResponse`: assert the call returns a
   `*CompletionResponse` with `Reasoning != ""` and a **nil** error. (Directly exercises the
   `reasoning.Len() == 0` term; without it this case regresses to `(nil, err)`. This is the
   shape `TestCompleteWithToolsStreamCtxPartialStreamDeliversDeltas` already feeds, but that
   test only asserts no-hang + sink delivery, so a dedicated `err == nil` assertion is added
   here rather than relying on it.)

---

## Open questions

1. **Malformed (non-empty) 200**: the issue lists "retry a malformed 200" as *optional*.
   Doing so cleanly requires moving `parseResponse` inside the retry loop (it currently
   runs after the loop), which is a larger restructure with more regression surface. This
   design covers **empty/whitespace** (the reported failure) and leaves a malformed
   non-empty body as `ErrorGeneric`, unchanged. Recommend deferring unless the reviewer
   wants it folded in now.
2. **Stats counter for empty-200 failures**: kept consistent with the existing early-return
   branches (no stat mutation on terminal empty-200). If observability into empty-200
   frequency is wanted, we could add an `EmptyResponseCount` to `ModelStats` — but that is
   scope beyond the issue; flagging for a maybe-later.
3. **`adapter.go` defensiveness**: deliberately skipped (the connection-layer guard fully
   covers the empty case). Worth adding only if another caller bypasses `complete()` and
   feeds raw bytes to `parseResponse` — none does today.
4. **Anthropic / Gemini *streaming* empty responses remain unguarded (intentional scope
   line)**: the streaming guard is added only to `parseOpenAIStream`, which the issue names
   explicitly and which is the path the reported gateways (OpenRouter, Z.AI, vLLM, LiteLLM)
   use. The Anthropic (`adapter.go:598`) and Gemini (`adapter.go:1254`) `parseStream`
   implementations still return `("", usage/nil, nil)` on an empty SSE. Their **blocking**
   empty-200s *are* covered for free (the `complete()` guard is adapter-agnostic). A
   universal streaming guard is **not** chosen here because the naive adapter-agnostic
   alternative (a `full == "" && usage == nil && err == nil` check hoisted into
   `completeStream`) reintroduces exactly Defect A: it cannot see the reasoning channel
   (`full` is content only), so it would misflag reasoning-only cut streams on every
   adapter. Per-parser reasoning-aware detection is the correct shape; extending it to the
   Anthropic/Gemini parsers is deferred as out-of-scope for this issue (flagged for a
   maybe-later, since no reported gateway uses those streaming paths).
