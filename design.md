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

Also add an exported constructor for the **no-routable-model fail-safe** (see Goal 4 / Defect 1.1).
`NewModelConnection()` (`connection.go:718`) deliberately leaves `configErr == nil` and points at
the `DefaultModelURL` localhost placeholder (locked by `TestRoutableValidation_BareNewModelConnectionUntouched`),
so gogent must **not** fall back to it — that would re-introduce the silent localhost-404 that
#505/#511 eliminated. Instead:

```go
// NewUnroutableConnection returns a connection that carries a deferred configErr and
// therefore fails every completion/scan call with a clear message, instead of silently
// dialing the DefaultModelURL placeholder. gogent uses it when no routable model is
// configured, preserving the #505/#511 "no silent localhost 404" guarantee even when
// every configured entry was swept as unroutable.
func NewUnroutableConnection(message string) *ModelConnection {
    conn := NewModelConnection()
    conn.configErr = &ModelError{Type: ErrorGeneric, Message: message}
    return conn
}
```
This keeps the private `configErr` field encapsulated in `internal/model` (gogent cannot set it
directly) while giving gogent a fail-safe connection.

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

> **Implementation note (supersedes the `errors.As` sketch below).** wrapcheck (enabled
> repo-wide) requires errors crossing the gogent→caller boundary to be wrapped, and the
> codebase already classifies model errors via `errors.Is` on gogent sentinels (the Delete
> handler). So `AddModel`/`UpdateModel` wrap the validation detail with a new sentinel
> `gogent.ErrModelInvalid` (`fmt.Errorf("%w: %w", ErrModelInvalid, verr)`), and the server
> classifies with `errors.Is(err, gogent.ErrModelInvalid)` → 400. This keeps `internal/model`
> out of `internal/server`'s imports (cleaner boundary) and is consistent with
> `ErrModelNotFound`/`ErrModelInUse`/`ErrModelIsDefault`.
>
> **Also:** `modelsSvc.Update`'s signature was reordered to `(r, req, name)` — webapi only
> binds the JSON body to the index-1 parameter, so the prior `(r, name, req)` silently
> ignored the body (binding `req` from the query string). The reorder matches the existing
> `watchersSvc.Update`/`commandsSvc.Update` convention and is required for PUT validation to
> operate on the real submitted config (otherwise every PUT would be rejected as unroutable).

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
kept := make([]*config.ModelConfig, 0, len(cfg.ModelConfigs)) // fresh slice, no [:0] tail-aliasing (Defect 3.2)
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
- Surface at startup in `(*Workbench).Run` **after** the welcome-dialog block (`ui/tui/tui.go:2879-2881`),
  so the config-warnings modal is the topmost layer and is dismissed *first* (a config error
  outranks onboarding), with the welcome dialog behind it — deterministic stacking, not a noisy race:
  `if w.handlers.ConfigWarnings != nil { if ws := w.handlers.ConfigWarnings(); len(ws) > 0 { w.showConfirm("Model config", "These configured models were ignored because they cannot be routed:\n\n• "+strings.Join(ws, "\n• ")+"\n\nEdit or remove them in ~/.gogent/config.json.", nil) } }`.
  This is the one user-facing TUI addition; it's read-only and additive.
- **Recurrence is intentional, not "one-time".** Because the sweep deliberately never rewrites the
  file, a bad entry persists and the notice re-fires on **every** launch until the user fixes/removes
  it. This is a deliberate decision (a persistent reminder is safer than a silently-rewritten config
  and beats a one-shot toast the user may miss); the alternative — auto-rewriting the file on first
  load — is explicitly rejected (we never silently mutate the user's config). Documented as a choice.

### GOAL 4 — new/restored sessions never inherit an unroutable default
Add a small reuse helper (three call sites — `defaultConnection`, the `SendMessage…` fallback, and
the model-listing path — currently duplicate "find default by name, else first"):

```go
// routableDefaultConfig returns the configured default model when it exists and is
// routable, else the first routable configured model, else nil. Skips nil/unroutable
// entries. Operates on a passed config snapshot so callers control locking.
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

**`defaultConnection` (`:1966`)** — builds a connection for NEW sessions, so it must always yield a
routable connection or a *clear-error* one (never the silent localhost placeholder):
```go
if def := routableDefaultConfig(g.config); def != nil {
    return g.buildConnection(def)
}
return model.NewUnroutableConnection(
    "no routable model is configured — add a model with an api_type or endpoint in the Models… dialog (or fix ~/.gogent/config.json)")
```
**This is the fix for Defect 1.1.** The previous `model.NewModelConnection()` fall-through has
`configErr == nil` and dials `DefaultModelURL` (localhost) → opaque connection-refused/404. With
`NewUnroutableConnection` the all-unroutable case fails with a **clear, actionable** error on first
send, satisfying the acceptance line *"fails safe (routable fallback OR clear error), no silent
doomed send."*

**`SendMessageToSessionWithModelAndEffort` default fallback (`:2558-2565`)** — operates on an
*existing* session that already has a connection. Behavior contract: an empty/unmatched
`DefaultModel` leaves `selectedConfig == nil`, and the downstream `if selectedConfig != nil` guard
(`:2580`) then **keeps the session's current connection** — that must be preserved. So we keep the
existing default-by-name loop and add a *narrow* routability guard that only fires when the resolved
default is actually unroutable (defense-in-depth; after the load sweep an unroutable default is
already gone from memory):
```go
if selectedConfig == nil {
    for _, m := range cfg.ModelConfigs {
        if m != nil && m.Name == cfg.DefaultModel { selectedConfig = m; break }
    }
    // Issue #532: never route a turn through an unroutable default. If the resolved
    // default is unroutable, drop to the first routable entry. If the default was
    // empty/unmatched, selectedConfig stays nil and the session keeps its existing
    // connection (unchanged behavior — see :2580).
    if selectedConfig != nil && model.ValidateModelConfig(selectedConfig) != nil {
        selectedConfig = routableDefaultConfig(cfg) // skips the unroutable default → first routable (or nil)
    }
}
```
This is **zero behavior change in every routable case** and **does not touch the nil-keeps-current
path** (resolves Defect 3.1): it only redirects when a matched default is itself unroutable, which
the sweep already prevents — so it is pure belt-and-suspenders, and is pinned by a test (Goal 3).

**Model-listing path (`:2299-2304`)** — currently independently resolves
`selected = cfg.ModelConfigs[0]` for the fallback, then errors "no model backend configured" when
empty. Route its fallback through the same helper for uniform, clear failure (Defect 1.2):
```go
if selected == nil { selected = routableDefaultConfig(cfg) }
if selected == nil { return nil, fmt.Errorf("no routable model is configured") }
```
(The explicit-name match above it is unchanged.) This makes the all-unroutable case fail clearly
here too, consistent with `defaultConnection`.

## Files touched
gogent:
- `internal/model/connection.go` — add `ValidateModelConfig` wrapper + `NewUnroutableConnection` fail-safe (2 fns).
- `internal/gogent/gogent.go` — `AddModel`, `UpdateModel` (save-time reject); load sweep at `:189`;
  `configWarnings` field + `ConfigWarnings()` accessor; `routableDefaultConfig` helper;
  `defaultConnection` (`:1966`) + `SendMessage…` default fallback (`:2558`) + model-listing fallback
  (`:2299`) routed through the helper.
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
- `internal/gogent` **all-unroutable fail-safe (Defect 1.1)**: seed `config.json` where *every*
  entry is unroutable + a bad `default_model` → after sweep `ModelConfigs` is empty;
  `defaultConnection()` returns a connection whose first completion/scan yields the **clear**
  "no routable model is configured" error (assert via the `configErr` path, the
  `routable_config_validation_test.go` `configErrOf` precedent) and **never** dials `DefaultModelURL`.
- `internal/gogent` **routable-default helper / narrow fallback (Defect 3.1)**: table test for
  `routableDefaultConfig` (default-routable→default; default-unroutable→first-routable;
  none-routable→nil); plus a `SendMessage`-fallback test asserting (a) an unmatched/empty
  `DefaultModel` leaves the session's existing connection untouched (selectedConfig stays nil),
  and (b) a *matched-but-unroutable* default (injected directly into `g.config`, bypassing the
  sweep) redirects to the first routable entry. This pins the intentional narrowed semantics.
- `internal/server` (extend models tests): `POST /models` with `{name, api_key, rest empty}` → **400**,
  not persisted; duplicate name still **409**; `PUT` validation → **400**, not-found still 404.

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
   models are routable; the `SendMessage` fallback's nil-keeps-current-connection path is preserved
   exactly (the routability guard is narrow and only fires on a matched-but-unroutable default); the
   all-unroutable case now fails with a **clear** error instead of the silent localhost-404 it would
   otherwise have produced (Defect 1.1); load sweep never calls `SaveConfig` (no surprise disk
   rewrite); sessions pinned to a now-dropped model fall back via existing restore logic. No
   `ui/tui`→`daemon`/`server` import added.
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
- **All-unroutable fail-safe (Defect 1.1)**: the *only* path that previously could fall to a bare
  `NewModelConnection()` (silent localhost) was the empty/all-bad config; `NewUnroutableConnection`
  replaces it with a clear deferred error. The common all-routable case is byte-for-byte unchanged
  (still `g.buildConnection(def)`).
- **`SendMessage` fallback semantics (Defect 3.1)**: the narrowed guard means an unmatched/empty
  `DefaultModel` still yields `selectedConfig == nil` → session keeps its current connection,
  identical to today; the redirect only triggers for a matched-but-unroutable default (which the
  load sweep already prevents from existing in memory). Pinned by test.

## Resolved decisions (previously open)
- **No-routable-model fail-safe**: returns a `NewUnroutableConnection` with a clear deferred
  `configErr`, never the bare localhost placeholder (Defect 1.1 fixed).
- **`SendMessage` default-fallback scope**: narrowed so it preserves the nil-keeps-current-connection
  behavior and only redirects a matched-but-unroutable default (Defect 3.1 fixed + tested).
- **Notice recurrence**: intentionally re-fires every launch until the user fixes the file; we never
  auto-rewrite the config. Modal stacks above (and is dismissed before) the welcome dialog.

## Open questions
- **Notice in attached/remote mode**: left silent (handler nil) to avoid daemon-notice plumbing,
  matching the `GetDefaultModel`-nil-while-attached precedent. The daemon still logs + sweeps its own
  config at its startup, so the bad entry is handled server-side; only the *client popup* is skipped.
  Acceptable for this fix? (Surfacing it remotely would need a daemon endpoint — a follow-up.)
- **Notice placement**: startup `showConfirm` chosen over a Models-dialog badge for minimal UI
  change. If a persistent in-dialog indicator is preferred, that's a small additive follow-up.
