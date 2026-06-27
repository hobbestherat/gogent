# Design — Issue #532: Unroutable model configs persist silently and break sessions on restart

Branch: `pair1/validate-model-config-save-and-load-bug`. gogent-only. No new deps, no go.mod bump, no turbotui change.

## Problem (confirmed in code)

`validateRoutableConfig` (`internal/model/connection.go:798`) only runs lazily inside
`NewModelConnectionFromConfig` (`:761`), setting a deferred `conn.configErr` surfaced on the
first send. Nothing validates a `ModelConfig` at **save** (`AddModel:3313` / `UpdateModel:3423`
in `internal/gogent/gogent.go`) or at **load** (`config.LoadConfig:965` is a plain unmarshal;
gogent calls it at `gogent.go:189`). So an unroutable entry (empty `api_type` AND empty
`endpoint`, or a hosted gateway with empty `model`) can sit in `~/.gogent/config.json` forever.
On restart it loads, a new/restored session defaults to it (`defaultConnection:1966`,
`SendMessageToSessionWithModelAndEffort` default fallback `:2558`, restore-by-name `:1843`),
and the first message fails with `api_type and endpoint are both empty`.

The validation message itself is correct (#511). The bug is that the bad shape can **persist
and survive restarts silently**. Current UI forms can't create it; it's a stale pre-#522/#486
leftover. Fix = harden persist / load / use, reusing `validateRoutableConfig` — no duplicated logic.

## Approach (one shared validator, three seams)

### Shared, exported wrapper — no duplicated logic
Add to `internal/model/connection.go`, beside `validateRoutableConfig`:

```go
// ValidateModelConfig is the exported wrapper over validateRoutableConfig so callers
// outside this package (gogent save/load/use paths) can reject an unroutable config
// at the source instead of only at connection-build time. Returns a *ModelError
// (model-named, field-naming) or nil.
func ValidateModelConfig(cfg *config.ModelConfig) error { return validateRoutableConfig(cfg) }
```

`gogent` already imports `internal/model`; `config` does **not** import `model`, so calling
`model.ValidateModelConfig` from gogent introduces **no import cycle**. (This is exactly why the
load-sweep lives in gogent, not in `internal/config`.)

### GOAL 1 — validate at SAVE time (`internal/gogent/gogent.go`)
- **`AddModel` (`:3313`)**: after the existing duplicate-name check, before `append` + `SaveConfig`:
  `if err := model.ValidateModelConfig(&cfg); err != nil { unlock; return err }`. No mutation of
  `g.config.ModelConfigs`, no disk write on reject.
- **`UpdateModel` (`:3423`)**: after resolving `found != nil` (so not-found still wins), before
  `*found = updated` + `SaveConfig`: validate `&updated`; on error unlock and return without
  overwriting or saving.
- Validation is pure and `g`-independent, but is placed inside the locked region after the
  name checks (per the issue's ordering) and the lock is released before returning the error.
- The returned error is the `*model.ModelError` verbatim, e.g.
  `model "foo" is misconfigured: api_type and endpoint are both empty (cannot determine where to send requests)`.

### GOAL 1 — HTTP seam (`internal/server/resources.go`)
Today `modelsSvc.Create` (`:36`) maps any `AddModel` error to **409**, and `Update` (`:49`) maps
to **404**. Add a distinct **400** path for validation errors. The only `*model.ModelError`
`AddModel`/`UpdateModel` can return is the validation error (duplicate-name / not-found are plain
`fmt.Errorf`), so classify by type with `errors.As`:

```go
// Create
if err := svc.s.g.AddModel(cfg); err != nil {
    var me *model.ModelError
    if errors.As(err, &me) {
        return nil, webapi.NewHTTPError(http.StatusBadRequest, err.Error())
    }
    return nil, webapi.NewHTTPError(http.StatusConflict, err.Error()) // duplicate name
}
```
Same pattern in `Update`: `*model.ModelError` → 400, else → 404 (not found). `server` imports
`internal/model` (a lower layer; `server` already imports `gogent`→`model` transitively — no new
architectural boundary crossed; the `ui/tui`-must-not-import-`server`/`daemon` rule is untouched).

### GOAL 1 — TUI seam (already wired)
`ui/tui/model_editor.go:219` already does `if err := onSave(cfg); err != nil { w.showConfirm("Model", "Could not save model:\n"+err.Error(), nil) }`,
and `onSave` is `AddModel`/`UpdateModel` via the embedded handlers (`cmd/embedded_handlers.go:166/175`)
or the daemon over HTTP (remote). So the new save-time error reaches the dialog **with no TUI
change**. `model_catalog_dialog.go` save closure (`:451`) routes through the same `AddModel` handler.

### GOAL 2 — validate at LOAD time + WARN-AND-SKIP (policy (a))
In `NewGogent` right after `config.LoadConfig` (`gogent.go:189-193`), sweep `cfg.ModelConfigs`:
drop any `nil` entry and any entry where `model.ValidateModelConfig(m) != nil`; keep the survivors;
collect a human notice per dropped entry. **Do not** call `SaveConfig` — we never silently rewrite
the user's file (they edit/remove it themselves).

```go
var warnings []string
kept := cfg.ModelConfigs[:0]
for _, m := range cfg.ModelConfigs {
    if m == nil { continue }
    if verr := model.ValidateModelConfig(m); verr != nil {
        warnings = append(warnings, verr.Error())
        log.Warnf("ignoring unroutable model config: %v", verr)
        continue
    }
    kept = append(kept, m)
}
cfg.ModelConfigs = kept
```
Store on the struct: add field `configWarnings []string` to `Gogent`, assign `g.configWarnings = warnings`
after construction, and add accessor `func (g *Gogent) ConfigWarnings() []string`.

Consequences that fall out for free:
- A dropped entry is no longer in memory, so `GetModelConfig(name)` returns nil → restore-by-name
  (`:1843`) can't resolve it into a live connection, and it can't be `DefaultModel` (see Goal 4).
- If the dropped entry *was* `DefaultModel`, `defaultConnection`'s existing
  "default-not-found → first config" fallback already moves to a survivor (Goal 4 hardens this).

### GOAL 2 — user-visible, one-time notice
- New optional handler `ConfigWarnings func() []string` on `tui.Handlers` (`ui/tui/tui.go:36`),
  wired in `cmd/embedded_handlers.go` to `g.ConfigWarnings()`. Left **nil in attached/remote**
  mode (config is daemon-owned there — same precedent as `GetDefaultModel` being nil while
  attached, `cmd/attach.go:322`); nil handler ⇒ no notice, no crash.
- Surface once at startup in `(*Workbench).Run` right beside the welcome dialog (`ui/tui/tui.go:2879`):
  `if w.handlers.ConfigWarnings != nil { if ws := w.handlers.ConfigWarnings(); len(ws) > 0 { w.showConfirm("Model config", "These configured models were ignored because they cannot be routed:\n\n• "+strings.Join(ws, "\n• ")+"\n\nEdit or remove them in ~/.gogent/config.json.", nil) } }`.
  Shown after the welcome modal is queued so it stacks predictably. This is the one user-facing
  TUI addition; it's read-only and additive.

### GOAL 4 — new/restored sessions never inherit an unroutable default
Add a small reuse helper (both call sites currently duplicate the "find default by name, else
first" logic):

```go
// routableDefaultConfig returns the configured default model when it exists and is
// routable, else the first routable configured model, else nil. Operates on a passed
// config snapshot so callers control locking.
func routableDefaultConfig(cfg *config.Config) *config.ModelConfig {
    if cfg == nil { return nil }
    var def, first *config.ModelConfig
    for _, m := range cfg.ModelConfigs {
        if m == nil || model.ValidateModelConfig(m) != nil { continue }
        if first == nil { first = m }
        if m.Name == cfg.DefaultModel { def = m }
    }
    if def != nil { return def }
    return first
}
```
- `defaultConnection` (`:1966`): `if def := routableDefaultConfig(g.config); def != nil { return g.buildConnection(def) }; return model.NewModelConnection()`.
- `SendMessageToSessionWithModelAndEffort` default fallback (`:2558-2565`): replace the
  inline default-by-name loop with `selectedConfig = routableDefaultConfig(cfg)` (using the
  `cfg` snapshot already taken under `RLock` at `:2523`). An explicit `modelName` that matched a
  config is left as-is; only the *default fallback* gets the routable guard.

Belt-and-suspenders with Goal 2: even if an unroutable entry somehow remained in memory, the
default resolution skips it to a routable one; if none is routable, `defaultConnection` returns
the placeholder connection and the send fails with the **clear** deferred `configErr` (existing
behavior) — never a silent doomed send.

## Files touched
gogent:
- `internal/model/connection.go` — add `ValidateModelConfig` wrapper (1 fn).
- `internal/gogent/gogent.go` — `AddModel`, `UpdateModel` (save-time reject); load sweep at `:189`;
  `configWarnings` field + `ConfigWarnings()` accessor; `routableDefaultConfig` helper;
  `defaultConnection` + `SendMessage…` default fallback rewired.
- `internal/server/resources.go` — `Create`/`Update` add 400 path via `errors.As(*model.ModelError)`; add `internal/model` import.
- `ui/tui/tui.go` — `Handlers.ConfigWarnings` field; one-time startup `showConfirm` in `Run`.
- `cmd/embedded_handlers.go` — wire `ConfigWarnings` to `g.ConfigWarnings()`.
- Tests (see Goal 3).

turbotui: **none.** The optional disabled-option enhancement is an explicit out-of-scope follow-up.

## Tests (GOAL 3 — mirror `internal/model/routable_config_validation_test.go`)
- `internal/model`: `ValidateModelConfig` returns the same verdicts as the existing table
  (reuse the cases); confirms no logic drift. Existing `routable_config_validation_test.go` untouched.
- `internal/gogent` (new `validate_model_config_save_test.go`): `AddModel`/`UpdateModel` with
  empty-api_type+empty-endpoint, and openrouter+empty-model → return error; `g.config.ModelConfigs`
  unchanged; `~/.gogent/config.json` not created/modified (temp home; assert absence or unchanged bytes).
  Valid add/update still persists.
- `internal/gogent` (new `load_sweep_test.go`): seed `config.json` with one bad + one good entry +
  `default_model` = bad name → `NewGogent` keeps only good, `ConfigWarnings()` non-empty,
  `defaultConnection()`/`routableDefaultConfig` resolve to the good entry, no panic.
- `internal/server` (extend models tests): `POST /models` with `{name, api_key, rest empty}` → **400**,
  not persisted; duplicate name still **409**; `PUT` validation → **400**, not-found still 404.
- `internal/gogent`: restored/new session whose stored/default model is unroutable → resolves to a
  routable fallback (no connection whose `configErr` is set when a routable model exists).

## Design criteria
1. **Goal match** — fixes exactly the persist/load/use gap: unroutable config can't be saved
   (`AddModel`/`UpdateModel` + HTTP 400), is skipped at load with a notice, and can never be the
   silent default. `validateRoutableConfig` is reused via one exported wrapper — no new validation
   logic, no scope creep (no turbotui, no schema change, no file rewrite).
2. **Usability** — clear model-named errors land in the TUI editor's existing `showConfirm` and as
   HTTP 400; a one-time startup notice tells the user which entries were ignored and where to fix
   them; valid flows are byte-for-byte unaffected (the validator returns nil for every previously
   valid config, including local `openai` servers with an explicit endpoint and empty model).
3. **No regressions** — duplicate-name (409) and not-found (404) HTTP paths preserved by ordering
   the `errors.As` branch first; `defaultConnection`'s default-by-name precedence preserved when all
   models are routable; load sweep never calls `SaveConfig` (no surprise disk rewrite); sessions
   pinned to a now-dropped model fall back via existing restore logic. No `ui/tui`→`daemon`/`server`
   import added.
4. **Holistic / cross-repo** — load sweep and helpers sit in gogent to dodge a `config`→`model`
   cycle; the validator lives once in `internal/model` (its home) and is reused everywhere; the
   gogent↔turbotui seam is untouched (TUI consumes turbotui widgets only; the notice uses the
   existing `showConfirm`). The optional turbotui disabled-option UI is deliberately deferred.

## Regression risks
- **HTTP status reclassification**: a validation error that used to surface as 409 now surfaces as
  400. Intended; covered by the server test and the `errors.As`-first ordering keeps duplicates 409.
- **Load sweep mutating in-memory config away from disk**: deliberate (no rewrite). Risk that code
  assumes memory==disk for models — none found; saves always serialize the full in-memory slice, so
  the next legitimate `SaveConfig` would drop the bad entry from disk too (acceptable, and only on an
  explicit user-initiated save).
- **`routableDefaultConfig` lock discipline**: takes a `*config.Config` snapshot so it never touches
  `g.mu` itself; `SendMessage…` passes its already-RLocked snapshot, `defaultConnection` passes
  `g.config` exactly as the current code reads it (no new lock interleaving).
- **Over-rejection**: validator is unchanged, so `openai`+explicit-endpoint+empty-model and all
  base-URL-deriving providers still pass — no valid config becomes unsavable.

## Open questions
- **Notice in attached/remote mode**: left silent (handler nil) to avoid daemon-notice plumbing,
  matching the `GetDefaultModel`-nil-while-attached precedent. The daemon still logs + sweeps its own
  config at its startup, so the bad entry is handled server-side; only the *client popup* is skipped.
  Acceptable for this fix? (Surfacing it remotely would need a daemon endpoint — a follow-up.)
- **Notice placement**: startup `showConfirm` chosen over a Models-dialog badge for minimal UI
  change. If a persistent in-dialog indicator is preferred, that's a small additive follow-up.
