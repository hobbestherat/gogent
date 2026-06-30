package model

// OpenAI-compatible providers: the generic OpenAI API plus gateways that share
// its wire format and differ only in default base URL, auth, and attribution
// headers (Z.AI, OpenRouter). All three use openAIAdapter and the OpenAI
// "GET <base>/models" listing convention.

func init() {
	registerProvider(&provider{
		apiType: APITypeOpenAI,
		adapter: openAIAdapter{},
		caps: Capabilities{
			// OpenAI reasoning models (o-series, GPT-5) require max_completion_tokens
			// and reject a custom temperature; they accept reasoning_effort.
			ReasoningTokenParam:         "max_completion_tokens",
			ReasoningRejectsTemperature: true,
			SupportsReasoningEffort:     true,
			SupportsResponseFormat:      true,
			// Cached input is ~0.5x; caching is automatic (no client directive), no
			// write count. DeepSeek rides this api_type but caches deeper — it
			// overrides the read multiplier per-model via ModelQuirks (model_overrides.go).
			CacheReadMultiplier: 0.50,
		},
		// Neutral local default; apps with their own default resolve it upstream
		// (config.DefaultEndpoint) and pass a full endpoint in.
		endpoints: staticBaseEndpoints{defaultBaseURL: "http://localhost:8080/v1", chatPath: "/chat/completions"},
		auth:      keyAuth{mode: authBearer},
		// tagsPath enables the bare-Ollama /api/tags fallback: a local server that
		// only speaks the native Ollama API (no /v1/models) is still scannable. Only
		// the generic OpenAI api_type sets it; the hosted gateways below do not.
		lister: openAILister{chatPath: "/chat/completions", modelsPath: "/models", tagsPath: "/api/tags"},
	})

	registerProvider(&provider{
		apiType: APITypeZAI,
		adapter: openAIAdapter{},
		caps: Capabilities{
			// Z.AI rejects max_tokens outside [1, 131072]; GLM keeps max_tokens and
			// accepts a temperature, and exposes thinking + reasoning_effort.
			MaxTokensLimit:          131072,
			SupportsReasoningEffort: true,
			SupportsThinking:        true,
			SupportsResponseFormat:  true,
			// GLM caches automatically (no client directive), no write count. 0.20 is
			// a provisional read discount pending confirmation against Z.AI pricing
			// (design Open Q3); it still beats counting cache hits at full price.
			CacheReadMultiplier: 0.20,
		},
		endpoints:   staticBaseEndpoints{defaultBaseURL: "https://api.z.ai/api/paas/v4", chatPath: "/chat/completions"},
		auth:        keyAuth{mode: authBearer},
		lister:      openAILister{chatPath: "/chat/completions", modelsPath: "/models"},
		derivesBase: true, // base URL is the Z.AI default; an empty endpoint is routable
	})

	registerProvider(&provider{
		apiType: APITypeOpenRouter,
		adapter: openAIAdapter{},
		// OpenRouter is a passthrough: the real cache discount depends on the
		// underlying model (e.g. Anthropic ~0.1x), which is not knowable from the
		// api_type, and it exposes no cache-write count. So cache reads are priced at
		// 1.0 (no multiplier set) — a deliberate, documented known-inaccuracy that
		// never UNDER-counts the budget. Per-model ModelQuirks rows can refine it later.
		caps:      Capabilities{SupportsResponseFormat: true},
		endpoints: staticBaseEndpoints{defaultBaseURL: "https://openrouter.ai/api/v1", chatPath: "/chat/completions"},
		auth: keyAuth{mode: authBearer, extraHeaders: map[string]string{
			"HTTP-Referer": openRouterReferer,
			"X-Title":      openRouterTitle,
		}},
		lister:      openAILister{chatPath: "/chat/completions", modelsPath: "/models"},
		derivesBase: true, // base URL is the OpenRouter default; an empty endpoint is routable
	})
}
