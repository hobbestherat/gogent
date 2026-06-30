# Design — Issue #589 "Context window issues"

Branch: `pair1/context-window-as-model-setting-and-safe`
Scope: gogent-only, stdlib-only, no new deps. Lane = `internal/model` + `internal/agent` + the wiring point in `internal/gogent`. **No `ui/tui` changes.**

---

## 0. Investigation result — what already exists (important)

I read the code before designing. **Part A (context window as a model setting) is already implemented in mainline** and must not be re-built:

- `internal/config/config.go:47` — `ContextWindow int \`json:"context_window,omitempty"\`` field on `ModelConfig`, documented as the input-window budget, deliberately distinct from `MaxTokens` (the output cap).
- `internal/config/config.go:138` — `const defaultContextWindow = 32768` (conservative).
- `internal/config/config.go:143` — `(*ModelConfig).ContextWindowOrDefault()` returns the configured value or the default when unset (`<=0`), nil-safe.
- `omitempty` tag + standard `json.Unmarshal` in `LoadConfig`/`SaveConfig` (`config.go:1041`/`:1065`) already give backward-compatible round-trip: an old config with no `context_window` decodes to `0` and `ContextWindowOrDefault()` supplies the default.
- Shipped default models all set an explicit `ContextWindow` (config.go:1147…1278).
- Already wired into the model-switch path: `internal/gogent/gogent.go:2685` calls `ag.ThoughtTrain.SetMaxContextLength(selectedConfig.ContextWindowOrDefault())`, and `internal/model/model_session.go` calibrates compaction water-marks (80% high / 50% low) against `MaxContextLength` (`NeedsCompression`, `model_session.go:331`).
- Tests already cover the accessor and defaults: `internal/config/config_test.go` `TestContextWindowOrDefault`, `TestDefaultConfigSetsContextWindow`, `TestContextWindowDistinctFromMaxTokens`.

**Consequence:** Part A needs only one small *additive* test (a JSON save→load round-trip / old-config back-compat assertion that is not yet covered). **The real engineering work is Part B** — the safe migration + bounded chunked-compression fallback, which does **not** exist today.

### What's missing today (the actual bug)

At `gogent.go:2669-2686` every user message re-points the session at the selected model:

```go
newModel := g.buildConnection(selectedConfig)
... ag.ThoughtTrain.Resume(newModel) ...
ag.ThoughtTrain.SetMaxContextLength(selectedConfig.ContextWindowOrDefault())
```

`Resume` (`model_session.go:394`) recomputes `CurrentTokenCount = lastUsageTotal(History)` — i.e. the real size of the whole conversation as last measured. **There is no guard**: if you switch to a model whose context window is *smaller* than the current conversation, the next send goes out over-budget and fails at the provider (or silently mis-behaves). The existing in-loop `compactIfNeeded` (`user_session.go:1730`) only fires *inside* `runLoop` at the 80% high-water mark and does a **single** `SafeSplit`+`Summarize` pass — it cannot rescue a transcript whose verbatim recent turns alone already exceed a much smaller target window. This is exactly the maintainer's "sessions cannot safely migrate to a smaller context window … maybe a multi-step chunked compression fallback."

---

## Part A — finish & lock the model setting (minimal)

Field, default, accessor, persistence, and wiring already exist (see §0). **Code change: none.** **Test change (additive):** add `TestContextWindowJSONRoundTrip` in `internal/config/config_test.go`:

1. Decode a hand-written *old* config JSON blob that omits `context_window` → assert `ModelConfig.ContextWindow == 0` and `ContextWindowOrDefault() == defaultContextWindow` (back-compat).
2. `SaveConfig`→`LoadConfig` a config whose model has `ContextWindow: 200000` → assert the value survives the round-trip unchanged.

This satisfies the issue's explicit "old config loads with default; with-field preserved across save+load" requirement without touching shipping code.

---

## Part B — safe migration + bounded chunked-compression fallback (the work)

### B.1 New method (testable, reuses existing compressor)

Add to `internal/agent/user_session.go` (next to `compactIfNeeded`):

```go
// migration tuning (file-local consts)
const (
    maxMigrationRounds      = 8   // hard cap on compression rounds (bounded, no infinite loop)
    migrationTargetFraction = 0.8 // land at/under the compaction high-water mark → 20% headroom for the next turn
)

// MigrateToContextWindow compresses the live transcript, in bounded chunked
// rounds, until it fits under the TARGET model's context window before the first
// send on that model. Returns a clear error if it cannot fit even after maximal
// compression. A no-op (returns nil) when the target window is unknown (raw
// ContextWindow <= 0) so existing behavior is preserved.
func (s *UserSession) MigrateToContextWindow(sess *model.ModelSession, cfg *config.ModelConfig) error
```

`internal/agent` already imports both `gogent/internal/compression` and `gogent/internal/config` (user_session.go:12-13), so no new import edges and no new package dependency.

**Algorithm (deterministic, bounded):**

1. `if sess == nil || cfg == nil || cfg.ContextWindow <= 0 { return nil }`
   — *raw* `cfg.ContextWindow`, **not** `ContextWindowOrDefault()`. Unknown window (0) ⇒ no guard, back-compat preserved (acceptance requirement).
2. `target := int(float64(cfg.ContextWindow) * migrationTargetFraction)`.
   When `context_window` is set, `ContextWindowOrDefault() == ContextWindow`, so `target` is exactly the in-loop compaction high-water mark — the migrated session lands just below the trigger and won't immediately re-compact.
3. Measure with `sess.GetCurrentTokenCount()` (set by `Resume` to the real last-turn total; after each compression it returns the fresh `EstimateTokens`+system-prompt estimate — consistent before/after).
   `if size <= target { return nil }` — **fits ⇒ no compression invoked** (migration-unchanged path).
4. Loop, `keep := compression.DefaultKeepRecentTurns` (3):
   - `for round := 0; round < maxMigrationRounds; round++`:
     - `if sess.GetCurrentTokenCount() <= target { return nil }`
     - `prev := sess.GetCurrentTokenCount()`
     - `older, recent := compression.SafeSplit(sess.GetTranscript(), keep)`
     - `if len(older) == 0 { if keep <= 1 { break }; keep--; continue }` — fold more of the tail when nothing older remains at this depth.
     - `digest, err := compressOlder(sess, older)` (shared helper, see B.2). On `err`/empty digest → return a clear wrapped error (cannot compress).
     - `sess.ApplyCompressedTranscript(append([]model.Message{digestMsg(digest)}, recent...))`; bump `compactionCount`; `s.emit(SessionEventCompaction{...})` so the UI/stats see each round.
     - **No-progress guard:** `if sess.GetCurrentTokenCount() >= prev { if keep <= 1 { break }; keep-- }` else `if keep > 1 { keep-- }` — guarantees termination even if a digest doesn't shrink the tail.
5. Final check: `if sess.GetCurrentTokenCount() <= target { return nil }`.
6. Otherwise **fail cleanly** (never panic, never truncate):
   ```
   fmt.Errorf("cannot fit this conversation into model %q: its context window is %d tokens "+
       "but the conversation is still ~%d tokens after maximal compression (%d rounds). "+
       "Start a new session or switch to a model with a larger context window.",
       cfg.DisplayName/Name, cfg.ContextWindow, size, rounds)
   ```
   Message names the target model, its window, and the achieved size (acceptance requirement).

**Termination/determinism:** `keep` is monotonically non-increasing and floored at 1; the loop is hard-capped at `maxMigrationRounds`; the no-progress guard prevents re-summarizing a digest forever. With a stub completer the path is fully deterministic and hermetic.

### B.2 Small shared helper (DRY with `compactIfNeeded`)

Extract the "pick the compression completer (fast model else session backend) → `compression.NewCompressionAgent(...).Summarize(older)`" step that `compactIfNeeded` already does (user_session.go:1750-1762) into:

```go
func (s *UserSession) compressOlder(sess *model.ModelSession, older []model.Message) (string, error)
```

`compactIfNeeded` is refactored to call it (behavior identical), and `MigrateToContextWindow` reuses it. This keeps a single compression seam and avoids reinventing the compressor (issue's "REUSE it" directive).

### B.3 Wiring (the only `internal/gogent` edit)

In `gogent.go`, immediately after `SetMaxContextLength` (line 2685), inside `if selectedConfig != nil {`:

```go
ag.ThoughtTrain.SetMaxContextLength(selectedConfig.ContextWindowOrDefault())
if err := userSession.MigrateToContextWindow(ag.ThoughtTrain, selectedConfig); err != nil {
    return nil, err   // abort the turn cleanly before any over-budget send
}
```

`ProcessMessage`'s enclosing entrypoints already return `(*model.CompletionResponse, error)` (`SendMessageToSessionWithModelAndEffort` etc.), so returning the error surfaces it through the normal error channel to the TUI/HTTP/SSE layer with no new plumbing. The compression rounds emit the existing `SessionEventCompaction`.

`selectedConfig` is the per-session shallow copy when an effort override is applied (gogent.go:2661) — it still carries the original `ContextWindow`/`Name`, so the guard reads the right window.

---

## The 4 design gates

**(1) Goal match.** Exactly the issue's two asks: (A) context window is a real, persisted, per-model setting with a safe default and back-compat — already in place, locked with an added round-trip test; (B) migrating an over-budget session to a smaller-window model triggers a **bounded multi-step chunked** compression that fits-or-fails-cleanly and **never panics**. No scope creep: no new tokenizer, no TUI editor (explicitly deferred to a follow-up), no change to unrelated model behavior.

**(2) Usability.** The user drives the input by choosing the model (existing model selector); the system reacts. When the session fits, migration is invisible (no needless compression). When it must compress, each round emits `SessionEventCompaction` (the user already sees compaction feedback). When it genuinely cannot fit, the user gets one clear, actionable sentence naming the model, its window, and the achieved size — never a silent provider 400 or a broken truncated send. Headroom (20%) means the very next turn doesn't instantly re-compact.

**(3) No regressions.**
- Unknown window (raw `ContextWindow == 0`) ⇒ `MigrateToContextWindow` returns `nil` immediately ⇒ identical to today. (`ContextWindowOrDefault`'s 32768 still drives in-loop compaction as before — unchanged.)
- Sessions that already fit ⇒ early return, **zero** compression calls ⇒ no behavioral change, no extra latency, no extra LLM cost.
- `compactIfNeeded` keeps identical behavior (only its summarize step is extracted to a shared helper; covered by existing `compression_completer_test.go` / `session_stats_extras_test.go`).
- Transcript invariants preserved: reuses `compression.SafeSplit` (never strands a tool_call from its results) and `ApplyCompressedTranscript` (epoch bump, system-prompt accounting, hysteresis) — the same primitives the live path already trusts. No message dropped outside the compression path.
- gofmt/build/vet/golangci-lint clean; `go test ./...` green without `-race` (Pi5). Pre-existing `TestUserSessionSendMessage` 404 remains the only acceptable failure; `internal/model` keeps its no-`ui/tui` import seam (no new imports there at all).

**(4) Holistic across both repos.** The fix is server/daemon-side in gogent, at the single point where the active model is (re)bound to the session — the correct seam. **turbotui needs no change:** the migration error flows back through the existing `(*CompletionResponse, error)` return and the compaction feedback through the existing `SessionEventCompaction` over SSE — no new event type, no protocol change, no client-side context-window logic to keep in sync. I confirmed the context-window/compression concern lives entirely in gogent; turbotui is a pure terminal client and owns none of it. Seam respected, no new dep, stdlib-only.

---

## Files touched

| File | Change |
|---|---|
| `internal/agent/user_session.go` | **new** `MigrateToContextWindow`; **new** `compressOlder` helper; `compactIfNeeded` refactored to call it; two file-local consts. |
| `internal/gogent/gogent.go` (~line 2685) | call `MigrateToContextWindow` after `SetMaxContextLength`; return its error to abort the turn cleanly. |
| `internal/config/config_test.go` | **new** `TestContextWindowJSONRoundTrip` (old-config back-compat + with-field save/load round-trip). |
| `internal/agent/migrate_window_test.go` (**new**) | Part B unit tests (see below). |

No code edits to `internal/model` (its existing `Resume`/`SetMaxContextLength`/`EstimateTokens`/water-marks are reused as-is) and **none** to `ui/tui`.

## Tests (hermetic; reuse existing `stubCompleter` + `makeCompressibleSession`)

- **Fits:** session under target window ⇒ `MigrateToContextWindow` returns nil, **stub completer called 0 times** (no compression), transcript unchanged.
- **Needs compression:** session over target ⇒ chunked fallback runs, ends `<= target`, transcript strictly shrank, fits with headroom.
- **Impossible:** tiny window + an incompressible large recent turn ⇒ non-nil error whose message contains the target window; **no panic**; transcript not left in a half-broken state.
- **Unknown window (0):** `cfg.ContextWindow == 0` ⇒ returns nil, no compression, migration proceeds (back-compat).
- **Bounded rounds:** a pathological non-shrinking stub digest ⇒ terminates within `maxMigrationRounds` (assert call count capped) and returns the clean error.
- **Part A round-trip:** `TestContextWindowJSONRoundTrip` (old-config default + with-field preserved).

---

## Open questions

1. **Headroom target.** I chose 80% of the window (matches the in-loop high-water mark, 20% headroom). Alternative: target the 50% low-water mark for a deeper post-migration cushion at the cost of more aggressive summarization. 80% preserves the most content while still clearing the trigger; flag if a larger cushion is preferred.
2. **`compactionCount` accounting.** I count each migration round as a compaction in the stats view (reflects real work). If the maintainer prefers migration to register as a single compaction event, collapse to one increment — trivial.
3. **Same-model "migration".** The guard runs on every user message (the block is per-turn), but it's an idempotent early-return when the session already fits, so same-model turns are unaffected. If a stricter "only on actual backend change" gate is wanted, we can compare against the previous model name — not needed for correctness, only to avoid a cheap `GetCurrentTokenCount()` comparison per turn.
