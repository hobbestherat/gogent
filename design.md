# Design — Issue #589 "Context window issues"

Branch: `pair1/context-window-as-model-setting-and-safe`
Scope: gogent-only, stdlib-only, no new deps. Lane = `internal/model` + `internal/agent`. **No `ui/tui` and no `internal/gogent` wiring changes.**

> Revision 2 — resolves design-critic findings D1 (daemon/SSE error silently dropped), D2 (guard over-fired every turn), D3 (error reported the pre-compression size), plus the parity nits.

---

## 0. Investigation result — what already exists

Verified against the code (not assumed):

**Part A (context window as a model setting) is already implemented in mainline:**
- `internal/config/config.go:47` — `ContextWindow int \`json:"context_window,omitempty"\`` on `ModelConfig`, documented as the input-window budget, deliberately distinct from `MaxTokens`.
- `config.go:138` — `const defaultContextWindow = 32768`; `config.go:143` — nil-safe `(*ModelConfig).ContextWindowOrDefault()`.
- `omitempty` + standard `json.Unmarshal` (`LoadConfig`/`SaveConfig`, config.go:1041/:1065) already give backward-compatible round-trip; shipped defaults all set it (config.go:1147…1278).
- Wired at `gogent.go:2685`: `ag.ThoughtTrain.SetMaxContextLength(selectedConfig.ContextWindowOrDefault())`; compaction water-marks (80%/50%) calibrate against it (`model_session.go:318,331`).
- Tests cover the accessor/defaults: `config_test.go` `TestContextWindowOrDefault`, `TestDefaultConfigSetsContextWindow`, `TestContextWindowDistinctFromMaxTokens`.

⇒ **Part A code change: none.** One additive persistence test is missing (see Part A).

**Part B (safe migration to a smaller window) does NOT exist.** At `gogent.go:2669-2686` every user message re-binds the session via `Resume` (`model_session.go:394`, which recomputes `CurrentTokenCount = lastUsageTotal(History)` — the real whole-conversation size) then `SetMaxContextLength`. **No guard**: switching to a model with a smaller window sends the next request over-budget. The in-loop `compactIfNeeded` (`user_session.go:1730`) only fires inside `runLoop` at 80% and does a **single** `SafeSplit`+`Summarize` pass — it cannot rescue a transcript whose verbatim recent turns already exceed a much smaller target. This is the maintainer's "multi-step chunked compression fallback."

---

## Part A — finish & lock the model setting (test-only)

Field, default, accessor, persistence, wiring, and accessor tests already exist. **Code change: none.** Add `TestContextWindowJSONRoundTrip` to `internal/config/config_test.go`:
1. Decode a hand-written *old* config blob with no `context_window` → assert `ContextWindow == 0` and `ContextWindowOrDefault() == defaultContextWindow` (back-compat).
2. `SaveConfig`→`LoadConfig` a config whose model sets `ContextWindow: 200000` → assert it survives unchanged.

---

## Part B — safe migration + bounded chunked-compression fallback

### B.1 Where the guard lives (resolves D1 cleanly, no gogent.go edit)

`ExecuteTaskLoopWithModel(ctx, agentID, message, modelConfig)` (`user_session.go:2462`) currently **drops `modelConfig`** and just delegates to `ExecuteTaskLoop`. It is the single synchronous entrypoint every dispatch path funnels through, and it already holds `ctx` (carrying the turn id) **and** the raw `modelConfig`. Put the guard here:

```go
func (s *UserSession) ExecuteTaskLoopWithModel(ctx context.Context, agentID, message string, modelConfig *config.ModelConfig) ([]*model.CompletionResponse, error) {
    if ag := s.GetAgent(agentID); ag != nil && ag.ThoughtTrain != nil {
        if err := s.MigrateToContextWindow(ctx, ag.ThoughtTrain, modelConfig); err != nil {
            return nil, err // already emitted SessionEventError internally (see below)
        }
    }
    return s.ExecuteTaskLoop(ctx, agentID, message)
}
```

This runs **after** `gogent.go:2680/2685` Resume+SetMaxContextLength (so `CurrentTokenCount` is the fresh real size) and **before** `runLoop`. Critically, the returned error then flows through the **existing** terminal handling at `gogent.go:2705-2727` — `persistSession` (saves the compacted transcript / failure record, issue #487), `NotifyHooks(HookError)`, and `return nil, fmt.Errorf("process message: %w", err)` — with **no new code in `internal/gogent`**.

**D1 fix — the error is no longer silent on the daemon/SSE path.** `MigrateToContextWindow` emits the failure itself, in-package, using the same plumbing `runLoop` uses:

```go
s.emit(SessionEvent{Type: SessionEventError, Err: err, TurnID: turnIDFrom(ctx)})
```

`turnIDFrom` (`user_session.go:171`, unexported — so this must live in `internal/agent`, not be wired from `gogent`) stamps the originating turn; `s.emit`→`s.observer`→hub→SSE is exactly the path `EmitError`/runLoop use. This makes a migration failure surface **identically to any in-loop error on every path**:
- **Daemon** (`dispatch.go:81` discards the returned error): the SSE client gets `SessionEventError` from the internal emit — the previously-silent path is fixed.
- **Embedded** (`cmd/embedded_handlers.go:62` re-emits on the returned error): same emit-and-return contract `runLoop` already uses (`user_session.go:1342` emits **and** returns the error), so behavior matches existing loop errors with **no new divergence**.

(Retracting Rev-1's "no new plumbing" claim: the plumbing — `emit`/`turnIDFrom`/observer→SSE — already exists; the bug was simply that nothing emitted on this pre-loop path. We now emit.)

### B.2 The bounded chunked algorithm (resolves D2 + D3)

Add to `internal/agent/user_session.go`:

```go
const (
    maxMigrationRounds      = 8   // hard cap on compression rounds (bounded; no infinite loop)
    migrationTargetFraction = 0.8 // compress to ≤80% of the window → 20% headroom for the next turn
)

func (s *UserSession) MigrateToContextWindow(ctx context.Context, sess *model.ModelSession, cfg *config.ModelConfig) error
```

**Steps:**

1. `if sess == nil || cfg == nil { return nil }`.
2. `target := cfg.ContextWindow` — the **raw** field, not `ContextWindowOrDefault()`.
   `prev := sess.LastMigrationWindow(); sess.SetLastMigrationWindow(target)` (new per-session state, §B.3).
3. **Unknown window:** `if target <= 0 { return nil }` — no guard, today's behavior preserved (acceptance requirement).
4. **D2 fix — only act on an actual shrink:** `if prev > 0 && target >= prev { return nil }`.
   A same-model turn has `target == prev` ⇒ skip entirely; a switch to an *equal-or-larger* window ⇒ skip. In both cases ordinary growth is left to the in-loop `compactIfNeeded` exactly as today — so the chunked fallback **never alters same-model compaction cadence or cost**. It engages only when the target window is strictly smaller than the one the session was last calibrated to (a genuine migration), or on the first calibration of a freshly *resumed* large transcript (`prev == 0`), where step 5's fit-check makes a small transcript a no-op anyway.
5. **Fits ⇒ no compression:** `fit := int(float64(target) * migrationTargetFraction); if sess.GetCurrentTokenCount() <= fit { return nil }`.
6. **Chunked loop**, `keep := compression.DefaultKeepRecentTurns` (3):
   ```
   for round := 0; round < maxMigrationRounds; round++ {
       if sess.GetCurrentTokenCount() <= fit { return nil }
       before := sess.GetCurrentTokenCount()
       older, recent := compression.SafeSplit(sess.GetTranscript(), keep)
       if len(older) == 0 {                 // nothing older at this depth
           if keep <= 1 { break }           // maximal: can't fold further
           keep--; continue
       }
       digestMsg, ok := s.summarizeOlder(sess, older)   // shared helper, §B.4
       if !ok { break }                     // compression backend failed/empty → stop, fail cleanly below
       sess.ApplyCompressedTranscript(append([]model.Message{digestMsg}, recent...))
       s.bumpCompaction(sess, digestMsg)    // compactionCount++ and emit SessionEventCompaction
       if sess.GetCurrentTokenCount() >= before { // no progress this round
           if keep <= 1 { break }           // can't shrink further → bail (early, avoids spinning 8 rounds)
           keep--
       } else if keep > 1 {
           keep--                            // compress harder next round (intended; loop bounded by the cap)
       }
   }
   ```
7. **Final fit check:** `if sess.GetCurrentTokenCount() <= fit { return nil }`.
8. **Fail cleanly (D3 fix — report the ACHIEVED size, never panic, never truncate):**
   ```go
   err := fmt.Errorf("cannot fit this conversation into model %q: its context window is %d tokens, "+
       "but the conversation is still ~%d tokens after maximal compression. "+
       "Start a new session or switch to a model with a larger context window.",
       displayName(cfg), target, sess.GetCurrentTokenCount())   // ← live, post-loop count, not the stale pre-loop size
   s.emit(SessionEvent{Type: SessionEventError, Err: err, TurnID: turnIDFrom(ctx)})
   return err
   ```
   (`displayName(cfg)` = `cfg.DisplayName` falling back to `cfg.Name`.) The message names the target model, its window, and the **achieved** size.

**Termination/determinism:** `keep` is monotonically non-increasing, floored at 1; the loop is hard-capped at `maxMigrationRounds`; the no-progress branch breaks at `keep<=1` (so a non-shrinking digest can't spin 8 futile LLM round-trips). With a stub completer the path is fully deterministic and hermetic.

**Post-migration, no double compression:** `ApplyCompressedTranscript` sets `compressSuppressed = true` (`model_session.go:380`) and leaves the count ≈80%, so `runLoop`'s first `compactIfNeeded` (`user_session.go:1332`) sees `NeedsCompression()==false` and no-ops.

### B.3 New per-session state (resolves D2's "actual change" requirement)

Add to `internal/model/model_session.go` (in lane), guarded by the existing `s.mu`:
```go
// LastMigrationWindow records the raw target context window the migration guard
// last evaluated, so a same-or-larger window switch can skip the chunked fallback
// and leave ordinary growth to NeedsCompression/compactIfNeeded.
func (s *ModelSession) LastMigrationWindow() int
func (s *ModelSession) SetLastMigrationWindow(int)
```
This is independent of `MaxContextLength` (which is already overwritten by `SetMaxContextLength` at gogent.go:2685 before the guard runs, and uses the never-zero `ContextWindowOrDefault`, so it can't carry the raw-or-`prev` signal). It is per-`ModelSession`, so the root agent and sub-agents (separate `ThoughtTrain`s) never interfere; only the root goes through `ExecuteTaskLoopWithModel`.

### B.4 Shared helper (DRY parity nits)

Extract the compression step `compactIfNeeded` already performs (pick fast completer under `RLock` else session backend → `compression.NewCompressionAgent(...).Summarize(older)` → build `model.Message{Role: RoleUser, Content: "[Earlier conversation summarized to save context]\n\n" + digest}`) into:
```go
func (s *UserSession) summarizeOlder(sess *model.ModelSession, older []model.Message) (model.Message, bool)
```
**Both** `compactIfNeeded` and `MigrateToContextWindow` call it, so the two paths emit **byte-identical digest messages** (parity nit), and the completer-selection lock discipline lives in one place. **Locking parity:** the helper reads `s.compressionCompleter` under `s.mu.RLock()` and **never** holds `UserSession.mu` across `sess.ApplyCompressedTranscript()` (which takes the `ModelSession`'s own mutex); `bumpCompaction` takes `s.mu.Lock()` only to `compactionCount++`, then emits — exactly mirroring `compactIfNeeded` (`user_session.go:1750-1773`).

---

## The 4 design gates

**(1) Goal match — OK.** Exactly the two asks: (A) context window is a real, persisted per-model setting with a safe default + back-compat (already in place, locked with a round-trip test); (B) migrating an over-budget session to a smaller-window model triggers a **bounded multi-step chunked** compression that fits-or-fails-cleanly and **never panics**. No new tokenizer, no TUI editor (deferred), no unrelated model-behavior change.

**(2) Usability — OK.** The user drives by choosing the model; the system reacts. Fits ⇒ invisible (no needless compression, no extra latency/LLM cost). Must compress ⇒ each round emits the existing `SessionEventCompaction`. Cannot fit ⇒ **one clear sentence naming the model, its window, and the achieved size**, emitted as `SessionEventError` on **both** the daemon/SSE and embedded paths (D1 fixed) — never a silent idle, never a provider 400, never a truncated send. 20% headroom means the next turn doesn't instantly re-compact.

**(3) No regressions — OK.**
- Unknown window (raw `ContextWindow == 0`) ⇒ immediate `return nil` (back-compat).
- **Same model / equal-or-larger window ⇒ `return nil` before any work (D2 fixed)**: same-model compaction cadence and cost are unchanged; growth stays owned by `compactIfNeeded`.
- Sessions that already fit ⇒ early return, **zero** compression calls.
- `compactIfNeeded` keeps identical behavior (only its summarize step is extracted to the shared helper; covered by `compression_completer_test.go` / `session_stats_extras_test.go`).
- Transcript invariants preserved: reuses `compression.SafeSplit` (never strands a tool_call from its results) and `ApplyCompressedTranscript` (epoch bump, system-prompt accounting, hysteresis). No message dropped outside the compression path; no panic (clean error).
- `internal/model` gains only two trivial accessors and keeps its no-`ui/tui` import seam. gofmt/build/vet/golangci-lint clean; `go test ./...` green without `-race` (Pi5); pre-existing `TestUserSessionSendMessage` 404 remains the only acceptable failure.

**(4) Holistic — OK.** The fix is server/daemon-side in gogent, at the single synchronous turn entrypoint — the correct seam. **turbotui needs no change** and owns zero context-window/compaction logic (verified). The migration error now genuinely **traverses the gogent→turbotui SSE seam** via the existing observer→hub→SSE `SessionEventError` (the D1 fix is precisely *using* that seam, which Rev-1 wrongly assumed happened automatically). No new event type, no protocol change, no new dep, stdlib-only.

---

## Files touched

| File | Change |
|---|---|
| `internal/agent/user_session.go` | **new** `MigrateToContextWindow` (emits `SessionEventError` via `s.emit`+`turnIDFrom`); call it from `ExecuteTaskLoopWithModel` (finally uses its `modelConfig` param); **new** shared `summarizeOlder` + `bumpCompaction` helpers; `compactIfNeeded` refactored onto them; two consts. |
| `internal/model/model_session.go` | **new** `LastMigrationWindow`/`SetLastMigrationWindow` (+ one mutex-guarded field). |
| `internal/config/config_test.go` | **new** `TestContextWindowJSONRoundTrip`. |
| `internal/agent/migrate_window_test.go` (**new**) | Part B unit tests. |

**No edits** to `internal/gogent` (existing call + terminal handling are reused) and **none** to `ui/tui`.

## Tests (hermetic; reuse `stubCompleter` + `makeCompressibleSession`)

- **Fits:** session under the headroom target ⇒ `MigrateToContextWindow` returns nil, **stub called 0 times**, transcript unchanged.
- **Needs compression (shrink):** `prev` large, `target` small, size over ⇒ chunked fallback runs, ends `<= 0.8·target`, transcript strictly shrank, no error.
- **Impossible:** tiny window + incompressible large recent turn ⇒ non-nil error whose message contains the **target window and the achieved (post-loop) size**; a `SessionEventError` was emitted; **no panic**; transcript not half-broken.
- **Unknown window (0):** `cfg.ContextWindow == 0` ⇒ returns nil, no compression (back-compat).
- **D2 same/larger window:** `prev > 0 && target >= prev` (incl. `target == prev`) ⇒ returns nil with **zero** compression calls even when the session is over 80% — proves same-model cadence is unchanged.
- **Bounded rounds:** non-shrinking stub digest ⇒ terminates within `maxMigrationRounds` (assert capped call count) and returns the clean error.
- **Part A:** `TestContextWindowJSONRoundTrip` (old-config default + with-field preserved).

---

## Open questions

1. **Headroom target.** 80% of the window (matches the in-loop high-water mark, 20% headroom). A 50% (low-water) target would leave a deeper cushion at the cost of more aggressive summarization. Flag if the larger cushion is preferred.
2. **`compactionCount` accounting.** Each migration round counts as one compaction in the Statistics view (reflects real work). Collapse to a single increment if the maintainer prefers migration to register as one event — trivial.
