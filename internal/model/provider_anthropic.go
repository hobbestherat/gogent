package model

// Anthropic providers: the direct Messages API (x-api-key auth) and Claude on
// Google Vertex AI (ADC auth, model-in-URL :rawPredict routes, anthropic_version
// in the body). Both speak the Anthropic Messages wire format via anthropicAdapter;
// the vertex flag switches the body shape (see anthropicAdapter.buildBody).

func init() {
	registerProvider(&provider{
		apiType: APITypeAnthropic,
		adapter: anthropicAdapter{},
		// Extended thinking and reasoning_effort are not wired through the direct
		// Anthropic adapter, so their capability flags stay unset; structured output
		// is achieved via strict tools rather than response_format. Cache: reads at
		// 0.1x, writes (cache_creation_input_tokens) at the 1.25x 5-minute-breakpoint
		// premium; client-side cache_control breakpoints are declared for #545.
		caps: Capabilities{
			CacheControl:         CacheControlBreakpoints,
			CacheReadMultiplier:  0.10,
			CacheWriteMultiplier: 1.25,
		},
		endpoints: staticBaseEndpoints{defaultBaseURL: "https://api.anthropic.com", chatPath: "/v1/messages"},
		// x-api-key + the required anthropic-version header on every request.
		auth: keyAuth{mode: authXAPIKey, extraHeaders: map[string]string{"anthropic-version": anthropicVersion}},
		// Anthropic's GET /v1/models returns the OpenAI {"data":[...]} shape.
		lister:      openAILister{chatPath: "/v1/messages", modelsPath: "/v1/models"},
		derivesBase: true, // base URL is api.anthropic.com; an empty endpoint is routable
	})

	registerProvider(&provider{
		apiType: APITypeVertexAnthropic,
		adapter: anthropicAdapter{vertex: true},
		// Claude on Vertex exposes extended thinking (emitted as adaptive thinking).
		// reasoning_effort is not an Anthropic body param and there is no
		// response_format field, so both stay off. Cache pricing/control mirror the
		// direct Anthropic API (same Messages wire format).
		caps: Capabilities{
			SupportsThinking:     true,
			CacheControl:         CacheControlBreakpoints,
			CacheReadMultiplier:  0.10,
			CacheWriteMultiplier: 1.25,
		},
		endpoints: modelURLEndpoints{
			baseURLFunc:   vertexNativeBaseURL,
			chatURLFunc:   vertexAnthropicChatURL,
			streamURLFunc: vertexAnthropicStreamURL,
		},
		auth:        adcAuth{},
		lister:      vertexPublisherLister{publisher: "anthropic", format: bareModelID, keep: hasPrefixAny("claude")},
		validate:    vertexValidate,
		derivesBase: true, // base URL is derived from project/location; an empty endpoint is routable
	})
}
