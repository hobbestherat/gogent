# Design — Issue #486: Assisted model setup via models.dev in the Config dialog

## 0. One-paragraph summary

Add an **"Add from catalog…"** flow to *Config → Models* that fetches the public
models.dev catalog, lets the user pick a **provider** then a **model** (both
searchable), pre-fills a fully-editable review form with every `ModelConfig`
field models.dev can supply, prompts only for the credential models.dev cannot
know (API key, or Vertex project/location), and on Save creates a **new,
uniquely-named, persisted** model entry that is immediately selectable. The
catalog is fetched by a new stdlib-only `internal/modelsdev` client with a
24h-TTL on-disk cache and graceful offline fallback. The mutation reuses the
existing daemon plumbing via one new method (`AddModel`) and one new HTTP route
(`POST /api/models`). **gogent-only; no turbotui change, no `go.mod` bump.**

---

## 1. Confirmed source anchors (verified, not approximate)

| Area | Location (verified) |
|---|---|
| `ModelConfig` struct | `internal/config/config.go:18-70` |
| `GetDefaultConfig()` models.dev-seeded entries | `internal/config/config.go:1097-1201` |
| `config.SaveConfig(home, cfg)` / dir `~/.gogent` | `internal/config/config.go:989-991` (dir at `:991`) |
| `Gogent.SaveConfig` | `internal/gogent/gogent.go:3075` |
| `Gogent.Models()` (deep copies) | `internal/gogent/gogent.go:3286` |
| `Gogent.UpdateModel` (update-only; "model %q not found") | `internal/gogent/gogent.go:3301` |
| `Gogent.SetDefaultModel` / `DefaultModelName` | `internal/gogent/gogent.go:3352` / `:3376` |
| `Gogent.ScanModels` (throwaway conn from draft) | `internal/gogent/gogent.go:3386` |
| HTTP `modelsSvc` (List/Update/Scan) | `internal/server/resources.go:15-74` |
| Route table (`/models` GET/PUT, `/models/:name/scan`) | `internal/server/api.go:207-209` |
| `modelView` / `updateModelRequest` / `modelToView` | `internal/server/wire.go:342-367, 432-454` |
| `requireHuman` gate | used at `resources.go:20` etc. |
| `APIClient.ListModels/UpdateModel/ScanModels` | `ui/tui/api_client.go:599-623` |
| `ModelDTO` / `ToModelConfig` | `ui/tui/api_client.go:339-364` |
| `Handlers` struct (GetModels/UpdateModel/ScanModels/…) | `ui/tui/tui.go:113-125` |
| Remote handler wiring | `ui/tui/remote_handlers.go:769-782` |
| Embedded handler wiring | `cmd/embedded_handlers.go:162-176` |
| `showModelEditor` + Save/refresh pattern | `ui/tui/model_editor.go:49`, save at `:254-286` |
| Menu entries (`&Models…`) | `ui/tui/tui.go:1031`; palette `command_palette.go:249` |
| APIType resolver / display order / `APITypeIDs()` | `internal/model/provider.go:62, 79, 91, 100` |
| `newSelect` / `newButton` helpers | `ui/tui/theme.go:163` / `:149` |
| `Select` API (`Options`, `SetOptions`, `SetSelected`, `Value`, `OnChange`) | turbotui `turbotv/widget_select.go:19,61,72,82` |
| `TextBox` API (`SetText`/`GetText`/`OnSubmit`; **no OnChange**) | turbotui `turbotv/widget_textbox.go:9-21,50-56` |
| Docs model table | `docs/api.md:148-156` |

**Orchestration check:** branch is already based on a `main` containing #481
(`de9f7ae`), #487 (`81c732c`) and #482 (`9f9d142`). The files this issue touches
(`gogent.go`, `api_client.go`, `remote_handlers.go`) already carry those changes;
our additions are net-new methods/handlers appended alongside — no rebase needed
now, just additive edits.

---

## 2. New package: `internal/modelsdev/` (fetch + cache + pure transform)

Stdlib only (`net/http`, `encoding/json`, `os`, `path/filepath`, `time`,
`strings`, `context`). **No new module deps.**

### 2.1 Decoded types (subset of api.json we consume)
```go
type Catalog map[string]Provider          // keyed by provider id

type Provider struct {
    ID     string
    Name   string
    Env    []string            // env var names, e.g. ["OPENROUTER_API_KEY"]
    NPM    string              // hint for APIType derivation
    API    string              // /v1 base URL
    Doc    string
    Models map[string]Model    // keyed by model id
}
type Model struct {
    ID               string
    Name             string
    Reasoning        bool
    Temperature      bool      // WHETHER temperature is accepted, not a value
    Limit            Limit
    Cost             Cost
    ReasoningOptions []ReasoningOption  // json:"reasoning_options"
}
type Limit struct{ Context, Output int }
type Cost  struct{ Input, Output float64 }
type ReasoningOption struct {
    Type   string   // "effort" | "toggle"
    Values []string
}
```

### 2.2 Injectable fetch seam (network-free tests)
```go
// Fetcher abstracts the HTTP GET so tests inject a fake. revalidate carries the
// cached validators; a 304 returns (nil, "", "", ErrNotModified-equivalent ok=false).
type Fetcher interface {
    Fetch(ctx context.Context, etag, lastMod string) (data []byte, newETag, newLastMod string, notModified bool, err error)
}
type httpFetcher struct{ url string; client *http.Client }  // default → https://models.dev/api.json
```
`Client` holds `{ cachePath string; ttl time.Duration; fetcher Fetcher; now func() time.Time }`.
`now` is injected (the workflow/runtime forbids `time.Now()` in some contexts; for
prod it's `time.Now`, tests pass a fixed clock).

### 2.3 On-disk cache: `~/.gogent/modelsdev-cache.json`
```go
type cacheFile struct {
    FetchedAt    time.Time `json:"fetched_at"`
    ETag         string    `json:"etag,omitempty"`
    LastModified string    `json:"last_modified,omitempty"`
    Data         Catalog   `json:"data"`
}
```
`func (c *Client) Catalog(ctx, force bool) (Catalog, error)`:
1. Load cache file if present.
2. If `!force` and `now-FetchedAt < ttl` → return cached `Data` (no network).
3. Else `Fetch` with `If-None-Match`/`If-Modified-Since` from cache:
   - **304** → bump `FetchedAt`, rewrite cache, return cached `Data`.
   - **200** → decode, rewrite cache, return fresh `Data`.
   - **network/HTTP error** → if cache present, return cached `Data` + a non-fatal
     `staleErr` (caller shows "(offline — cached)"); if **no** cache, return the
     error so the dialog can fall back to the manual editor.
4. `force=true` ("Refresh catalog") skips the TTL short-circuit (step 2) but still
   sends validators and accepts 304.

Cache write uses the same `0750` dir / `0600` file discipline as `config.SaveConfig`.

### 2.4 Pure transforms (the unit-tested core, no network)
```go
// ProviderAPIType maps a models.dev provider to a gogent api_type token.
func ProviderAPIType(p Provider) string
// ToModelConfig builds a fully-populated draft (no APIKey).
func ToModelConfig(providerID string, p Provider, m Model) config.ModelConfig
// UniqueName sanitizes providerID+"/"+modelID and suffixes -2/-3… against `taken`.
func UniqueName(providerID, modelID string, taken map[string]bool) string
```

**`ProviderAPIType` table** (lowercased provider id / npm hint → api_type;
unknown → `"openai"`, matching `StringToAPIType`):

| models.dev provider id (examples) | api_type |
|---|---|
| `openrouter` | `openrouter` |
| `zai`, `z-ai`, `z.ai` | `zai` |
| `anthropic` | `anthropic` |
| `google-vertex`, `vertex`, `google-vertex-anthropic` | `vertex` / `vertex-anthropic` |
| everything else (`openai`, `groq`, `together`, `deepseek`, `mistral`, …) | `openai` |

Rationale: all the OpenAI-compatible gateways (Groq/Together/DeepSeek/Mistral/
Fireworks/etc.) are reached through gogent's generic `openai` adapter — the
feature only **selects** the right existing api_type; no adapter changes
(`provider.go` is read-only context per the issue). Vertex-native/`gemini` is
intentionally *not* auto-selected (models.dev's vertex entries are OpenAI-shim
oriented); the user can switch it in review if they want the native path.

**`ToModelConfig` field map** (mirrors the issue's mapping table):

| ModelConfig field | Source | Notes |
|---|---|---|
| `Name` | `UniqueName(providerID, m.ID, taken)` | unique, sanitized |
| `DisplayName` | `m.Name` | |
| `APIType` | `ProviderAPIType(p)` | |
| `Endpoint` | `p.API` | `/v1` base; for `zai`/`openrouter` may be left as provider default if blank |
| `Model` | `m.ID` | |
| `APIKey` | `""` | user enters; env hint from `p.Env` |
| `Temperature` | `0.7` | `m.Temperature` only gates *whether* accepted |
| `MaxTokens` | `m.Limit.Output` | |
| `ContextWindow` | `m.Limit.Context` | drives compaction |
| `ReasoningEffort` | first value of `reasoning_options[type=effort]` | "" if none |
| `EffortOptions` | `reasoning_options[type=effort].Values` | nil if none |
| `Thinking` | `nil` if a `type=toggle` option exists (provider default, user-togglable); else `nil` | never force on/off |
| `Free` | `m.Cost.Input==0 && m.Cost.Output==0` | |
| `Project`/`Location` | `""` | Vertex only; user supplies |

Fields models.dev has but gogent doesn't model (cost, modalities, tool_call,
structured_output, cutoff, benchmarks) are **ignored** in v1 (explicit non-goal).

---

## 3. Core: `Gogent.AddModel` + catalog accessor (`internal/gogent/gogent.go`)

```go
// AddModel appends a NEW model config and persists it. The Name must not collide
// with an existing entry (409 semantics at the HTTP layer). Mirrors UpdateModel's
// lock/SaveConfig discipline.
func (g *Gogent) AddModel(cfg config.ModelConfig) error {
    g.mu.Lock()
    for _, m := range g.config.ModelConfigs {
        if m != nil && m.Name == cfg.Name {
            g.mu.Unlock()
            return fmt.Errorf("model %q already exists", cfg.Name)
        }
    }
    c := cfg
    g.config.ModelConfigs = append(g.config.ModelConfigs, &c)
    g.mu.Unlock()
    if err := g.SaveConfig(); err != nil {
        g.warnf("Failed to persist config: %v", err)
    }
    return nil
}

// ModelCatalog returns the models.dev catalog (cached, TTL, offline fallback),
// lazily constructing a modelsdev.Client against g.homeDir. force triggers a
// revalidating refresh.
func (g *Gogent) ModelCatalog(force bool) (modelsdev.Catalog, error)
```
`AddModel` is the **authority** on uniqueness even though the dialog pre-computes a
non-colliding name with `UniqueName` (defence in depth against a concurrent add).

---

## 4. HTTP API: `POST /api/models` (`internal/server/`)

`resources.go` — new method on `modelsSvc`:
```go
// Create handles POST /models — create a NEW entry. 409 on name conflict.
func (svc modelsSvc) Create(r *http.Request, req updateModelRequest) (interface{}, error) {
    if err := requireHuman(r, svc.s.provider); err != nil { return nil, err }
    cfg := req.ModelConfig
    if err := svc.s.g.AddModel(cfg); err != nil {
        return nil, webapi.NewHTTPError(http.StatusConflict, err.Error())
    }
    return modelToView(&cfg), nil
}
```
`api.go:207` — add one route alongside the existing three:
```go
{Path: "/models", Method: http.MethodPost, Handler: mods.Create, AuthLevel: req},
```
Reuses the existing `updateModelRequest` body type (embeds `config.ModelConfig`).
Unlike `PUT`, `POST` does **not** preserve an omitted key — a new entry has no
prior key, so a blank `api_key` is stored blank (the user is expected to supply it
in the review form; an empty key simply yields an unusable-until-edited entry,
consistent with the seeded "API Key Required" defaults).

**Catalog over HTTP?** Deliberately **not** added in v1. models.dev is public and
reachable from wherever the TUI runs, so the catalog is fetched client-side in both
embedded and remote modes (§5). This keeps the new HTTP surface to exactly the one
endpoint the issue mandates. (See Open Questions for the daemon-side alternative.)

---

## 5. TUI plumbing

### 5.1 `ui/tui/api_client.go`
```go
// AddModel creates a NEW model on the daemon (POST /models). 409 → name conflict.
func (c *APIClient) AddModel(m config.ModelConfig) error {
    return c.do(http.MethodPost, "/models", m, nil)
}
```

### 5.2 `ui/tui/tui.go` — `Handlers` struct (after `ScanModels`, ~line 120)
```go
// AddModel creates a NEW model config (the catalog flow). May be nil.
AddModel func(config.ModelConfig) error
// GetModelCatalog returns the models.dev catalog (cached; force = manual refresh).
// May be nil (the "Add from catalog…" affordance is then hidden).
GetModelCatalog func(force bool) (modelsdev.Catalog, error)
```
(ui/tui already imports `internal/config`/`internal/model`; adding
`internal/modelsdev` is in-tree and dependency-free. `Catalog` is a plain
`map[string]Provider` of pure data — fine in a handler signature.)

### 5.3 `cmd/embedded_handlers.go` (after `ScanModels`, ~line 170)
```go
AddModel:        func(m config.ModelConfig) error { return g.AddModel(m) },
GetModelCatalog: func(force bool) (modelsdev.Catalog, error) { return g.ModelCatalog(force) },
```

### 5.4 `ui/tui/remote_handlers.go` (after `ScanModels`, ~line 782)
```go
AddModel: func(m config.ModelConfig) error { return c.AddModel(m) },
// Catalog is public data; the attached client fetches it directly (cache lives in
// the client host's ~/.gogent). AddModel still mutates the daemon's config via POST.
GetModelCatalog: func(force bool) (modelsdev.Catalog, error) {
    return rc.modelsDevClient().Catalog(context.Background(), force)
},
```
`rc.modelsDevClient()` lazily builds one `modelsdev.Client` against the client's
home dir (memoised on the remote-handler struct).

---

## 6. TUI dialog: `ui/tui/model_editor.go` — "Add from catalog…"

### 6.1 Entry affordance
Add a button **"Add from catalog…"** to the existing Model Settings dialog
(left of "Set Default" on the button row), shown only when
`w.handlers.AddModel != nil && w.handlers.GetModelCatalog != nil`. Clicking it
opens the wizard. Also add a Config menu item `&Add Model from Catalog…`
(`tui.go:1031` area) and a command-palette entry (`command_palette.go:249`) so it
is reachable even when no models exist (the current editor early-returns "No
models are configured." at `model_editor.go:55`, which would otherwise block a
brand-new user — the catalog flow must work from zero models).

### 6.2 Wizard = three modal layers built from existing primitives (NO new widget)

**Step 1 — Provider picker** (`showAddModelDialog`):
- Fetch catalog: `cat, err := w.handlers.GetModelCatalog(false)`.
  - error + no usable data → `showConfirm` explaining offline, offer to open the
    manual editor instead (graceful degradation; feature never blocks).
- A **filter `TextBox`** above a **`Select`** of provider rows
  `"<name> — env: <ENV_VAR>"` sorted by name. Each row shows the provider name and
  the credential env var so the user knows what to have ready.
- **Searchable:** TextBox has no `OnChange`, so we *chain* its `Component.OnTypeFn`:
  capture the original handler, install a wrapper that calls it then re-filters the
  Select via `sel.SetOptions(filtered)` and maintains a `filteredIdx→providerID`
  slice. (Pure composition of existing turbotui primitives — explicitly **not** a
  new `FilteredList` widget; see §9.)
- "Refresh catalog" button → `GetModelCatalog(true)` then rebuild the list.
- Next → Step 2 with the chosen provider id.

**Step 2 — Model picker:** same filter+Select pattern over
`cat[providerID].Models`. Each row: `"<DisplayName>  · ctx <Nk> · out <Nk> [· reasoning] [· free]"`.
Back → Step 1; Next → Step 3 with the chosen model.

**Step 3 — Review form:** a pre-filled form **reusing the exact field layout** of
`showModelEditor` (extract the field/`load`/`store` builders into a shared helper
so both editors stay in lockstep — `buildModelFields(dialog, …) → (loaders, stores)`).
Pre-filled from `modelsdev.ToModelConfig(providerID, p, m)` with
`Name = UniqueName(…, takenNames)`:
- **Name** — shown read-only (generated, unique) with an "Edit" toggle; conflict is
  surfaced live (re-suffix if the user types a colliding name).
- **API type** — read-only label "from catalog" (the existing api-type `Select`
  pre-selected, disabled).
- **Display name, Endpoint, Model id, Temperature, Max tokens, Reasoning, Thinking**
  — auto-filled, **fully editable**. Model id keeps the existing **"Scan"** button
  (reuses `handlers.ScanModels` against the draft) to swap to a live dropdown.
- **API key** — blank, **required** (validated on Save). For Vertex api_types,
  swap the API-key requirement for **Project**/**Location** (already fields in the
  editor at rows 12-13) and drop the key requirement (ADC auth).
- Save → `handlers.AddModel(cfg)`:
  - 409 / conflict error → re-suffix name, show inline error, stay on the form.
  - success → refresh exactly like `model_editor.go:272-282`: re-fetch
    `GetModels()`, `w.SetModels(ptrs)`, `w.rebuildMenu()`, close layer. Optionally
    prompt **"Set as default for new sessions?"** via `SetDefaultModel`.

The new entry is then immediately selectable in the sidebar/model dropdowns.

---

## 7. Docs
- `docs/api.md:148-156` — add the `POST /api/models` row (body `updateModelRequest`,
  `409` on name conflict, requires `human`).
- `docs/configuration.md` — document the "Add from catalog…" flow, the
  `~/.gogent/modelsdev-cache.json` cache + 24h TTL + manual Refresh, and offline
  fallback behaviour.
- `docs/providers.md` — note models.dev as the metadata source and the
  provider→api_type mapping (and that unknown providers default to `openai`).

---

## 8. Tests (all network-free; injected fetcher / fake `http.Server`)
- `internal/modelsdev`:
  - `ProviderAPIType`: openrouter/zai/anthropic/vertex variants + unknown→openai.
  - `ToModelConfig`: every mapped field incl. `Free` (cost==0), `EffortOptions`,
    effort default, toggle→Thinking, limits→MaxTokens/ContextWindow, blank APIKey.
  - `UniqueName`: sanitization + `-2/-3` suffixing against a taken set.
  - Cache: TTL short-circuit (no fetch within TTL), 304 revalidation bumps
    FetchedAt, 200 refresh replaces data, **network failure → cache fallback**,
    **no cache + failure → error**, `force` bypasses TTL. Uses an injected
    `Fetcher` and a fixed `now` clock.
- `internal/gogent`: `AddModel` appends + persists (reload config asserts it);
  rejects duplicate name.
- `internal/server`: `POST /api/models` creates an entry; **409** on conflict;
  rejects non-human like the sibling endpoints (mirror existing model-endpoint
  tests).
- `ui/tui`: `APIClient.AddModel` POSTs to `/models` with the right body (httptest).
- Regression guards: existing `UpdateModel`/`ListModels`/`ScanModels` and
  `issue389_test.go` refresh behaviour unaffected (shared `buildModelFields`
  extraction must keep `showModelEditor` byte-for-byte equivalent in behaviour).

---

## Design criteria

### (1) Goal match — it's exactly the feature asked, no scope creep
A user adds a model end-to-end in the TUI (provider→model→review→save) with no
hand-editing of `config.json`; all mappable fields auto-filled from models.dev and
editable; only the key (or Vertex project/location) is prompted; Save creates a
**new, unique, persisted** entry that's immediately selectable. Non-goals are
honoured: no auto-sync of existing entries, no pricing/benchmark UI, the manual
editor and hand-edit path are untouched. The seeded `GetDefaultConfig()` entries
remain as fallback/seed.

### (2) Usability — user drives, nothing silent
Both pickers are searchable (filter TextBox re-filtering the Select); provider rows
surface the **env var** so the user has the right credential ready; model rows
surface ctx/output/reasoning/free at a glance. The review step is **fully
editable** — auto-fill is a starting point, not a lock-in. Failures are surfaced
(offline banner, 409 conflict inline, missing-key validation), never swallowed.
Cache + manual **Refresh** keep it fast; offline degrades to cache then to the
manual editor. Works from **zero** configured models (menu/palette entry bypasses
the editor's "No models configured" early-return).

### (3) No regressions
`UpdateModel`/`Models`/`ScanModels`/`SetDefaultModel` and the GET/PUT/scan
endpoints are unchanged — `AddModel` and `POST /models` are purely additive.
`AddModel` mirrors `UpdateModel`'s lock + `SaveConfig` discipline (appends a copy,
no aliasing). The shared `buildModelFields` refactor must preserve
`showModelEditor` behaviour (issue #389 live-refresh test is the guard). New
`Handlers` fields are nil-able, so every existing `SetHandlers` caller and test
(`issue389_test.go`, `model_selector_width_test.go`, …) compiles and behaves
unchanged; the affordance simply hides when the handlers are nil. gofmt/vet/build
clean, stdlib-only, no `-race` on Pi5; pre-existing `TestUserSessionSendMessage`
404 is the only tolerated failure.

### (4) Holistic across both repos
**Right place:** catalog fetch/cache/transform is its own pure `internal/modelsdev`
package (unit-testable, injectable fetcher); the mutation reuses the existing
`Gogent`→HTTP→`APIClient`→`Handlers` model-config seam rather than inventing a
parallel one. **Cross-repo seam respected:** v1 is **gogent-only with existing
turbotui primitives** (plain `Select` + chained-`OnTypeFn` `TextBox` filter +
`Button` + `Dialog`) — **no turbotui change, no `go.mod` bump.** The decision is
explicit, not implicit: I evaluated whether the large catalog needs a turbotui
`FilteredList`/wizard primitive and concluded the filter-over-Select pattern is
adequate (hundreds of options, filtered to a handful as the user types). If during
implementation the plain Select proves unworkable for the catalog size, I will
**STOP and report** it as a follow-up turbotui-first task (separate PR to
`hobbestherat/turbotui` then a `go.mod` bump here) — I will **not** silently bump
the dep. Downstream effect on turbotui: none in v1.

---

## Open questions
1. **Catalog over HTTP for remote/attached mode?** v1 fetches the catalog
   client-side in both modes (public data; keeps the HTTP surface to the one
   mandated `POST /api/models`). The alternative is a daemon-side
   `GET /api/models/catalog` so the *daemon's* network/cache/credentials are used
   (matters if the client host is offline but the daemon isn't). **Recommend
   client-side for v1**, daemon-side as a noted follow-up. Confirm acceptable.
2. **Exact models.dev provider ids** for the zai / vertex variants (`zai` vs
   `z-ai`, `google-vertex` vs `vertex`) — the `ProviderAPIType` map will accept the
   observed aliases; I'll snapshot a small fixture from a live `api.json` pull at
   implementation time (tests feed decoded structs, not the network).
3. **Endpoint for `zai`/`openrouter`:** these api_types synthesize their base URL
   from the api_type alone (seeded entries leave `Endpoint:""`). Should the catalog
   transform leave `Endpoint` blank for them (rely on the adapter default) or fill
   `p.API`? **Lean:** blank for `zai`/`openrouter` (matches the seeded defaults and
   the adapter's intent), `p.API` for everything else. Confirm.
4. **Cache location in remote mode** writes `~/.gogent/modelsdev-cache.json` on the
   *client* host (may create `~/.gogent` there). Acceptable, or gate the cache to
   embedded mode only? **Lean:** allow it (harmless, speeds repeat opens).
