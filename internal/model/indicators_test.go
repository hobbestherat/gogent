package model

import (
	"strings"
	"testing"

	"gogent/internal/config"
)

// TestResolvedBaseURL is the read-only helper the catalog review form (issue #542)
// uses to render a derive-base provider's resolved Endpoint as a read-only
// indicator. It pins every api_type's (base, fromProjectLocation) so the indicator
// can never drift from the real routing the adapter performs.
//
// The derivesBase guard is the load-bearing case: the generic "openai" provider is
// itself a staticBaseEndpoints with a localhost placeholder default and
// derivesBase:false, so a bare type-switch would leak "http://localhost:8080/v1" for
// gateways. That regression is asserted explicitly below.
func TestResolvedBaseURL(t *testing.T) {
	cases := []struct {
		name            string
		apiType         APIType
		wantBase        string
		wantFromProjLoc bool
	}{
		// Static-base derive-base providers: the form shows "(derived: <base>)".
		{"anthropic", APITypeAnthropic, "https://api.anthropic.com", false},
		{"zai", APITypeZAI, "https://api.z.ai/api/paas/v4", false},
		{"openrouter", APITypeOpenRouter, "https://openrouter.ai/api/v1", false},
		// vertex* build the base from project/location: "(derived from Project + Location)".
		{"vertex", APITypeVertex, "", true},
		{"vertex-native", APITypeVertexNative, "", true},
		{"vertex-anthropic", APITypeVertexAnthropic, "", true},
		// NON-derive-base: the caller uses p.API. The guard is mandatory here.
		{"openai does not leak the localhost placeholder", APITypeOpenAI, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBase, gotFromProjLoc := ResolvedBaseURL(tc.apiType)
			if gotBase != tc.wantBase || gotFromProjLoc != tc.wantFromProjLoc {
				t.Errorf("ResolvedBaseURL(%q) = (%q, %v), want (%q, %v)",
					tc.apiType, gotBase, gotFromProjLoc, tc.wantBase, tc.wantFromProjLoc)
			}
		})
	}
}

// TestResolvedBaseURLOpenAINoLeak is the dedicated regression for the round-1 design
// defect: without the derivesBase guard, ResolvedBaseURL("openai") would return the
// generic OpenAI provider's localhost placeholder default and the review form would
// render "(derived: http://localhost:8080/v1)" for a Groq/Together/DeepSeek gateway.
// The base MUST be empty so the gateway keeps its unchanged p.API editable box.
func TestResolvedBaseURLOpenAINoLeak(t *testing.T) {
	base, fromProjLoc := ResolvedBaseURL(APITypeOpenAI)
	if base != "" {
		t.Errorf("ResolvedBaseURL(openai) base = %q, want empty (the localhost placeholder must not leak to the gateway indicator)", base)
	}
	if fromProjLoc {
		t.Errorf("ResolvedBaseURL(openai) fromProjectLocation = true, want false")
	}
	// An unrecognized api_type resolves to the OpenAI provider (providerFor fallback),
	// so it must behave identically — never leaking a base.
	unknownBase, unknownFromProjLoc := ResolvedBaseURL(APIType("totally-unknown"))
	if unknownBase != "" || unknownFromProjLoc {
		t.Errorf("ResolvedBaseURL(unknown) = (%q, %v), want (%q, false) — unknown must fall through to the openai gateway behaviour",
			unknownBase, unknownFromProjLoc, "")
	}
}

// TestResolvedBaseURLMatchesRouting ensures the indicator's base is exactly the base
// the adapter actually requests when Endpoint is empty — the single-source-of-truth
// guarantee. A drift would mean the form tells the user a different URL than the one
// their requests use.
func TestResolvedBaseURLMatchesRouting(t *testing.T) {
	for _, apiType := range []APIType{APITypeAnthropic, APITypeZAI, APITypeOpenRouter} {
		base, fromProjLoc := ResolvedBaseURL(apiType)
		if fromProjLoc {
			t.Errorf("%s: fromProjectLocation=true, want false for a static-base provider", apiType)
			continue
		}
		// A derive-base config with an empty endpoint resolves to <base> + chatPath.
		// The indicator base must be a prefix of (or equal to the stripped) chat URL.
		cfg := &config.ModelConfig{Name: "t", APIType: string(apiType), Model: "m", Endpoint: ""}
		conn := NewModelConnectionFromConfig(cfg)
		if conn.URL == "" {
			t.Errorf("%s: resolved chat URL is empty", apiType)
			continue
		}
		// The base the indicator shows must be the prefix the chat URL is built from.
		if !strings.HasPrefix(conn.URL, base) {
			t.Errorf("%s: indicator base %q is not the prefix of the routed chat URL %q (indicator drifted from routing)",
				apiType, base, conn.URL)
		}
	}
}

// TestDerivesBase tests the PUBLIC DerivesBase helper the review form branches on
// (distinct from the registry-field guard in routable_config_validation_test.go,
// which catches provider-registration drift). It also locks the contract DerivesBase
// forms with ResolvedBaseURL — the two helpers the dialog composes — so the dialog's
// hint switch always has a real case for a derive-base provider.
func TestDerivesBase(t *testing.T) {
	for _, tc := range []struct {
		apiType APIType
		want    bool
	}{
		{APITypeOpenAI, false}, // the only placeholder-default provider
		{APITypeZAI, true},
		{APITypeOpenRouter, true},
		{APITypeAnthropic, true},
		{APITypeVertex, true},
		{APITypeVertexNative, true},
		{APITypeVertexAnthropic, true},
		// Unknown resolves to the OpenAI provider (providerFor fallback) => not derive-base.
		{APIType("totally-unknown"), false},
	} {
		t.Run(string(tc.apiType), func(t *testing.T) {
			if got := DerivesBase(tc.apiType); got != tc.want {
				t.Errorf("DerivesBase(%q) = %v, want %v", tc.apiType, got, tc.want)
			}
			// Contract with ResolvedBaseURL: a derive-base provider must yield a hint
			// the review form can render (a named base OR project/location), never the
			// ("", false) pair that means "use p.API". When NOT derive-base, it must
			// return ("", false) so the form treats it as a gateway.
			base, fromProjLoc := ResolvedBaseURL(tc.apiType)
			switch {
			case tc.want && base == "" && !fromProjLoc:
				t.Errorf("DerivesBase(%q)=true but ResolvedBaseURL=(%q,false): the review form's hint switch has no real case (the unreachable '(derived by the adapter)' default would fire)", tc.apiType, base)
			case !tc.want && (base != "" || fromProjLoc):
				t.Errorf("DerivesBase(%q)=false but ResolvedBaseURL=(%q,%v): a non-derive-base provider must look like a gateway to the form", tc.apiType, base, fromProjLoc)
			}
		})
	}
}

// TestSupportsThinking pins which api_types actually emit the `thinking` request
// parameter, so the review form's Thinking annotation is truthful. The direct
// Anthropic Messages API has SupportsThinking unset (caps:{}), so a Claude model
// with a catalog toggle is annotated "(no effect)" rather than "(supported)".
//
// Note vertex-native (native Gemini) DOES set SupportsThinking — the design's prose
// enumeration ("true for zai/vertex-anthropic") omitted it; this asserts the live
// cap so it is covered.
func TestSupportsThinking(t *testing.T) {
	cases := []struct {
		apiType APIType
		want    bool
	}{
		{APITypeAnthropic, false},      // direct Messages API: caps:{} — drops thinking
		{APITypeOpenRouter, false},     // OpenAI-compat gateway, no thinking toggle
		{APITypeOpenAI, false},         // generic openai: reasoning_effort only, no thinking
		{APITypeVertex, false},         // Gemini via OpenAI shim: thinking not exposed
		{APITypeZAI, true},             // GLM exposes thinking:{type:enabled|disabled}
		{APITypeVertexAnthropic, true}, // Claude on Vertex: extended thinking
		{APITypeVertexNative, true},    // native Gemini: thinkingConfig
	}
	for _, tc := range cases {
		t.Run(string(tc.apiType), func(t *testing.T) {
			if got := SupportsThinking(tc.apiType); got != tc.want {
				t.Errorf("SupportsThinking(%q) = %v, want %v", tc.apiType, got, tc.want)
			}
		})
	}
}
