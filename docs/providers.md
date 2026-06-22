# Model providers

gogent talks to LLM providers through a provider abstraction (`internal/model`). Each model entry in your config has an `api_type` that selects both an endpoint layout (the `providerSpec`) and a wire-format adapter. This lets a single codebase speak to OpenAI-compatible servers, the Anthropic Messages API, and gateway products that layer extra behavior on top of the OpenAI wire format.

For the full `ModelConfig` schema (every field, defaults, and validation), see [configuration.md](configuration.md). This page focuses on how each provider behaves and how to configure it.

## api_type values

`api_type` accepts four values. The resolver (`StringToAPIType`) lowercases and trims the input, maps `"z.ai"` → `zai` and `"claude"` → `anthropic`, and treats any unknown or empty value as `openai` (the default).

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

## Authentication modes

Each `providerSpec` carries an `authMode` that decides how the API key is attached to requests:

| Mode | Header / mechanism | Used by |
|------|--------------------|---------|
| `authBearer` | `Authorization: Bearer <key>` | OpenAI, Z.AI, OpenRouter (default) |
| `authXAPIKey` | `x-api-key` header | Anthropic |
| `authAzureKey` | `api-key` header | Azure OpenAI |
| `authQuery` | `?key=` in the URL | Gemini-style |

With no key configured, the client uses the shared HTTP transport directly (no auth header injected). Providers may also attach extra static headers: Anthropic sends `anthropic-version`; OpenRouter sends `HTTP-Referer` and `X-Title`.

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
- During `buildRequest`, reasoning models on a spec with `reasoningTokenParam == "max_completion_tokens"` set `MaxCompletionTokens` instead of `MaxTokens`; temperature/top_p are omitted when `reasoningRejectsTemperature`; `reasoning_effort`/`thinking` are emitted only when the corresponding flag is set. `max_tokens` is clamped to `spec.maxTokensLimit` (Z.AI: 131072).

Recognized `reasoning_effort` values: `minimal`, `low`, `medium`, `high` — plus `none`, `max`, and `xhigh` on Z.AI GLM-5.2.

## Prompt-cache accounting

gogent tracks cache usage so you can see how much of your prompt was served from a discount cache:

- `TokenUsage.CachedTokens` — prompt tokens served from cache (a subset of `PromptTokens`, billed at the discounted rate).
- `TokenUsage.ReasoningTokens` — internal chain-of-thought output tokens (a subset of `CompletionTokens`).

`UnmarshalJSON` normalizes two wire shapes: the OpenAI-compatible `usage.prompt_tokens_details.cached_tokens`, **or** the DeepSeek-style top-level `prompt_cache_hit_tokens`. It also lifts `completion_tokens_details.reasoning_tokens` into `ReasoningTokens`.

For Anthropic, the adapter sums `InputTokens + CacheReadInputTokens + CacheCreationInputTokens` into `PromptTokens`, and routes `CacheReadInputTokens` into `CachedTokens`.

Stats accumulate `TotalTokensIn`, `TotalCachedTokensIn`, and `TotalTokensOut` across the session. OpenAI-compatible backends cache the stable prefix (tools + system prompt + history) automatically — no request markers are needed; gogent sends messages in stable-to-volatile order so the cacheable prefix stays intact.

## Streaming

`CompleteStream` and `CompleteWithToolsStreamCtx` build the request with `stream=true` and `StreamOptions{IncludeUsage: true}`; the SSE stream is parsed by the adapter's `parseStream`.

**OpenAI stream** (`parseOpenAIStream`): reads with a `bufio.Reader`, accumulates tool-call fragments by `Index` in first-seen order, and surfaces reasoning deltas from **either** `reasoning_content` (Z.AI/GLM, DeepSeek) **or** `reasoning` (OpenRouter). It drains until `[DONE]` or EOF, then emits one terminal `StreamResponse{Done: true}` carrying the assembled `ToolCalls`, `FinishReason`, and `Usage`. If vLLM omits tool-call ids, they are synthesized as `call_<idx>`.

**Anthropic stream**: event-driven — `message_start` → `content_block_start` → `content_block_delta` (`text_delta` | `thinking_delta` | `input_json_delta`) → `message_delta` → `message_stop`. `thinking_delta` maps to `StreamResponse.Reasoning`; tool arguments accumulate per content-block index.

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
- Model id — with a **Scan** button that probes the backend's listing endpoint and replaces the text field with a dropdown of advertised models
- API key
- Temperature
- Max tokens
- Reasoning (effort)
- Thinking (default / on / off select)

Buttons: **Save**, **Cancel**, **Set Default**.

Note that `effort_options`, `context_window`, `top_p`, and `free` are config-only fields — they are not exposed in the editor. Per-session effort options appear in the window header instead.
