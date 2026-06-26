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
`now` defaults to `time.Now` in production (`time.Now()` is perfectly fine in Go
code); it is injected **only** so cache-TTL tests are deterministic with a fixed
clock. Constructor: `func NewClient(homeDir string) *Client` (cachePath =
`<homeDir>/.gogent/modelsdev-cache.json`, ttl = 24h, default httpFetcher, now =
`time.Now`) — built directly in the handler wiring (§5.3/5.4), so the fetch-cache
concern stays out of core.

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
| `Endpoint` | **resolver-aware** — see below | NOT blindly `p.API` |
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

**`Thinking` is always `nil` in v1** (provider default). The table's nil/nil
branches collapse to a single rule: we never *force* the toggle on or off from the
catalog. When the model advertises a `type=toggle` reasoning option, the review
form's Thinking selector is left enabled so the **user** can flip it; we just don't
pre-commit a value. (Forcing it on would also mark the model as a reasoning model
via `IsReasoningModel`, changing request encoding — not something to infer
silently.)

**Endpoint is resolver-aware, not blindly `p.API`** — this is the one place
models.dev's data shape collides with gogent's adapter conventions:

- gogent's **OpenAI family** keeps `/v1` in the *base* and `chatPath =
  "/chat/completions"` (`provider_openai.go:21,38,47`), so the resolved URL is
  `p.API + /chat/completions` — feeding `p.API` (a `/v1` base) is **correct**.
- gogent's **Anthropic** adapter does the opposite: base
  `https://api.anthropic.com` with `chatPath = "/v1/messages"`
  (`provider_anthropic.go:16`) — the `/v1` lives in the chatPath.
  `stripChatPath` (`provider.go:404-414`) only trims a *trailing full chatPath*, so
  a `p.API` of `https://api.anthropic.com/v1` is **not** reduced, and `appendPath`
  yields the broken `https://api.anthropic.com/v1/v1/messages` → 404.
- **zai / openrouter / vertex\*** synthesize their base from the api_type alone
  (the seeded entries all leave `Endpoint:""`, `config.go:1130,1146,1190`).

So the transform classifies api_types into two buckets:

```go
// deriveBaseAPITypes leave Endpoint BLANK → adapter uses its own default base
// (their version/base is implied by the adapter, not models.dev's p.API).
var deriveBaseAPITypes = map[string]bool{
    "anthropic": true, "zai": true, "openrouter": true,
    "vertex": true, "vertex-native": true, "vertex-anthropic": true,
}
// Endpoint rule:
//   if deriveBaseAPITypes[apiType] { Endpoint = "" }   // adapter default base
//   else                          { Endpoint = p.API } // generic openai: /v1 base required
```

The generic `openai` adapter's default base is the useless
`http://localhost:8080/v1` (`provider_openai.go:21`), so a real OpenAI-compatible
gateway (Groq/Together/DeepSeek/…) **must** carry `p.API`. The derive-base set
must NOT, because their adapters already know (or version-embed) the base.
A unit test asserts the resolved Anthropic chat URL is `…/v1/messages` (single
`/v1`), and that an `openai`-mapped provider keeps `p.API`.

Fields models.dev has but gogent doesn't model (cost, modalities, tool_call,
structured_output, cutoff, benchmarks) are **ignored** in v1 (explicit non-goal).

---

## 3. Core: `Gogent.AddModel` + `HomeDir()` accessor (`internal/gogent/gogent.go`)

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

// HomeDir exposes the home dir so embedded handler wiring can construct a
// modelsdev.Client (cache → <home>/.gogent/modelsdev-cache.json). Trivial accessor.
func (g *Gogent) HomeDir() string { return g.homeDir }
```
`AddModel` is the **authority** on uniqueness even though the dialog pre-computes a
non-colliding name with `UniqueName` (defence in depth against a concurrent add).

The catalog fetch/cache concern deliberately does **not** live on `*Gogent`: the
`modelsdev.Client` is built in the handler wiring (§5.3/5.4), keeping core free of
a network/caching dependency. Embedded mode passes `g.HomeDir()`; remote mode uses
the client host's home.

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
mdc := modelsdev.NewClient(g.HomeDir())   // cache → <home>/.gogent/modelsdev-cache.json
...
AddModel:        func(m config.ModelConfig) error { return g.AddModel(m) },
GetModelCatalog: func(force bool) (modelsdev.Catalog, error) { return mdc.Catalog(context.Background(), force) },
```

### 5.4 `ui/tui/remote_handlers.go` (after `ScanModels`, ~line 782)
```go
home, _ := os.UserHomeDir()
mdc := modelsdev.NewClient(home)   // public data; cache lives on the client host
...
AddModel: func(m config.ModelConfig) error { return c.AddModel(m) },
// Catalog is public data; the attached client fetches it directly. AddModel still
// mutates the DAEMON's config via POST /models, so the new entry lands server-side.
GetModelCatalog: func(force bool) (modelsdev.Catalog, error) { return mdc.Catalog(context.Background(), force) },
```
One `modelsdev.Client` is constructed per wiring (embedded/remote) and captured by
the closures — no per-call re-construction, no extra struct field.

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
  new `FilteredList` widget; see the cross-repo decision under Design criteria (4).)
- **Focus contract (spell it out for the implementer):** the `Select` has its own
  `typeAhead`/`popupType` (`widget_select.go:349,406`) that captures keystrokes
  when it holds focus. So substring filtering works **only while the filter TextBox
  is focused**; the user types to narrow, then **Tab** to the Select to arrow/pick.
  Initial focus is the TextBox. This is "type in the box to filter," not "type
  anywhere." The narrowed Select still scrolls (scrollbar, `widget_select.go:263`),
  so a few hundred options remain navigable once filtered.
- "Refresh catalog" button → `GetModelCatalog(true)` then rebuild the list.
- Next → Step 2 with the chosen provider id.

**Step 2 — Model picker:** same filter+Select pattern over
`cat[providerID].Models`. Each row: `"<DisplayName>  · ctx <Nk> · out <Nk> [· reasoning] [· free]"`.
Back → Step 1; Next → Step 3 with the chosen model.

**Step 3 — Review form:** a **standalone dialog** (`showReviewModelDialog`) that
*replicates* the ~15 lines of single-draft field construction from the editor's
layout — **`showModelEditor` is left byte-for-byte untouched.** This is a
deliberate reversal of an earlier "extract a shared `buildModelFields` helper"
idea: that helper is **not cleanly extractable** (the editor's `load(i)`/`store(i)`
index into `models[cur]`, `sel.OnChange` does `store(cur);cur=i;load(cur)`, and
`scanModels` captures `cur`/`target` for a mid-scan selection-change guard,
`model_editor.go:182-251`) and the existing #389 test drives only `SetModels` +
session-window refresh — it never opens the editor, so it would **not** catch a
regression in re-wired editor closures. The review step has a *single* draft and no
selector, so it gets its own flat `load`/`store` over one `config.ModelConfig`
(label/box rows identical in appearance, ~15 lines, zero shared mutable state with
the editor). Lower risk than re-plumbing a tested, closure-heavy function behind a
weaker net.

Pre-filled from `modelsdev.ToModelConfig(providerID, p, m)` with
`Name = UniqueName(…, takenNames)`:
- **Name** — shown read-only (generated, unique) with an "Edit" toggle; conflict is
  surfaced live (re-suffix if the user types a colliding name).
- **API type** — read-only label "from catalog" (the existing api-type `Select`
  pre-selected, disabled).
- **Display name, Endpoint, Model id, Temperature, Max tokens, Reasoning, Thinking**
  — auto-filled, **fully editable**. Model id offers a **"Scan"** button (same
  `handlers.ScanModels` draft-probe as the editor) to swap to a live dropdown —
  but `ScanModels` probes the backend with the *draft* config, which has a **blank
  API key**, so it 401s until a key is entered. The button is therefore **disabled
  until the API key field is non-empty** (for Vertex, until project+location are
  set), so the user isn't handed a button that always errors pre-credential.
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
    effort default, `Thinking` always nil, limits→MaxTokens/ContextWindow, blank
    APIKey.
  - **Endpoint resolver-awareness (the blocking fix):** `anthropic` → `Endpoint==""`
    and, fed through `model.NewModelConnectionFromConfig` + the resolver, the chat
    URL is `https://api.anthropic.com/v1/messages` (single `/v1`, **not**
    `/v1/v1/messages`); `zai`/`openrouter`/`vertex*` → `Endpoint==""`; a generic
    `openai`-mapped provider (e.g. groq) → `Endpoint == p.API`.
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
  `issue389_test.go` refresh behaviour unaffected. **`showModelEditor` is left
  byte-for-byte untouched** (the review step is a standalone dialog, §6.3), so no
  new test is needed to protect the editor's closures — the change cannot reach
  them.

---

## Design criteria

### (1) Goal match — it's exactly the feature asked, no scope creep
A user adds a model end-to-end in the TUI (provider→model→review→save) with no
hand-editing of `config.json`; all mappable fields auto-filled from models.dev and
editable; only the key (or Vertex project/location) is prompted; Save creates a
**new, unique, persisted** entry that's immediately selectable. Non-goals are
honoured: no auto-sync of existing entries, no pricing/benchmark UI, the manual
editor and hand-edit path are untouched. The seeded `GetDefaultConfig()` entries
remain as fallback/seed. Crucially the auto-fill is **correct out of the box** for
every mapped provider, not just plausible-looking: the Endpoint mapping is
resolver-aware (§2.4) so Anthropic resolves to `…/v1/messages` rather than the
double-`/v1` 404 that a naïve `Endpoint = p.API` would produce — an editable review
step does not excuse a silently-wrong default.

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
no aliasing). The review step is a **standalone dialog**; `showModelEditor` is left
byte-for-byte untouched, so the closure-heavy, multi-model editor (and the #389
live-refresh behaviour) cannot regress from this change — no risky shared-helper
refactor. New `Handlers` fields are nil-able, so every existing `SetHandlers`
caller and test
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
3. **(Resolved — now §2.4.)** Endpoint mapping is resolver-aware: blank for the
   derive-base set (`anthropic`, `zai`, `openrouter`, `vertex*`), `p.API` for
   generic `openai`. Anthropic was the missed case (version in chatPath, not base)
   and is now covered + unit-tested. No longer open.
4. **Cache location in remote mode** writes `~/.gogent/modelsdev-cache.json` on the
   *client* host (may create `~/.gogent` there). Acceptable, or gate the cache to
   embedded mode only? **Lean:** allow it (harmless, speeds repeat opens).
