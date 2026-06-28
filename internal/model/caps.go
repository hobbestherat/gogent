package model

import "strings"

// This file adds the PER-MODEL capability axis that complements the per-provider
// Capabilities struct (see provider.go). Capabilities is one value per api_type,
// read uniformly by buildRequest; it cannot express a quirk that varies by model
// WITHIN a provider (e.g. current-gen Claude rejecting temperature while older
// Claude on the same API accepts it). ModelCaps fills exactly that gap.
//
// Capability data is keyed on (provider × model), mirroring the models.dev
// structure data[provider].models[model]: the same model id can carry different
// capability signatures across providers, so model alone is not a safe key.
//
// resolveModelCaps is the single seam buildRequest consults. The ZERO value of
// ModelCaps means "no known quirk — inherit every default", so a model with no
// matching entry resolves to ModelCaps{} and behaves byte-identically to before
// this layer existed (the no-regression invariant).

// ModelCaps reports per-(provider,model) wire quirks that the per-provider
// Capabilities struct cannot express because they differ by model within one
// provider. The zero value is "inherit everything"; only set fields override a
// default.
type ModelCaps struct {
	// RejectsSampling drops temperature AND top_p from the outbound request.
	// Current-gen Claude rejects the mere presence of either parameter with a 400
	// ("temperature is deprecated for this model"), even temperature:0, and
	// independent of whether reasoning is enabled. buildRequest reads this to omit
	// the sampling pointers entirely.
	RejectsSampling bool
}

// resolveModelCaps returns the capability overrides for a (provider, model) pair.
//
// Resolution is TIERED, authoritative -> default and most-specific -> least:
//  1. the curated override table (model_overrides.go), the authoritative tier,
//     matched (provider,model) exact, then (provider,*) provider-wildcard, then
//     (*,model) model-only (which applies across providers);
//  2. (future, step 3) the models.dev catalog booleans as the default tier;
//  3. an empty ModelCaps when nothing matches — inherit every default.
//
// Step 1 of issue #543 wires only tier 1; the catalog default tier is a separate,
// additive follow-up. The model id is normalized to its base (a dated/pinned
// snapshot like "claude-opus-4-5@20251101" matches its family row).
func resolveModelCaps(apiType APIType, model string) ModelCaps {
	base := baseModelID(model)

	// Tier 1: curated overrides, most specific first.
	if mc, ok := findOverride(func(o modelOverride) bool {
		return o.provider == apiType && o.model == base
	}); ok {
		return mc
	}
	if mc, ok := findOverride(func(o modelOverride) bool {
		return o.provider == apiType && o.model == ""
	}); ok {
		return mc
	}
	if mc, ok := findOverride(func(o modelOverride) bool {
		return o.provider == "" && o.model == base
	}); ok {
		return mc
	}

	// Tier 2 (models.dev catalog defaults) is intentionally not wired in step 1.
	// Tier 3: nothing matched — inherit every default.
	return ModelCaps{}
}

// baseModelID strips a dated/pinned snapshot suffix ("@20251101") from a model id
// so a pinned snapshot inherits its family's capability row. Surrounding
// whitespace is trimmed; everything else is left untouched.
func baseModelID(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.IndexByte(m, '@'); i >= 0 {
		return m[:i]
	}
	return m
}
