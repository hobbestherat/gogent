package model

// Read-only indicators the catalog review form ("Add from Catalog…", issue #542)
// uses to explain catalog-derived facts to the user. They expose information the
// provider registry already owns — the base URL a derive-base provider resolves to
// and whether a provider actually emits the `thinking` parameter — so the TUI can
// surface provenance without duplicating routing literals or guessing at wire
// behaviour. Both are pure reads of the registry; neither affects routing.

// ResolvedBaseURL reports the request base URL a provider uses when the config
// leaves Endpoint empty.
//
//   - For the static-base derive-base providers (anthropic/zai/openrouter) it
//     returns their built-in default base (fromProjectLocation=false), so the form
//     can show "(derived: https://api.anthropic.com)".
//   - For the vertex* family it returns ("", true): the base is built from
//     Project+Location and so is unknown until the user enters them, so the form
//     shows "(derived from Project + Location)".
//   - For NON-derive-base providers (the generic "openai" and unknown gateways) it
//     returns ("", false). The caller uses the catalog's p.API for those instead.
//
// The derivesBase guard is essential and comes first: the generic "openai"
// provider is itself a staticBaseEndpoints with a localhost placeholder default
// (derivesBase=false), so a bare type-switch would leak that placeholder. Only the
// derive-base providers reach the resolver type-switch. This reuses the registry's
// defaultBaseURL literals, so the indicator can never drift from real routing.
func ResolvedBaseURL(apiType APIType) (base string, fromProjectLocation bool) {
	p := providerFor(apiType)
	if !p.derivesBase {
		return "", false
	}
	switch e := p.endpoints.(type) {
	case staticBaseEndpoints:
		if e.defaultBaseURL != "" {
			return e.defaultBaseURL, false // anthropic / zai / openrouter
		}
		return "", true // vertex OpenAI-compat shim (base from project/location)
	case modelURLEndpoints:
		return "", true // vertex native / vertex-anthropic (base from project/location)
	}
	return "", false
}

// SupportsThinking reports whether the provider actually emits the `thinking`
// request parameter (its caps.SupportsThinking). The review form ANDs this with
// the catalog's per-model thinking toggle so it never claims the Thinking selector
// is meaningful for a provider that silently drops the parameter (e.g. the direct
// Anthropic Messages API, whose caps leave SupportsThinking unset).
func SupportsThinking(apiType APIType) bool {
	return providerFor(apiType).caps.SupportsThinking
}
