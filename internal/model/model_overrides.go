package model

// This file is the CURATED, authoritative tier of the per-model capability layer
// (see caps.go). Each row records a known wire quirk for a (provider, model) pair.
// It is authoritative on purpose: the models.dev catalog carries a `temperature`
// bool but no top_p, no deprecation status, and is stale on fresh deprecations
// (its claude-opus-4-1 entry still says temperature:true), and no vendor publishes
// a machine-readable per-parameter accepted/rejected schema. So a confirmed quirk
// is recorded here as one data row rather than smuggled into an adapter branch or
// fused to a config boolean.
//
// Adding a quirk is a one-line edit here — that is the whole point of the seam.

// modelOverride is one row of the curated override table. An empty provider
// matches ANY provider (the model-only tier, applied across providers); an empty
// model matches ANY model on the given provider (the provider-wildcard tier). At
// least one of provider/model is set in every real row. See resolveModelCaps for
// match precedence.
type modelOverride struct {
	provider APIType
	model    string
	caps     ModelCaps
}

// modelOverrides is the authoritative override table. Order does not matter:
// resolveModelCaps selects by tier (exact, then provider-wildcard, then
// model-only), not by table position.
var modelOverrides = []modelOverride{
	// --- Current-gen Claude rejects sampling params (temperature/top_p) ---
	// The field's mere presence is a 400 ("temperature is deprecated for this
	// model"), even temperature:0, regardless of reasoning. These are MODEL-ONLY
	// rows (empty provider), so they apply across BOTH the direct Anthropic API
	// and Claude-on-Vertex — giving identical behavior for the same model on
	// either path (issue #543). claude-opus-4-8 is confirmed live; its current-gen
	// siblings share the deprecation. Add a row here as each new deprecation is
	// confirmed; this curated list is the source of truth.
	{model: "claude-opus-4-8", caps: ModelCaps{RejectsSampling: true}},
	{model: "claude-opus-4-5", caps: ModelCaps{RejectsSampling: true}},
	{model: "claude-opus-4-1", caps: ModelCaps{RejectsSampling: true}},
	{model: "claude-sonnet-4-5", caps: ModelCaps{RejectsSampling: true}},
	{model: "claude-haiku-4-5", caps: ModelCaps{RejectsSampling: true}},

	// Provider-wildcard: every Claude served over Vertex drops sampling params.
	// This reproduces the former `if a.vertex { /* drop temperature/top_p */ }`
	// branch in anthropicAdapter.buildBody exactly, so no Vertex model regresses
	// regardless of generation — the decision now lives here as data rather than
	// in the adapter.
	{provider: APITypeVertexAnthropic, caps: ModelCaps{RejectsSampling: true}},

	// --- DeepSeek cache discount (issue #544) ---
	// DeepSeek is not a distinct api_type: it is reached as a base-URL config on
	// api_type "openai" and so shares OpenAI's Capabilities (cache read 0.5x). But
	// DeepSeek's cache-hit price is far deeper (~0.1x of input). These exact
	// (provider,model) rows override just the read multiplier for the two DeepSeek
	// chat models, leaving everything else (incl. sampling) at the OpenAI default.
	// DeepSeek reports no cache-write count, so WriteMultiplier stays inherited.
	{provider: APITypeOpenAI, model: "deepseek-chat", caps: ModelCaps{CacheReadMultiplier: ptr(0.10)}},
	{provider: APITypeOpenAI, model: "deepseek-reasoner", caps: ModelCaps{CacheReadMultiplier: ptr(0.10)}},
}

// ptr returns a pointer to v, for the optional (nil = inherit) multiplier fields
// on ModelCaps.
func ptr[T any](v T) *T { return &v }

// findOverride returns the caps of the first override row satisfying match, and
// whether one was found. resolveModelCaps calls it once per tier with a
// tier-specific predicate.
func findOverride(match func(modelOverride) bool) (ModelCaps, bool) {
	for _, o := range modelOverrides {
		if match(o) {
			return o.caps, true
		}
	}
	return ModelCaps{}, false
}
