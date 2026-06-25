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
- `internal/gogent/maxsteps_wiring_test.go` — **🔴 build break the first pass
  missed.** `TestCreateUserSessionDefaultConfigKeepsHistoricalBound` hard-asserts
  `us.MaxSteps() != 25` (`:82`) → **`!= 100`**; its name + comment (`:67` "yields
  the historical 25-step cap … no behaviour change") assert an invariant we are
  *deliberately* breaking (#449), so rename to e.g.
  `TestCreateUserSessionDefaultConfigUsesDefaultCap` and rewrite the rationale to
  "the full GetDefaultConfig→CreateUserSession path yields `DefaultMaxSteps` (100)".
  `TestCreateUserSessionWiresMaxStepsFromConfig`'s `"nil resolves to default 25"`
  case *value* is symbolic (`config.DefaultMaxSteps`, ✅); only fix the case-name
  string (`:46`) and header comment (`:38`). The explicit-0/9/negative cases are
  unaffected.
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
  4. **Add a step-limit-specific synthesized result note + parameterize the
     balancer.** `finalToolCallResultNote = "[Final answer recorded and delivered
     to the user.]"` (`:364`) is accurate for a *folded final* but actively
     **wrong** for a tool call that was never executed — a resumed model re-reading
     it would think the work was done. Add:
     `stepLimitToolCallResultNote = "[Tool call not executed: the per-turn step cap
     was reached before this call ran. Re-issue it if still needed.]"`. Generalize
     the balancing loop into `balanceTranscriptToolCalls(sess, resp, executed, note)`
     and make the existing `finalizeTranscriptToolCalls` a thin delegate that passes
     `finalToolCallResultNote` (so its two current callers `:1461`/`:1510` are
     **unchanged**). The step-limit path passes `stepLimitToolCallResultNote`.
  5. In the exit path (`:1550`), **before** the existing `SessionEventFinal` emit,
     handle the capped-but-unprocessed `resp`. The orphaned `resp` is exactly what
     the top of `step=N` would have inspected, so we branch the *same* way that
     iteration would have — distinguishing a genuine final that merely *landed* at
     the cap from a tool round that was *interrupted* by it:
     ```go
     if capExit && resp != nil {
         calls, explicitFinal := s.collectToolCalls(resp)
         switch {
         case explicitFinal || containsTerminalFinal(calls):
             // A GENUINE final answer landed exactly at the cap. collectToolCalls
             // already folded the text-embedded / truncated-salvage forms into
             // resp.Content; foldTerminalFinal folds a well-formed NATIVE
             // structured_output{final} the serial runner never reached. Surface it
             // as the normal final — NOT a step-limit stop — and balance its
             // sibling/terminal calls with the (accurate) "recorded and delivered" note.
             foldTerminalFinal(resp, calls)               // no-op unless a native final needs folding
             s.finalizeTranscriptToolCalls(sess, resp, nil)
         case len(calls) > 0:
             // Real, non-final tool calls orphaned mid-action: surface the stop and
             // balance them with the "not executed" note.
             resp = stopForStepLimit(resp, maxSteps)       // (a) visible notice
             s.balanceTranscriptToolCalls(sess, resp, nil, stepLimitToolCallResultNote) // (b)
         }
         // default: plain-text cap (no calls) → fall through to the normal final.
     }
     ```
     The existing block then reads `resp.Content` (the folded answer, or the notice
     with partial beneath) as `finalText` and emits the `SessionEventFinal` — no
     second emit.
     - **Why the terminal-final branch is required (critic correctness fix).** A
       *well-formed native* `structured_output{final}` returns from
       `collectToolCalls` as an ordinary call with **`explicitFinal == false`**
       (`:1806`/`:1812`; the `explicitFinal=true` salvage at `:1791` fires only for
       a *truncated* sole call, and the text-embedded branch `:1828` only for JSON
       text). So a gate of `len(calls) > 0` alone would mis-stamp a real final
       answer — landing on the cap turn — as "interrupted" and overwrite it with the
       notice, dropping the model's answer. This is reachable whenever a turn ends
       on a native `structured_output{final}` at the cap, and is the **standard**
       sub-agent finish path (sub-agents end via `structured_output`). Adding
       `containsTerminalFinal(calls)` (the loop's own existing predicate) plus a
       tiny `foldTerminalFinal(resp, calls)` helper — mirroring the serial runner's
       fold at `:1718-1722` (`resp.Content = call.Args["response"]`) — folds and
       surfaces it correctly.
     - `balanceTranscriptToolCalls`/`finalizeTranscriptToolCalls` synthesize a
       tool-result for every unanswered native `tool_call_id` (no-op for plain-text
       / JSON-fallback turns, which carry no native `tool_calls`). The transcript
       becomes a balanced `assistant{tool_calls} → tool-results` sequence → a
       resumed session's next user turn is valid, and (with the step-limit note) the
       resume context now *accurately* says the calls were not executed.
  6. `subAgentOutcome` (`:2550`): add `|| strings.HasPrefix(up, stepLimitReachedMarker)`
     beside the `budgetExceededMarker` check. **Required for consistency, not scope
     creep:** `runLoop` is shared, so a sub-agent that hits the cap mid-action now
     returns a final prefixed `STEP_LIMIT_REACHED`. Without this it would report
     `StatusCompleted` while carrying an "interrupted" notice — the "looks finished,
     was abandoned" bug one level down. A capped sub-agent did not finish, so it is
     a failure, identically to `BUDGET_EXCEEDED`. (A sub-agent that *did* finish via
     `structured_output{final}` at the cap takes the terminal-final branch above, so
     its `SUCCESS:`/answer is preserved and it is **not** mis-marked failed.)

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
message to continue" resumes on a valid transcript. **Resume context is now
accurate** (critic fix): orphaned calls are balanced with the dedicated
`stepLimitToolCallResultNote` ("Tool call not executed … re-issue if needed")
instead of the misleading "Final answer recorded and delivered" — a resumed model
reads the truth that the calls did not run. A genuine final answer that merely
*landed* at the cap is surfaced verbatim (not mis-stamped "interrupted"). Partial
progress is preserved beneath the notice.

**(3) No regressions.** Loop restructure is behaviour-identical (same
`maxSteps<=0` unlimited semantics; same `N+1` request count; same break/return
exits; cancellation precedence preserved). The new exit block runs **only** on a
cap exit (`capExit`), and within it the `explicitFinal || containsTerminalFinal`
branch protects a genuine final landing at the cap, the `len(calls)>0` branch
handles real orphans, and a plain-text cap falls through to the normal final — so
clean-final / budget / truncation / cancel paths are untouched. Transcript
invariant is *strengthened*: the cap exit now satisfies the one-to-one
`tool_calls↔results` balance the two sibling tool-call exits already maintain (the
cap exit was the lone hole). `collectToolCalls` is called once on the capped `resp`
(no double-mutation). `finalizeTranscriptToolCalls` keeps its signature and the
two existing callers (`:1461`/`:1510`) are unchanged — only a new
`balanceTranscriptToolCalls(…, note)` is factored out beneath it. **Test impact is
now fully enumerated in §2**, including the previously-missed build break
`internal/gogent/maxsteps_wiring_test.go:82` (`!= 25`), every literal `25/26` pin,
and the response-count inputs (`makeToolCallResponses` n) that must clear 100 for
their cap/`past-the-default` assertions to hold. The `subAgentOutcome` addition
breaks no existing assertion and gains a direct table case (§5).

**(4) Holistic across both repos.** The fix belongs entirely in gogent's
`runLoop` — the seam where the cap, the transcript, and the `SessionEvent` stream
all live. **Correction to the first pass:** the `SessionEvent` *consumer* is
gogent's own `ui/tui` (`tui.go` `case agent.SessionEventFinal` → plain bubble;
`eventNotification` → `firstLine(ev.Text)`), **not** turbotui — turbotui is a
generic TUI widget library (`module github.com/hobbestherat/turbotui`) that does
not import gogent and has zero references to `SessionEventFinal` or any marker
(grep-verified). gogent's `ui/tui` does **not** special-case the
`BUDGET_EXCEEDED`/`OUTPUT_TRUNCATED` text prefixes (the `budgetExceeded` enum in
`session_window.go` is the budget-*bar* status, unrelated to message markers), so
`STEP_LIMIT_REACHED` renders correctly with **no client edit in either repo**. The
downstream effect is "the final bubble shows a clear notice instead of an
empty/odd turn" — strictly an improvement. The marker convention matches the
existing `BUDGET_EXCEEDED`/`OUTPUT_TRUNCATED` family, keeping the contract uniform.

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
- **New `TestStepCapWithTerminalFinalSurfacesAnswerNotNotice` (critic regression):**
  build a response sequence whose *capped* turn is a **well-formed native
  `structured_output{final:true, response:"DONE-ANSWER"}`** (not a calc call) —
  e.g. `SetMaxSteps(2)` with `[calc, calc, structured_output{final}]`. Assert the
  `SessionEventFinal` text is `DONE-ANSWER` (the model's real answer), **not** the
  `STEP_LIMIT_REACHED` notice, and that the transcript is balanced. This is the
  direct guard for the `explicitFinal || containsTerminalFinal` branch — without it
  the regression (dropping the answer, stamping "interrupted") is silent.
- **New `subAgentOutcome` table row** in `internal/agent/budget_test.go` (`:51-54`,
  the existing `SUCCESS`/`FAILURE`/`BUDGET_EXCEEDED`/plain table):
  `{"step limit", stepLimitReachedMarker + ": reached the per-turn step cap (100); …", StatusFailed}`.
  Directly pins the §2-step-6 classification rather than relying on indirect cap
  tests. (Also refresh the stale "25-step cap" wording in that file's comments at
  `:67`/`:90`; the assertions there — `fs.calls > 4` — are non-breaking.)
- **Update `TestDefaultMaxStepsCapsLoopAtHistoricalBound`:** new counts
  (`120`/`want=101`) per §2, **and** add the Part-2 assertion that the cap-exit
  `SessionEventFinal` carries the `STEP_LIMIT_REACHED` notice (the capped turn is a
  tool call, so the notice must fire) — i.e. the cap exit is no longer silent even
  at the default cap.
- The numeric bumps in `TestMaxStepsZeroIsUnlimitedRunsPastDefaultBound`,
  `TestMaxStepsNegativeIsAlsoUnlimited`, `TestSubAgentLoopHonoursUnlimited`,
  `TestSubAgentLoopUsesHistoricalDefaultWhenUnwired`; the config-test literal flips;
  and the `internal/gogent/maxsteps_wiring_test.go` rename + `!= 100` flip, per §2.

**Dev gate (per memory):** build, vet, gofmt, golangci-lint v2, and `go test`
**without `-race`** (Pi5). The full package test run (not just the maxsteps files)
is the backstop that catches any further hardcoded `25` the sweep missed.

---

## 6. Open questions

1. **(RESOLVED — was OQ#1) Synthetic result note wording.** Now fixed per the
   critic: orphaned calls are balanced with the dedicated
   `stepLimitToolCallResultNote` ("Tool call not executed … re-issue if needed"),
   added alongside `finalToolCallResultNote`, via a factored
   `balanceTranscriptToolCalls(…, note)`. The folded-final branch still uses the
   accurate `finalToolCallResultNote`. No remaining ambiguity; left here only to
   record the decision. (The issue said "reuse `finalizeTranscriptToolCalls`"; we
   honor that — `finalizeTranscriptToolCalls` is kept and delegates — while giving
   the genuinely-unexecuted case truthful wording.)
2. **(RESOLVED — was OQ#2) Persist the notice into the transcript?** Decision:
   **do not.** `stopForTruncation` folds its notice onto the persisted assistant
   message only because that turn was otherwise empty; here the capped assistant
   turn has real `tool_calls` + (now truthfully-noted) balanced results, so the
   STEP_LIMIT notice lives only in the emitted `SessionEventFinal`. The resumed
   model sees the real partial content + accurate "not executed" results rather
   than an injected marker. With OQ#1 resolved, the earlier misleading-resume
   concern is gone. Flagging for maintainer awareness, not as a blocker.
3. **`maxSteps` semantics labelling.** Code/comments call it a "per-task" /
   "per-turn" step cap interchangeably; the issue says "per-turn." Not changing
   the mechanism — just noting I'll keep the existing comments' wording except
   where the notice string uses "per-turn step cap" as the issue specifies.
