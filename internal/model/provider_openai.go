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
		},
		// Neutral local default; apps with their own default resolve it upstream
		// (config.DefaultEndpoint) and pass a full endpoint in.
		endpoints: staticBaseEndpoints{defaultBaseURL: "http://localhost:8080/v1", chatPath: "/chat/completions"},
		auth:      keyAuth{mode: authBearer},
		lister:    openAILister{chatPath: "/chat/completions", modelsPath: "/models"},
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
		},
		endpoints:   staticBaseEndpoints{defaultBaseURL: "https://api.z.ai/api/paas/v4", chatPath: "/chat/completions"},
		auth:        keyAuth{mode: authBearer},
		lister:      openAILister{chatPath: "/chat/completions", modelsPath: "/models"},
		derivesBase: true, // base URL is the Z.AI default; an empty endpoint is routable
	})

	registerProvider(&provider{
		apiType:   APITypeOpenRouter,
		adapter:   openAIAdapter{},
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
