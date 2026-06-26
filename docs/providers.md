# Model providers

gogent talks to LLM providers through a provider abstraction (`internal/model`). Each model entry in your config has an `api_type` that selects a registered **provider**. This lets a single codebase speak to OpenAI-compatible servers, the Anthropic Messages API, Google Vertex AI, and gateway products that layer extra behavior on top of the OpenAI wire format.

For the full `ModelConfig` schema (every field, defaults, and validation), see [configuration.md](configuration.md). This page focuses on how each provider behaves and how to configure it. For how the provider layer is structured (and how to add a backend or an operation), see [Architecture](#architecture-adding-a-provider-or-operation).

## api_type values

`api_type` selects the provider conventions. The resolver (`StringToAPIType`) lowercases and trims the input, maps a few aliases (`"z.ai"` → `zai`, `"claude"` → `anthropic`, `"gemini"` → `vertex-native`, `"claude-vertex"` → `vertex-anthropic`), and treats any unknown or empty value as `openai` (the default). The full set: `openai`, `zai`, `anthropic`, `openrouter`, and the three Google Vertex AI types `vertex`, `vertex-native`, and `vertex-anthropic`.

### `openai` (default)

Any OpenAI-compatible server. The `endpoint` may be a full `.../chat/completions` URL or just a base URL (e.g. `https://host/v1`); the remaining paths are appended automatically. Authentication is a bearer token sent as `Authorization: Bearer <key>`.

Other OpenAI-compatible gateways work under `openai` by pointing `endpoint` at their base URL and setting `api_key`. This includes the Google Gemini OpenAI-compatibility layer and Azure OpenAI (Azure uses the `api-key` header — see [Authentication modes](#authentication-modes)).

```json
{
  "name": "Local vLLM",
  "api_type": "openai",
  "endpoint": "http://localhost:8000/v1",
  "model": "meta-llama/Llama-3.1-70B-Instruct",
  "api_key": "token-ignored-by-vllm",
  "temperature": 0.7,
  "max_tokens": 4096
}
```

### `zai`

The Z.AI platform (`https://docs.z.ai`). The wire format is OpenAI-compatible; only the default base URL differs (`https://api.z.ai/api/paas/v4`). Leave `endpoint` empty to use the built-in base URL. The coding-plan models live under a different base (`.../api/coding/paas/v4`), so set `endpoint` explicitly for those. Authentication is bearer.

```json
{
  "name": "Z.AI GLM",
  "api_type": "zai",
  "endpoint": "",
  "model": "glm-4.6",
  "api_key": "your-zai-key",
  "reasoning_effort": "medium"
}
```

### `anthropic` (alias `claude`)

The Anthropic Messages API (`https://platform.claude.com`). This is **not** OpenAI-compatible: a dedicated adapter speaks the native `POST /v1/messages` protocol — `x-api-key` authentication plus the `anthropic-version: 2023-06-01` header, a top-level `system` prompt, content-block message arrays, and `input_schema` / `tool_use` / `tool_result` tool definitions. Leave `endpoint` empty to use `https://api.anthropic.com`.

gogent translates to and from its internal OpenAI-shaped types, so tools and prompt-cache accounting work unchanged. Extended thinking is **not** yet exposed for this provider.

```json
{
  "name": "Claude Sonnet",
  "api_type": "anthropic",
  "endpoint": "",
  "model": "claude-sonnet-4-5",
  "api_key": "your-anthropic-key",
  "temperature": 1.0,
  "max_tokens": 8192
}
```

### `openrouter`

The OpenRouter gateway (`https://openrouter.ai`). OpenAI-compatible (bearer auth, same wire format) but additionally sends the recommended `HTTP-Referer` and `X-Title` attribution headers used for app ranking and free-tier prioritization. Leave `endpoint` empty to use `https://openrouter.ai/api/v1`.

```json
{
  "name": "OpenRouter",
  "api_type": "openrouter",
  "endpoint": "",
  "model": "anthropic/claude-sonnet-4.5",
  "api_key": "sk-or-...",
  "temperature": 0.7,
  "max_tokens": 8192
}
```

### Google Vertex AI (`vertex`, `vertex-native`, `vertex-anthropic`)

Three `api_type` values target Google Vertex AI. All three authenticate with **Google Application Default Credentials** (no `api_key`): run `gcloud auth application-default login` or set `GOOGLE_APPLICATION_CREDENTIALS`. A short-lived bearer token is minted (and auto-refreshed) per request by `ADCRoundTripper`. The base URL is derived from the config's `project` and `location` (e.g. `us-east5`, or `global`) when `endpoint` is left empty; an explicit `endpoint` overrides the host but the model-path suffix is still appended.

| `api_type` | Wire format | Vertex route | Notes |
|------------|-------------|--------------|-------|
| `vertex` | OpenAI-compatible | `/endpoints/openapi/chat/completions` (v1beta1) | Reuses the OpenAI adapter; name a model like `google/gemini-2.5-flash`. |
| `vertex-native` (alias `gemini`) | Native Gemini | `:generateContent` / `:streamGenerateContent?alt=sse` (v1) | `contents`/`parts`, `systemInstruction`, `thinkingConfig`, `responseSchema`. |
| `vertex-anthropic` (alias `claude-vertex`) | Anthropic Messages | `:rawPredict` / `:streamRawPredict` (v1) | Claude on Vertex — see below. |

**Model discovery (Scan).** All three Vertex types support the model editor's **Scan** button. Listing goes through the Vertex Model Garden (`GET https://aiplatform.googleapis.com/v1beta1/publishers/{publisher}/models`), authenticated with the same ADC token plus an `X-Goog-User-Project: <project>` header (the catalog has no project in its path, so the quota project is carried in the header). `vertex` / `vertex-native` list the `google` publisher (filtered to `gemini*` / `gemma*` chat models); `vertex-anthropic` lists the `anthropic` publisher (`claude*`). Ids are formatted for the route: `google/<model>` for the OpenAI-compat shim, bare `<model>` for the native and Claude routes. A model with no `project` set cannot be scanned (the quota header can't be formed).

**`vertex-anthropic`** serves Anthropic's Claude models through Vertex. It reuses the Anthropic Messages adapter but with three Vertex-specific differences from the direct `anthropic` provider:

1. **Auth is ADC**, not `x-api-key`.
2. **The model name lives in the URL path** (`publishers/anthropic/models/{MODEL}:rawPredict`), so it is omitted from the request body. Use the bare first-party id (`claude-opus-4-8`); dated snapshots take an `@` separator (`claude-opus-4-5@20251101`) — there is **no** `anthropic.` prefix (that is Amazon Bedrock).
3. **The API version travels in the body** as `"anthropic_version": "vertex-2023-10-16"`, not the `anthropic-version` header.

It also uses the model optimally on Vertex:

- **Extended thinking** is exposed here (unlike the direct `anthropic` provider). Set `thinking: true` and it is emitted as **adaptive thinking** (`{"type":"adaptive","display":"summarized"}`) — the only thinking mode current Claude models accept. The thinking block (summary text + signature) is captured from the response and **replayed unmodified ahead of the turn's `tool_use` blocks**, which Anthropic requires for tool use with thinking enabled.
- **Sampling params are dropped** (`temperature`/`top_p`): modern Claude rejects them, so behavior is steered by prompting.
- **Prompt caching** is on by default — a 5-minute `cache_control` breakpoint is placed on the system prompt and on the last turn, so a growing agent transcript is largely served from cache across turns.

```json
{
  "name": "Claude Opus (Vertex)",
  "api_type": "vertex-anthropic",
  "endpoint": "",
  "project": "your-gcp-project",
  "location": "global",
  "model": "claude-opus-4-8",
  "max_tokens": 8192,
  "thinking": true
}
```

## Authentication modes

Each `providerSpec` carries an `authMode` that decides how the API key is attached to requests:

| Mode | Header / mechanism | Used by |
|------|--------------------|---------|
| `authBearer` | `Authorization: Bearer <key>` | OpenAI, Z.AI, OpenRouter (default) |
| `authXAPIKey` | `x-api-key` header | Anthropic |
| `authAzureKey` | `api-key` header | Azure OpenAI |
| `authQuery` | `?key=` in the URL | Gemini-style |
| `authADC` | `Authorization: Bearer <ADC token>` (minted/refreshed per request, no `api_key`) | Vertex AI (`vertex`, `vertex-native`, `vertex-anthropic`) |

With no key configured (and a non-ADC provider), the client uses the shared HTTP transport directly (no auth header injected). Providers may also attach extra static headers: Anthropic sends `anthropic-version` (the Vertex Claude route instead carries the version in the body, so it adds no header); OpenRouter sends `HTTP-Referer` and `X-Title`.

## Endpoint normalization

Each `providerSpec` carries `defaultBaseURL`, `chatPath`, and `modelsPath`. Endpoint resolution works as follows:

- An **empty** `endpoint` resolves to `spec.defaultBaseURL`.
- A **full chat-completions URL** is trimmed back to its base via `stripChatPath`, which strips the chat path from the URL's path component only and preserves any query string (important for Azure's `?api-version=`).
- Trailing slashes are dropped.
- `chatURL()` / `modelsURL()` are computed as `appendPath(base, path)`, inserting the path **before** any query string.
- `modelsURL()` derives the model-listing endpoint by stripping the chat path and re-deriving via the spec.

## Reasoning models (reasoning_effort / thinking)

Reasoning behavior is config-driven via `ModelConfig.ReasoningEffort` (a string) and `Thinking` (`*bool`). A model counts as a reasoning model when `IsReasoningModel()` returns true — i.e. `ReasoningEffort != "" || Thinking != nil`.

Capability flags on the `providerSpec` gate what gets emitted on the wire:

| Flag | openai | zai | anthropic | openrouter |
|------|--------|-----|-----------|------------|
| `supportsReasoningEffort` | yes | yes | unset | unset |
| `supportsThinking` | — | yes | — | — |
| `reasoningTokenParam` | `max_completion_tokens` | (empty) | (empty) | (empty) |
| `reasoningRejectsTemperature` | true | false | — | — |

Notes:

- **`reasoningTokenParam`**: OpenAI o-series / GPT-5 reject `max_tokens`, so the openai spec uses `max_completion_tokens` instead. Z.AI, Anthropic, and OpenRouter keep `max_tokens`.
- **`reasoningRejectsTemperature`**: openai omits `temperature`/`top_p` for reasoning tiers; zai accepts `temperature`.
- **`vertex-anthropic`** sets `supportsThinking` and emits adaptive thinking (`{"type":"adaptive"}`) when `thinking` is enabled; it also drops `temperature`/`top_p` unconditionally (in the adapter, not via the reasoning gate) because modern Claude rejects them. `vertex-native` (Gemini) emits thinking via `thinkingConfig`. `reasoning_effort` is not an Anthropic body parameter, so it is not sent on either Vertex Claude or Gemini.
- During `buildRequest`, reasoning models on a spec with `reasoningTokenParam == "max_completion_tokens"` set `MaxCompletionTokens` instead of `MaxTokens`; temperature/top_p are omitted when `reasoningRejectsTemperature`; `reasoning_effort`/`thinking` are emitted only when the corresponding flag is set. `max_tokens` is clamped to `spec.maxTokensLimit` (Z.AI: 131072).

Recognized `reasoning_effort` values: `minimal`, `low`, `medium`, `high` — plus `none`, `max`, and `xhigh` on Z.AI GLM-5.2.

## Prompt-cache accounting

gogent tracks cache usage so you can see how much of your prompt was served from a discount cache:

- `TokenUsage.CachedTokens` — prompt tokens served from cache (a subset of `PromptTokens`, billed at the discounted rate).
- `TokenUsage.ReasoningTokens` — internal chain-of-thought output tokens (a subset of `CompletionTokens`).

`UnmarshalJSON` normalizes two wire shapes: the OpenAI-compatible `usage.prompt_tokens_details.cached_tokens`, **or** the DeepSeek-style top-level `prompt_cache_hit_tokens`. It also lifts `completion_tokens_details.reasoning_tokens` into `ReasoningTokens`.

For Anthropic, the adapter sums `InputTokens + CacheReadInputTokens + CacheCreationInputTokens` into `PromptTokens`, and routes `CacheReadInputTokens` into `CachedTokens` — on both the direct `anthropic` and `vertex-anthropic` paths.

Stats accumulate `TotalTokensIn`, `TotalCachedTokensIn`, and `TotalTokensOut` across the session.

### Stable-to-volatile ordering (issue #404)

gogent keeps the prompt's cacheable prefix stable across turns by splitting the per-turn context into a **stable** bucket and a **volatile** bucket:

- **Stable** — the base agent prompt, AGENTS.md instructions, the repo map and the available-skills index. These do not change across turns, so they ride on the system prompt (`messages[0]`).
- **Volatile** — the live git status and the todo checklist. These change whenever a file is edited or a todo is updated, so they are appended as a **trailing per-request `user` message after the transcript**, never persisted to the transcript (recomputed each turn, mirroring how the system prompt is kept out of the transcript).

So the wire order is `[stable system][transcript…][small volatile tail]`. Editing a file (which changes git status) no longer invalidates the cached transcript — only the small tail after the last cache breakpoint is re-sent. This is what lets the implicit prefix cache used by `openai`/`zai`/`openrouter`/`vertex-native` (Gemini) keep hitting on an active editing session. The trailing `user` message merges cleanly into the preceding user turn — including when the last transcript message is a tool/function result (the Anthropic and Gemini adapters both map tool results to a `user` turn and merge consecutive same-role messages).

### Explicit caching on the Anthropic Messages adapter

The Anthropic adapter (`anthropic` and `vertex-anthropic`) emits `cache_control{type:ephemeral}` breakpoints on two blocks:

- the **system prompt**, sent as a one-element block array so the breakpoint can ride on it (Anthropic accepts `cache_control` only on a content block; the direct Messages API accepts the block-array `system` form the same as Vertex), and
- the **end of the cacheable prefix** — the last content block of the last *non-volatile* message, i.e. the last transcript message, *not* the volatile tail.

Both breakpoints are emitted on the direct `anthropic` provider as well as `vertex-anthropic`, so direct-Anthropic users get the same growing-transcript-from-cache benefit (issue #404). `cache_read_input_tokens` reported by the API flows back into `TokenUsage.CachedTokens`.

## Streaming

`CompleteStream` and `CompleteWithToolsStreamCtx` build the request with `stream=true` and `StreamOptions{IncludeUsage: true}`; the SSE stream is parsed by the adapter's `parseStream`.

**OpenAI stream** (`parseOpenAIStream`): reads with a `bufio.Reader`, accumulates tool-call fragments by `Index` in first-seen order, and surfaces reasoning deltas from **either** `reasoning_content` (Z.AI/GLM, DeepSeek) **or** `reasoning` (OpenRouter). It drains until `[DONE]` or EOF, then emits one terminal `StreamResponse{Done: true}` carrying the assembled `ToolCalls`, `FinishReason`, and `Usage`. If vLLM omits tool-call ids, they are synthesized as `call_<idx>`.

**Anthropic stream** (also used by `vertex-anthropic`): event-driven — `message_start` → `content_block_start` → `content_block_delta` (`text_delta` | `thinking_delta` | `signature_delta` | `input_json_delta`) → `message_delta` → `message_stop`. `thinking_delta` maps to `StreamResponse.Reasoning`; the first thinking block's text and its `signature_delta` are also accumulated and surfaced on the terminal `Done` event so the block can be replayed next turn; tool arguments accumulate per content-block index.

`CompleteWithToolsStreamCtx` forwards reasoning deltas to a `ReasoningSink`, assembles a full `CompletionResponse`, and contains panics defensively. Note that the **streaming path does not retry** — a stream cannot be safely replayed mid-stream. Only the blocking `complete()` retries.

## Retry, backoff, and error classification

The blocking `complete()` marshals the request body **once** into a pooled buffer, then retries only the socket send.

- **Attempts**: `maxAttempts = 3` by default.
- **Retryable status codes**: `408`, `409`, `429`, and any `5xx` are retried. Permanent `4xx` (`400`, `401`, `403`, `404`, `422`) fail fast.
- **Backoff**: a `Retry-After` header (capped at 30s) takes precedence; otherwise exponential with **full jitter** — a uniform random delay in `[0, base*2^n]`, with `base = 500ms` and a 30s cap. `sleepCtx` makes the backoff context-cancellable.
- **`parseRetryAfter`** handles both integer-seconds and HTTP-date forms.

`analyzeError` classifies responses into typed `ModelError`s:

| Condition | Error type |
|-----------|------------|
| `400` + "context"/"length" | `ErrorContextOverflow` |
| `403` + "refusal"/"content" | `ErrorRefusal` |
| `429` | `ErrorRateLimit` |
| `504` | `ErrorTimeout` |
| anything else | `ErrorGeneric` |
| context cancellation | `ErrorConnection` |

The shared HTTP transport is reused across all connections, turns, and sub-agent fan-out: `MaxIdleConns = 100`, `MaxIdleConnsPerHost = 32`, HTTP/2 enabled, 90s idle timeout.

## Model editor (TUI)

The model editor (Config → Models) surfaces the following fields for each entry:

- Display name
- API type (dropdown)
- Endpoint
- Model id — with a **Scan** button that queries the provider's listing endpoint and replaces the text field with a dropdown of advertised models. Supported by the OpenAI-compatible providers (`openai`/`zai`/`openrouter`, via `GET <base>/models`), `anthropic` (`GET /v1/models`), and all three Vertex types (via the Model Garden — see [Google Vertex AI](#google-vertex-ai-vertex-vertex-native-vertex-anthropic)). A provider that exposes no listing endpoint reports "not supported"; set the model id manually.
- API key
- Temperature
- Max tokens
- Reasoning (effort)
- Thinking (default / on / off select)

Buttons: **Save**, **Cancel**, **Set Default**.

Note that `effort_options`, `context_window`, `top_p`, and `free` are config-only fields — they are not exposed in the editor. Per-session effort options appear in the window header instead.

## Add model from catalog (models.dev)

Rather than hand-filling the editor, **Config → Add Model from Catalog…** pulls
provider and model metadata from the public [models.dev](https://models.dev)
catalog (`https://models.dev/api.json`, no auth) and pre-fills a new entry. See
[Adding a model from the models.dev catalog](configuration.md#adding-a-model-from-the-modelsdev-catalog)
for the user-facing flow and caching behavior.

The catalog metadata maps onto `api_type` as follows (in `internal/modelsdev`):
`openrouter` → `openrouter`, `zai`/`z-ai`/`z.ai` → `zai`, `anthropic` → `anthropic`,
`google-vertex` → `vertex`, `google-vertex-anthropic` → `vertex-anthropic`; **every
other (and unknown) provider defaults to `openai`**, matching `StringToAPIType`.
All the OpenAI-compatible gateways models.dev lists (Groq, Together, DeepSeek,
Mistral, Fireworks, …) are reached through the generic `openai` provider — the
catalog flow only selects the right *existing* `api_type`; it adds no new adapter.

`endpoint` is filled resolver-aware: it is left **blank** for `anthropic`, `zai`,
`openrouter`, and the Vertex types (whose adapters supply or derive their own base
URL — most importantly `anthropic`, where the `/v1` lives in the chat path, so
feeding the catalog's `/v1` base would double it), and set to the catalog's base
URL for generic `openai` gateways (which otherwise default to a useless localhost
base). `thinking` is left unset (provider default) even when the model advertises a
toggle; flip it in the review form if you want it on.

## Architecture: adding a provider or operation

Each `api_type` maps to a registered `*provider` (`internal/model`) that **composes small strategy objects** rather than carrying a flat config struct. `ModelConnection` is a generic executor that delegates to the provider; it contains no backend-specific logic.

A provider wires together:

- **`endpoints` (`endpointResolver`)** — builds the chat and stream URLs from a model config. `staticBaseEndpoints` (base URL + static chat path, model in the body) covers the OpenAI-compatible and Anthropic providers and Vertex's OpenAI-compat shim; `modelURLEndpoints` (model in the URL path) covers Vertex's native Gemini and Claude routes.
- **`auth` (`authScheme`)** — builds the request round-tripper. `keyAuth` presents an API key (bearer / `x-api-key` / Azure `api-key` / query param) plus any static headers; `adcAuth` mints a Google ADC bearer token. Returning `nil` means no auth (a local server).
- **`adapter`** — the wire-format translator (request/response/stream). OpenAI-compatible backends share `openAIAdapter`; Anthropic and Gemini have dedicated adapters.
- **`caps` (`Capabilities`)** — request-shaping flags (`max_tokens` limit, reasoning/thinking/response_format support) read by `buildRequest`.
- **`lister` (`modelLister`, optional)** — enumerates models for the Scan button. `openAILister` handles the `GET <base>/models` convention; `vertexPublisherLister` handles the Model Garden. `nil` means listing is unsupported.
- **`validate` (optional)** — a deferred config check (e.g. Vertex requiring `project`/`location`).

**To add a backend:** write a `provider_<name>.go` that composes the strategies it needs and calls `registerProvider` from `init()` (see `provider_openai.go`, `provider_anthropic.go`, `provider_vertex.go`). Add its `api_type` to `stringToAPITypeMap` and `apiTypeDisplayOrder` in `provider.go`. Reuse an existing strategy where the behavior matches; write a new strategy value only for a genuinely new axis.

**To add an operation** (token counting, embeddings, …): define a strategy interface and a nil-able field on `provider` (mirroring `lister`), implement it per provider, and run the HTTP call through the shared `ModelConnection.doJSON` runner — it applies the provider's auth and uniform error classification, so the operation only specifies the URL, any extra headers, and the response parse.
