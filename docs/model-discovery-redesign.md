# Model Discovery & Selection — Redesign

> Status: **DESIGN / under review** — no implementation yet.
> Living document: update as the work lands. Section 11 tracks decisions; Section 12 tracks open items.

## 1. Motivation

Today, configuring a model is unintuitive and bimodal:

- **Live "Scan"** probes a backend's `/models` endpoint but collapses everything to a
  `[]string` of ids (`ModelInfo` is only `{ID,Object,OwnedBy}`); all capability metadata
  the endpoint returns is discarded, and Scan only works *after* a model is saved (edit mode).
- **The catalog wizard** (models.dev) auto-fills capabilities nicely but is a *separate*
  door, is limited to catalog coverage, and locks `api_type`.
- The two paths **never meet**. Capabilities reach a saved `ModelConfig` only via the catalog
  transform or hand-editing.
- **Credentials are duplicated per model** — adding three GLM models means entering the key
  three times. There is no "provider I've connected to, which offers many models" concept.
- The manual editor is a flat 12-field form with **zero provider awareness**: the user must
  already know the wire type, endpoint conventions, and exact id-prefix scheme
  (`google/gemini-…` vs bare `gemini-…` vs `anthropic/claude-…`).

Goal: **pick a provider connection → browse a capability-rich, availability-aware model list →
pick → done**, with credentials entered once per provider.

## 2. Evidence: what providers actually expose (probed live)

| Provider | List endpoint | Auth | # | Self-describes capabilities? |
|---|---|---|---|---|
| **OpenRouter** | `GET /api/v1/models` | Bearer | 339 | **Full** — context, pricing incl. cache read/write, modalities, supported params, reasoning efforts |
| **Anthropic** | `GET /v1/models` | x-api-key + `anthropic-version` | 10 | **Partial** — max input/output tokens, capability flags (effort, thinking, vision, pdf); *no pricing* |
| **Z.AI / GLM** | `GET …/v4/models` | Bearer | 8 | **None** — `{id,object,created,owned_by}` |
| **Vertex** (google + anthropic publishers) | `GET v1beta1/publishers/{p}/models` (global host) | ADC Bearer + `X-Goog-User-Project` | 21 Gemini + 10 Claude | **None** — `name`, `versionId`, `launchStage` |

**models.dev** catalog (cached at `~/.gogent/modelsdev-cache.json`; live `api.json` = 147 providers /
~5,200 models) covers all of them — zai/GLM, Vertex (incl. Claude-on-Vertex), OpenRouter — with
context window, output cap, reasoning options, pricing, capability flags. The raw `api.json`
additionally carries (not currently captured by gogent's lossy projection):
`cost.cache_read`, `cost.cache_write`, `modalities.{input,output}`, `knowledge`, `release_date`,
`open_weights`, `status`, `structured_output`, and `reasoning_options[budget_tokens].min`.

**Core insight:** live endpoints tell you *what this account can use* (availability); the catalog
tells you *what each model can do* (capabilities). Neither alone is sufficient — the redesign
**merges** them.

## 3. Current architecture (the two disjoint paths)

```
PATH A (live Scan):  draft ModelConfig → ScanModels → ListModels → lister.list → HTTP GET /models
                     → []ModelInfo{ID,Object,OwnedBy} → FLATTEN to []string → UI dropdown
                     → writes ONLY ModelConfig.Model

PATH B (catalog):    Catalog() → wizard pick provider → pick model → ToModelConfig()
                     → fully-filled ModelConfig (ctx, max_tokens, effort, free…)

(3rd track) caps.go + model_overrides.go: per-(provider,model) WIRE QUIRKS (RejectsSampling,
            cache multipliers). Catalog "tier 2" defaults intentionally NOT wired yet.
```

## 4. Target architecture

```
ProviderConnection (NEW persisted unit)        ModelConfig (references a connection)
  name, api_type                                 name, display_name
  endpoint            (request base override)    connection  →
  discovery_endpoint  (listing host override)    model       (native id, route-formatted)
  api_key|project+location|adc                   caps        (ModelCapabilities snapshot)
        │                                         temperature/top_p/max_tokens/
        │ Discover(conn, catalog)                 reasoning_effort/thinking/cache_ttl  (tuning)
        ▼
   live IDs  ⨝  catalog metadata  → []DiscoveredModel (id + merged caps + availability flag)
        │ user picks one
        ▼
   ModelConfig with caps snapshot
```

### 4.1 Config schema

```go
type Config struct {
    Connections  []*ProviderConnection `json:"connections"`
    Models       []*ModelConfig        `json:"models"`
    DefaultModel string                `json:"default_model"` // -> ModelConfig.Name
    FastModel    string                `json:"fast_model"`    // -> ModelConfig.Name
    ModelRoles   map[string]string     `json:"model_roles"`
    // …rest unchanged…
}

type ProviderConnection struct {
    Name              string `json:"name"`                          // unique
    APIType           string `json:"api_type"`
    Endpoint          string `json:"endpoint,omitempty"`            // request base override
    DiscoveryEndpoint string `json:"discovery_endpoint,omitempty"`  // listing host override (private Vertex/local)
    APIKey            string `json:"api_key,omitempty"`
    Project           string `json:"project,omitempty"`
    Location          string `json:"location,omitempty"`
}

type ModelConfig struct {
    Name        string            `json:"name"`
    DisplayName string            `json:"display_name"`
    Connection  string            `json:"connection"` // -> ProviderConnection.Name
    Model       string            `json:"model"`      // native id, route-formatted
    Caps        ModelCapabilities `json:"caps"`       // snapshot from discovery
    // per-turn tuning (STAY top-level so the shallow override copy works):
    Temperature     float32 `json:"temperature,omitempty"`
    TopP            float32 `json:"top_p,omitempty"`
    MaxTokens       int     `json:"max_tokens,omitempty"`
    ReasoningEffort string  `json:"reasoning_effort,omitempty"`
    Thinking        *bool   `json:"thinking,omitempty"`
    CacheTTL        string  `json:"cache_ttl,omitempty"`
}

type ModelCapabilities struct {
    ContextWindow, MaxOutput          int
    Reasoning, ThinkingToggle         bool
    EffortOptions                     []string
    Vision, ToolCall, StructuredOutput, CustomTemp bool
    InputModalities, OutputModalities []string
    InputCostPerM, OutputCostPerM     float64
    CacheReadPerM, CacheWritePerM     float64
    Knowledge, ReleaseDate            string
    Source                            string // "merged"|"live"|"catalog"|"manual"
}
```

> **Naming caution:** `internal/model/caps.go` already defines `ModelCaps` — a *wire-quirk*
> override table (`RejectsSampling`, cache multipliers), resolved by `resolveModelCaps`. That is a
> DIFFERENT concept from `ModelConfig.Caps ModelCapabilities` (display/catalog capabilities).
> Keep the type names distinct; consider renaming the wire-quirk type to `ModelQuirks` for clarity.

### 4.2 Discovery engine (`internal/model/discover.go`, new)

```
Discover(ctx, conn, catalog) []DiscoveredModel
  live := NewModelConnection(conn, probe).ListModels()   // honors conn.DiscoveryEndpoint; /api/tags fallback
  cat  := catalog.modelsFor(conn.APIType, conn.Endpoint) // may be empty (local)
  for id in (live ∪ cat), keyed by normalizeForMatch(apiType, id):
     Available = id ∈ live
     caps,Source =
        live.caps present?      → merge(live ▸ catalog)   (merged / live)
        exact catalog hit?      → catalog                 (catalog)
        catalog FAMILY match?   → catalog family          (catalog)     // local decision
        else                    → empty → needs Manual    (manual)
  include catalog-only ids (Available=false), flagged ⚠
```

- **Capability precedence (display):** live-self-described ▸ exact catalog ▸ catalog family ▸ manual.
- **Wire quirks** (`model_overrides.go`) remain a separate, authoritative track.
- **`normalizeForMatch`:** strip `@version`; reconcile `google/…`↔bare↔`vendor/…` per api_type;
  family fallback (e.g. `llama-3.3-70b-local` → `llama-3.3`). Table-tested against real id shapes.
- **Z.AI multi-catalog:** api_type `zai` maps to several catalog providers
  (zai / zai-coding-plan / zhipuai…); match by endpoint first, then union by model id.

### 4.3 Catalog extension (`internal/modelsdev`)

- `Cost += {CacheRead, CacheWrite float64}`.
- `Model += {Modalities{Input,Output []string}, Knowledge, ReleaseDate string, StructuredOutput, OpenWeights bool}`.
- Read `reasoning_options[type=budget_tokens]` (today only `effort` is read).
- **Cache version bump** forces one re-fetch (old lossy cache discarded). All fields verified present in live `api.json`.
- Replace `ToModelConfig` with `ToModelCapabilities` feeding the discovery merge.

### 4.4 Local & custom-endpoint support

- **Custom/private Vertex:** `Endpoint` already overrides the *request* base, but Scan/listing always
  hits the hardcoded global host (`vertexModelGardenBase`). Add `DiscoveryEndpoint` so the listing
  host is per-connection. (Route suffix `/publishers/{pub}/models/{model}:{action}` stays templated.)
- **Local servers (Ollama/LM Studio/LAN):** `api_type:openai` + `Endpoint` works for completion;
  add `/models`→`/api/tags` fallback in the OpenAI lister so bare Ollama scans.
- **Catalog-less caps:** `Discover` tries a catalog **family match** first; if none, the UI presents a
  **manual-caps form** (`Source="manual"`), persisted in the `Caps` snapshot exactly like a merged one.

### 4.5 Daemon / client split

The TUI is an HTTP client of the daemon. The header sends only `(modelName, effort[, thinking])`
strings; the daemon re-resolves the `ModelConfig` + `ProviderConnection` server-side. Therefore:

- `ProviderConnection` needs a **wire DTO** with credentials **redacted client-side**
  (mirror the existing `HasAPIKey` pattern). Connection resolution stays server-side.
- The selectors resolve by string, so they are transport-agnostic.
- `wire.go` `modelView`/`updateModelRequest` and `api_client.go` `ModelDTO`/`ToModelConfig` change
  shape (Caps nesting + connection ref) — **this is the HTTP API between TUI and daemon**; update both together.

### 4.6 Session header controls

| Control | Binding today | After |
|---|---|---|
| Model selector | resolves by `Name`/`Model` | unchanged |
| Effort selector | `cfg.EffortOptions`, seed `ReasoningEffort` | `cfg.Caps.EffortOptions`; `ReasoningEffort` stays top-level |
| **Thinking toggle (NEW)** | — (config-only `ModelConfig.Thinking`) | per-turn `*bool` gated on `cfg.Caps.ThinkingToggle`, mirrors effort machinery |
| Context length | `ContextWindowOrDefault()` | reads `Caps.ContextWindow` |

The per-turn override is a shallow copy `override := *cfg; override.ReasoningEffort = effort`
(`gogent.go:~2660`). Keeping tuning fields (`ReasoningEffort`, `Thinking`) top-level and `Caps` a
**value** (not pointer) keeps this safe. Add `override.Thinking` for the new toggle.

> **"thinking" is already overloaded.** `/thinking` = live CoT *streaming* toggle
> (`Experimental.StreamThinking`); a transcript "Toggle thinking" *filter*; `ModelConfig.Thinking` =
> the *capability*. The new header control is a FOURTH — name it distinctly
> (e.g. "Extended thinking" / "Reasoning mode") to avoid collision.

## 5. UI surface & placement

- **Connections manager:** new top-level Config menu entry `&Connections…` (`tui.go settingsItems`)
  + command-palette twin (beside `command_palette.go:249`). Optional secondary "Connections…" button
  in the Models dialog.
- **Unified Discover list:** replace the catalog wizard's provider→model steps
  (`model_catalog_dialog.go showCatalogProviderStep/ModelStep`); reuse `showPicker`/`wireFilter`,
  extend `modelRow` with `✓` (available) / `⚠` (catalog-only, may need access) flags.
- **Manual-caps form:** a variant of `showModelForm` / `showCatalogReviewStep`, gated for catalog-less models.
- **Thinking toggle:** new widget pair in `session_window.go`, right-aligned left of the effort control;
  add `layoutThinkingControl` mirroring `layoutEffortControl`; copy the `effortEnabled`/`guardEffortSelect`/
  greying machinery; gate on `Caps.ThinkingToggle`. Header is layout-tight — effort already collision-hides
  on narrow windows; thinking should drop out first.
- **First-run / empty state:** `welcome_dialog.go` body and the empty-list placeholder
  (`model_dialog.go:241`) say nothing about connections today — add a "create your first connection" nudge.
- **Display surface:** `overall_stats.go:497` `formatEndpoint(model.Endpoint, model.APIType)` must resolve
  endpoint/api_type via the model's **connection** now.

## 6. Full change map (verified call sites)

### Credentials → ProviderConnection
- Fields removed from `ModelConfig`: `APIType,Endpoint,APIKey,Project,Location` (`config.go:24-35`).
- Auth read: `connection.go:905` (`p.auth.roundTripper`), `provider.go:474-479` (`cfg.APIKey`),
  `provider_vertex.go:78,110,119,167` (`Endpoint/Project/Location`).
- Endpoint resolution: `provider.go:384-417` (both resolvers), `provider_vertex.go:97-146`.
- Validation: `connection.go:938-1012` (`validateRoutableConfig`/`ValidateModelConfig`),
  `provider.go:244` + `provider_vertex.go:45,252` (provider validate), `derivesBase` `provider.go:256-261`.
- Bootstrap: `cmd/main.go:142-148`, `cmd/daemon.go:399-402` (replace bare ctor with resolved default conn).
- Key-preserve-on-blank: `resources.go:70-77`, `api_client.go:442` → move to connection update.
- DTOs: `wire.go:471-493` (`modelView`/`modelToView`), `api_client.go:415-460` (`ModelDTO`/`ToModelConfig`).
- UI read/write: `model_editor.go`, `model_catalog_dialog.go`, `model_dialog.go:51` (label),
  `overall_stats.go:497` (display).

### Caps relocation (→ ModelConfig.Caps)
- Fields moved: `MaxTokens?,ContextWindow,ReasoningEffort?,EffortOptions,Thinking?,Free`
  (Note: `MaxTokens`, `ReasoningEffort`, `Thinking`, `CacheTTL`, `TopP` stay as **tuning** on
  ModelConfig; `ContextWindow`,`EffortOptions`,`Free`,pricing,modalities,flags move to **Caps**).
- Accessors: `config.go:84-148` (`ContextWindowOrDefault`, `IsReasoningModel`, cache-TTL helpers) — retarget.
- Runtime reads (all via embedded `c.Config`): `connection.go buildRequest 1421-1545`, `1347`,
  `gemini_cache.go:96`, cost weighting `connection.go:1370-1382` (`CostWeightedInput`).
- Effort UI: `session_window.go:868,900`; catalog `model_catalog_dialog.go:521-522`; DTO `api_client.go:456`, `wire.go`.
- Context UI: stats-fed in most places (`statistics_render.go:88`, `session_window.go:3340+`), config-fed at `gogent.go:2685`.
- Seeds: `config.go GetDefaultConfig 1110-1283` (10 models) + `config.sample.json` (8) — rewrite to new shape.
- Transform: `modelsdev/transform.go:65-85`.

### Scan + catalog → Discover()
- Live: `gogent.go:3616 ScanModels`, `resources.go:112`, `api_client.go:819`, `model_editor.go:156-182`.
- Catalog: `modelsdev/modelsdev.go` (`Catalog`,`Fetcher`,`catalogURL`), `GetModelCatalog` handlers
  (`embedded_handlers.go:208`, `remote_handlers.go:1334`), `model_catalog_dialog.go`.
- New merge: `internal/model/discover.go`; widen `ModelInfo` (`connector.go:88`) to carry parsed caps.

### Cost / cache weighting
- Move cache multipliers to derive from `Caps.CacheRead/WritePerM ÷ InputCostPerM`; retarget
  `CostWeightedInput` (`connection.go:1370-1382`); drop the DeepSeek hardcode in `model_overrides.go`
  (becomes catalog data). Keep `model.ModelCaps` (renamed `ModelQuirks`) for `RejectsSampling`.

### Unaffected (key by Name — confirmed)
- Stats/attribution (`SetPrimaryModel(Name)`), roles (`ModelForRole`), sub-agents (inherit parent
  connector — `user_session.go:2695`), watchers/commands (by Name), session persistence
  (`session_store.go` stores Name + frozen DisplayName/ModelID). No per-sub-agent/per-watcher model
  config exists.

### Multimodal note
- Image-sending is **never gated** on capability today (`connection.go:164-176` always emits images).
  Decision: **warn on mismatch** — a non-blocking warning when a turn sends images to a model whose
  `Caps.Vision` is false. New consumer at the serialization/turn seam; images are still sent.

## 7. Migration

No migrator code. The maintainer's `~/.gogent/config.json` is **rewritten once, by hand**, into the
new shape (one `ProviderConnection` per distinct `(api_type, endpoint, key|project+location)` tuple;
models repointed; caps moved under `Caps`). `GetDefaultConfig` and `config.sample.json` are rewritten
to the new shape directly. The loader simply reads the new schema; legacy flat configs are not supported.

## 8. Build order (one branch, multiple commits)

1. Schema + model-package rebind (`NewModelConnection(conn,m)`; strategies take connection) — compiles on new shape.
2. Catalog extension (new fields, cache bump, `ToModelCapabilities`).
3. Discovery engine + caps wiring + manual/family-match + cost weighting.
4. Wire/orchestration: DTOs (redaction), connection CRUD, validation split, bootstrap, migrator, thinking param.
5. UI: connections dialog, discovery list, manual-caps form, thinking toggle, effort/ctx read from Caps, first-run nudge.
6. Tests throughout (~50 model test files + config/server/ui reworked); merge/normalization/family-match table tests.

## 9. Risk / attention sites
- `ModelConnection` embeds `*config.ModelConfig` as `Config`; the rebind threads a `*ProviderConnection` too.
- Per-turn shallow-copy override (`gogent.go:~2660`) — keep `Caps` a value, tuning top-level.
- DTO shape change = HTTP API change between TUI and daemon — update `wire.go` + `api_client.go` together.
- Type-name collision `ModelCaps` (existing wire quirks) vs `ModelCapabilities` (new).

## 10. Naming decisions
- `ProviderConnection` / `Connection` — accepted.
- New capability struct: `ModelCapabilities`; field `ModelConfig.Caps`.
- Header thinking control: pick a non-colliding label (e.g. "Extended thinking").

## 11. Decisions log
- Scope: **full** — Provider Connections + unified discovery (no backward-compat for public users).
- Catalog-only models: **show, flagged ⚠**.
- Catalog extension (cache pricing + modalities): **now**.
- Thinking: **per-session header toggle**, gated on capability.
- Local caps: **catalog family-match, then manual fallback**.
- Granularity: **one branch, multiple commits**; this doc is the living reference.
- Migration: **one-off manual rewrite** of the existing config (no migrator code).
- Vision/modalities: **warn on mismatch** (non-blocking; images still sent).
- Rename `model.ModelCaps` → **`ModelQuirks`**.

## 12. Open items / to confirm
- (all initial design questions resolved — see §11)
