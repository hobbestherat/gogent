# Design — issue #449: step-cap exit orphans the final turn's tool calls and stops silently

**Issue (FIX, maintainer kloune):** "Step-cap exit orphans the final turn's tool
calls and stops silently; raise default to 100 and surface the stop." Closes #449.

## 1. Problem restated (verified against the code)

`runLoop` (internal/agent/user_session.go) runs the **first** round-trip *before*
the loop (`:1215`), then the loop body processes the **previous** `resp`
(`collectToolCalls` at `:1306`) and, at the bottom, `advance()`s the **next**
round-trip (`:1545`). With `maxSteps = N` the body runs `step = 0..N-1`; the last
iteration `advance()`s round-trip `#N+1` — which `sendCtx` has already **persisted
to the transcript with its `tool_calls`** — and then the `for` condition
`step < maxSteps` fails. The loop exits at `:1550` **without** the
`collectToolCalls(resp)` that the top of `step = N` would have run.

Consequences of that exit (`:1550-1560`), all confirmed:

1. **Orphaned tool calls.** The capped turn's `tool_calls` are never executed and
   never answered, so the persisted transcript ends on an unanswered
   `assistant{tool_calls}` message. OpenAI and Anthropic both reject a reused
   session whose previous `tool_calls` are not answered one-to-one → the next user
   turn **400s**. (`finalizeTranscriptToolCalls` at `:1889` already exists to fix
   exactly this class of imbalance for the two *other* tool-call exits — `:1461`,
   `:1510` — but the cap exit never calls it.)
2. **Silent stop.** The exit emits a plain `SessionEventFinal` with whatever the
   orphaned turn's `Content` was (usually empty → recovered via `lastAssistant`),
   indistinguishable from a clean completion — unlike `stopForBudget` (`:1024`) /
   `stopForTruncation` (`:1095`), which fold a *visible* `BUDGET_EXCEEDED` /
   `OUTPUT_TRUNCATED` marker. The session looks done; the task was abandoned
   mid-action.

The fix has two independent parts: **(1)** raise the default cap 25→100, and
**(2)** make the cap exit deterministic and visible at *any* cap value.

---

## 2. Files / functions touched

### gogent (github.com/hobbestherat/gogent)

**Part 1 — raise default cap (pure constant + the tests/docs that pin 25):**
- `internal/config/config.go` — `DefaultMaxSteps` 25 → **100**. Single source of
  truth: `MaxStepsOrDefault()` (`:626`), `GetDefaultConfig` (`:1018` via
  `intPtr(DefaultMaxSteps)`), and the gogent.go wiring all inherit it. Update the
  doc comment on `DefaultMaxSteps` (`:611`) and on the `MaxSteps` field (`:587`,
  which says "DefaultMaxSteps, 25" twice).
- `internal/config/config_maxsteps_test.go` —
  - `TestDefaultMaxStepsMatchesHistoricalBound`: relax the `== 25` hard-assert.
    The issue-#249 invariant ("unset reproduces the historical 25 bound") is
    deliberately being broken, so the test must now assert `DefaultMaxSteps != 25`
    **and** `== 100`, with its rationale comment rewritten: the default is no
    longer the historical bound; #249's *mechanism* (nil⇒default, 0⇒unlimited)
    still holds, only the default *value* changed (#449).
  - `TestGetDefaultConfigRoundTripsMaxSteps`: `"max_steps":25` literal → `:100`.
  - `TestMaxStepsRoundTripsThroughJSON`: the `"explicit 25"` case is an *explicit*
    pointer value, unaffected; the `"absent"` case already uses the symbolic
    `DefaultMaxSteps`. No change.
- `internal/agent/user_session_maxsteps_test.go` —
  - `TestNewUserSessionDefaultsMaxStepsToHistoricalBound`: the symbolic
    `== config.DefaultMaxSteps` stays; the hard-coded `!= 25` (`:39`) → `!= 100`,
    comment updated.
  - `TestDefaultMaxStepsCapsLoopAtHistoricalBound`: bump
    `makeToolCallResponses(t, 40, …)` → **`(t, 120, …)`** (the cap can only fire if
    the server keeps offering tool calls past request 100) and `const want = 26`
    → **`101`** (1 initial + 100 capped rounds). Also extend it for Part 2 (below).
  - `TestMaxStepsZeroIsUnlimitedRunsPastDefaultBound`: `toolRounds = 40` → **120**
    and `defaultCapRequests = 26` → **101**. **Required, not cosmetic:** at the new
    default, 40 rounds (41 requests) no longer exceeds 100, so the test's
    "runs PAST the default cap" assertion (`fs.calls <= defaultCapRequests`) would
    fail unless `toolRounds` clears 100.
  - `TestMaxStepsNegativeIsAlsoUnlimited`: same intent fix — `toolRounds = 30` →
    **120**, `<= 26` (`:162`) → `<= 101`. (Currently passes by luck; update so it
    still actually proves "past the default cap.")
  - `TestConfiguredMaxStepsCapsLoop` (`1,2,3,5,25`), `TestSetMaxSteps…`,
    `TestMaxStepsResolvedAtLoopStartNotMidRun` (unlimited; asserts 41): explicit /
    unlimited values — **no change**.
- `internal/agent/user_session_maxsteps_budget_subagent_test.go` —
  - `TestSubAgentLoopUsesHistoricalDefaultWhenUnwired`: the assertion
    `fs.calls == config.DefaultMaxSteps+1` is symbolic ✅, **but** its
    `makeToolCallResponses(t, 40, …)` (`:147`) must bump to **`(t, 120, …)`** so
    the server still offers a tool call at request 101 — otherwise the unwired loop
    reaches the final at request 41 and `got=41 != want=101`. (The brief said
    "confirm only"; the response count is an overlooked dependency.)
  - `TestSubAgentLoopHonoursUnlimited`: `toolRounds = 30` → **120**, `<= 26`
    (`:138`) → `<= 101` (same "past the default" intent fix).
  - `TestUnlimitedLoopStillStopsOnTokenBudget` / `TestBudgetStillStopsUnderConfiguredCap`:
    budget trips at ~3 requests, far under any cap; the `> 26` sanity bound
    (`:48`) stays valid at 100. **No change** (optionally relax the stale "26" in
    the message; not required).
- `docs/configuration.md` — table row (`:38`) and the `max_steps` section table
  (`:339`): `25` → `100` (keep `nil ⇒ default`, `0 ⇒ unlimited`, `N>0 ⇒ cap N`).
- `docs/architecture.md` — `DefaultMaxSteps=25` (`:139`) → `=100`; add one clause
  on the cap-exit notice (below) next to the existing budget/truncation sentence.

**Part 2 — fix orphaning + surface the stop (structural, cap-value-independent):**
- `internal/agent/user_session.go`:
  1. Add marker `stepLimitReachedMarker = "STEP_LIMIT_REACHED"` next to
     `budgetExceededMarker` (`:925`).
  2. Add `stopForStepLimit(resp, maxSteps)` mirroring `stopForBudget` /
     `stopForTruncation`: build the notice
     `"STEP_LIMIT_REACHED: reached the per-turn step cap (N); the task was
     interrupted. Type a message to continue."`, preserve any partial
     `resp.Content` beneath it (`"\n\nPartial progress before stopping:\n"+partial`),
     overwrite `resp.Content`, return `resp`. Nil-safe like its siblings.
  3. **Make the cap exit distinguishable from a clean break.** Restructure the
     loop header `for step := 0; maxSteps <= 0 || step < maxSteps; step++` into
     `for step := 0; ; step++` with the cap as an explicit *first* statement in the
     body:
     ```go
     capExit := false        // declared just before the loop
     for step := 0; ; step++ {
         if maxSteps > 0 && step >= maxSteps { capExit = true; break }
         if err := ctx.Err(); err != nil { … }   // unchanged, still first real check
         …
     }
     ```
     This is behaviour-identical to the old condition (unlimited when
     `maxSteps<=0`; same `N+1`-request count) but yields a clean `capExit`
     boolean. Putting the check *before* `ctx.Err()` preserves the old precedence
     (the for-condition failed before the body ran, so a cancellation landing
     exactly at the boundary did not pre-empt the cap exit). Every other
     termination is a `break` that leaves `capExit == false`.
  4. In the exit path (`:1550`), **before** the existing `SessionEventFinal`
     emit:
     ```go
     if capExit && resp != nil {
         if calls, _ := s.collectToolCalls(resp); len(calls) > 0 {
             resp = stopForStepLimit(resp, maxSteps)            // (a) visible notice
             s.finalizeTranscriptToolCalls(sess, resp, nil)    // (b) balance orphans
         }
     }
     ```
     The existing block then reads `resp.Content` (now the notice, partial beneath)
     as `finalText` and emits it as the `SessionEventFinal` — no second emit needed.
     - `collectToolCalls` is the *same* inspection the top of `step=N` would have
       done. When it returns `len(calls) > 0` (native or JSON-fallback calls), the
       turn was mid-action → fold + balance. When it returns `explicitFinal`
       (a `structured_output{final}` that happened to land on the cap) it yields
       `len(calls)==0`, so we **don't** mark a step-limit stop — that genuine final
       answer surfaces normally. Plain-text caps (no calls) likewise fall through
       to the normal final, matching the issue's scope ("the stuck turn had real
       tool calls"; `looksLikePreamble` explicitly out of scope).
     - `finalizeTranscriptToolCalls(sess, resp, nil)` synthesizes a
       `finalToolCallResultNote` tool-result for every unanswered native
       `tool_call_id` (it is a no-op for JSON-fallback "calls", which are plain
       text and need no balancing). The transcript becomes a balanced
       `assistant{tool_calls} → tool-results` sequence → a resumed session's next
       user turn is valid.
  5. `subAgentOutcome` (`:2550`): add `|| strings.HasPrefix(up, stepLimitReachedMarker)`
     beside the `budgetExceededMarker` check. **Required for consistency, not scope
     creep:** `runLoop` is shared, so a sub-agent that hits the cap now returns a
     final prefixed `STEP_LIMIT_REACHED`. Without this, that sub-agent would report
     `StatusCompleted` while carrying an "interrupted" notice — exactly the
     "looks finished, was abandoned" bug, one level down. A capped sub-agent did
     not finish its task, so it is a failure, identically to `BUDGET_EXCEEDED`.

### turbotui (github.com/hobbestherat/turbotui) — **no change**

Verified: turbotui has **zero** references to `SessionEventFinal`,
`BUDGET_EXCEEDED`, `OUTPUT_TRUNCATED`, or any marker
(`grep` of `$HOME/work/turbotui` → empty). The seam is one-directional: gogent
emits `SessionEventFinal{Text}`, turbotui renders the text as the final assistant
bubble. The existing budget/truncation markers are plain human-readable text
prefixes with no special client handling; `STEP_LIMIT_REACHED` follows the same
contract and renders correctly with no turbotui edit. The repo boundary is
respected — the new state lives entirely in gogent's agent loop.

---

## 3. User-facing behaviour

Before: a task that exceeds the cap mid-action shows the session quietly go
idle with no last message (or a stray empty/partial turn), the tool it was about
to run silently dropped, and the session unusable on resume (next message 400s on
a real backend).

After: the session ends with a clear final message —
> `STEP_LIMIT_REACHED: reached the per-turn step cap (100); the task was
> interrupted. Type a message to continue.`
> *(any partial text the model produced appears beneath it)*

and the user can **type a message to continue** on a transcript that is valid and
balanced. The user drives the continuation (the notice tells them exactly how);
nothing is silent and nothing is lost. The default ceiling moves 25→100 so most
real tasks finish before ever seeing this, while the notice keeps the cap honest
and explainable whatever value it is set to.

---

## 4. Design criteria

**(1) Goal match.** Exactly the two asks: raise the default to 100 (single
constant, inherited everywhere) and surface the previously-silent cap stop while
not orphaning the final turn's tool calls. No new config surface, no token-budget
wiring to the root loop, no preamble-heuristic change, no behaviour change for
unlimited (`maxSteps<=0` never sets `capExit`) or for explicit caps below the
data size. It is a FIX: it reuses the established `stopForBudget` /
`finalizeTranscriptToolCalls` machinery rather than inventing a parallel one.

**(2) Usability.** The stop is now a first-class, visible `SessionEventFinal`
carrying an actionable notice (mirrors BUDGET/TRUNCATION). The dialog/seam needs
no extra share: it is the ordinary final bubble. The user can drive — "type a
message to continue" resumes on a valid transcript. The right thing is surfaced,
not silent, and partial progress is preserved beneath the notice.

**(3) No regressions.** Loop restructure is behaviour-identical (same
`maxSteps<=0` unlimited semantics; same `N+1` request count; same break/return
exits; cancellation precedence preserved). The new exit block is gated on
`capExit && len(calls)>0`, so a clean final / budget / truncation / cancel /
explicit-final-at-cap path is untouched. Transcript invariant is *strengthened*:
the cap exit now satisfies the one-to-one `tool_calls↔results` balance that the
two sibling tool-call exits already maintain (the cap exit was the lone hole).
`collectToolCalls` is called once on the capped `resp` (no double-mutation; its
only side effect is the truncated-`structured_output` salvage, irrelevant for a
well-formed orphaned tool call). Test impact is enumerated in §2 and is mechanical
(symbolic assertions hold; literal `25/26` pins and the response-count inputs that
must clear 100 are listed). `subAgentOutcome` change breaks no existing assertion
(the sub-agent cap tests assert request counts / log the final, not status).

**(4) Holistic across both repos.** The fix belongs entirely in gogent's
`runLoop` — the seam where the cap, the transcript, and the `SessionEvent` stream
all live. turbotui consumes only `SessionEvent` text and needs nothing (verified
by grep). The downstream effect on the other repo is "the final bubble now shows a
clear notice instead of an empty/odd turn" — strictly an improvement, requiring no
client code. The marker convention (`STEP_LIMIT_REACHED`) matches the existing
`BUDGET_EXCEEDED` / `OUTPUT_TRUNCATED` family, keeping the gogent→turbotui contract
uniform.

---

## 5. Tests to add / update (internal/agent/user_session_maxsteps_test.go)

- **New `TestStepCapSurfacesNoticeAndBalancesOrphanedToolCalls`:** drive the loop
  past `maxSteps` with the capped turn carrying a real (calc) tool call
  (`SetMaxSteps(3)`, `makeToolCallResponses(t, 40, …)`). Assert:
  - (a) a `SessionEventFinal` is emitted (captured via `SetObserver`) whose text
    has prefix `STEP_LIMIT_REACHED` and contains the cap number;
  - (b) **no dangling unanswered calls** in the persisted transcript
    (`ag.ThoughtTrain.GetTranscript()`): every `assistant.ToolCalls[i].ID` has a
    matching `tool` message `ToolCallID` (set-subset check) — directly proving
    `finalizeTranscriptToolCalls` balanced the orphans;
  - (c) session is valid for resume: a second `ExecuteTaskLoop(...)` returns
    without error and the request it sends carries the synthesized tool results
    (no unanswered `tool_calls` from the prior turn).
- **Update `TestDefaultMaxStepsCapsLoopAtHistoricalBound`:** new counts
  (`120`/`want=101`) per §2, **and** add the Part-2 assertion that the cap-exit
  `SessionEventFinal` carries the `STEP_LIMIT_REACHED` notice (the capped turn is a
  tool call, so the notice must fire) — i.e. the cap exit is no longer silent even
  at the default cap.
- The numeric bumps in `TestMaxStepsZeroIsUnlimitedRunsPastDefaultBound`,
  `TestMaxStepsNegativeIsAlsoUnlimited`, `TestSubAgentLoopHonoursUnlimited`,
  `TestSubAgentLoopUsesHistoricalDefaultWhenUnwired`, and the config-test literal
  flips, per §2.

**Dev gate (per memory):** build, vet, gofmt, golangci-lint v2, and `go test`
**without `-race`** (Pi5).

---

## 6. Open questions

1. **Synthetic result note wording.** `finalizeTranscriptToolCalls` answers the
   orphaned calls with `finalToolCallResultNote = "[Final answer recorded and
   delivered to the user.]"` — semantically off for an *interrupted* call a
   resumed model may re-read. The issue explicitly says to reuse
   `finalizeTranscriptToolCalls`, so the design does. **Proposed default:** keep
   the reuse (minimal, transcript stays valid; the note is an internal balancing
   artifact the model rarely re-reads). A cheap optional refinement is a
   step-limit-specific result note (e.g. `"[Tool call not executed: the per-turn
   step cap was reached before it ran. Re-issue it if still needed.]"`) via an
   overload/param — flagging only; not doing it unless you want it.
2. **Persist the notice into the transcript?** `stopForTruncation` additionally
   folds its notice onto the persisted assistant message
   (`FoldLastAssistantContent`) because that turn was otherwise empty. Here the
   capped assistant turn has real `tool_calls` + balanced results, so the design
   **deliberately does not** persist the notice (it lives only in the emitted
   `SessionEventFinal`), leaving the resumed model the real partial content +
   results rather than an injected marker. Confirm this is the intended resume
   context (I believe it is the cleaner choice).
3. **`maxSteps` semantics labelling.** Code/comments call it a "per-task" /
   "per-turn" step cap interchangeably; the issue says "per-turn." Not changing
   the mechanism — just noting I'll keep the existing comments' wording except
   where the notice string uses "per-turn step cap" as the issue specifies.
