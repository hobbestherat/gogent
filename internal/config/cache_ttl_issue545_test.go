package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAnthropicCacheTTLNormalization pins ModelConfig.AnthropicCacheTTL's
// fail-safe normalization (issue #545 Gap B): only "1h" selects the 1-hour cache;
// "off"/"none"/"disabled" disable; everything else — including the empty default,
// an explicit "5m", and any typo — collapses to the 5-minute default ("") so a
// misconfiguration never silently disables caching AND never emits an invalid ttl
// that would 400 the request.
func TestAnthropicCacheTTLNormalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},       // unset → default 5m
		{"5m", ""},     // explicit 5m is the default → no ttl emitted
		{"1h", "1h"},   // 1-hour cache
		{"1H", "1h"},   // case-insensitive
		{" 1h ", "1h"}, // surrounding whitespace tolerated
		{"off", "off"},
		{"OFF", "off"}, // case-insensitive disable
		{"none", "off"},
		{"disabled", "off"},
		{"2h", ""},      // unrecognized → default (NOT emitted, would 400)
		{"30m", ""},     // unrecognized → default
		{"1 hour", ""},  // unrecognized → default
		{"true", ""},    // not a disable token → default (keeps caching)
		{"0", ""},       // not a disable token → default
		{"garbage", ""}, // typo → default
	}
	for _, tc := range cases {
		m := &ModelConfig{CacheTTL: tc.in}
		if got := m.AnthropicCacheTTL(); got != tc.want {
			t.Errorf("AnthropicCacheTTL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAnthropicCacheTTLNilSafe ensures a nil *ModelConfig (e.g. a connection with
// no config) does not panic and resolves to the 5-minute default — buildRequest
// calls c.Config.AnthropicCacheTTL() unconditionally, so the method must be
// nil-safe (mirrors ContextWindowOrDefault).
func TestAnthropicCacheTTLNilSafe(t *testing.T) {
	var m *ModelConfig
	if got := m.AnthropicCacheTTL(); got != "" {
		t.Errorf("nil AnthropicCacheTTL = %q, want \"\" (default 5m, no panic)", got)
	}
}

// TestModelConfigCacheTTLRoundTrip confirms the new field survives a JSON
// marshal/unmarshal cycle, that a legacy config blob with no "cache_ttl" key loads
// as the zero value (backward compat), and that the field name is the documented
// "cache_ttl".
func TestModelConfigCacheTTLRoundTrip(t *testing.T) {
	for _, ttl := range []string{"1h", "off", ""} {
		in := &ModelConfig{Name: "m", CacheTTL: ttl}
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal CacheTTL=%q: %v", ttl, err)
		}
		// The field serializes under the documented key.
		if ttl != "" && !strings.Contains(string(data), `"cache_ttl":`) {
			t.Errorf("CacheTTL=%q: body missing cache_ttl key: %s", ttl, data)
		}
		var out ModelConfig
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("unmarshal CacheTTL=%q: %v", ttl, err)
		}
		if out.CacheTTL != ttl {
			t.Errorf("round-trip CacheTTL=%q came back as %q", ttl, out.CacheTTL)
		}
	}

	// A legacy config blob with no "cache_ttl" key loads as the zero value, so an
	// existing config.json is unaffected.
	const legacy = `{"name":"x"}`
	var legacyCfg ModelConfig
	if err := json.Unmarshal([]byte(legacy), &legacyCfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacyCfg.CacheTTL != "" {
		t.Errorf("legacy config should leave CacheTTL empty, got %q", legacyCfg.CacheTTL)
	}
}

// TestDefaultConfigModelsHaveNoCacheTTL guards against a shipped default model
// accidentally opting into 1h or disabling caching — the default for every model
// must be the 5-minute ephemeral cache (issue #545: 5m stays the default).
func TestDefaultConfigModelsHaveNoCacheTTL(t *testing.T) {
	cfg := GetDefaultConfig()
	for _, m := range cfg.ModelConfigs {
		if m.CacheTTL != "" {
			t.Errorf("default model %q has CacheTTL=%q; default must stay empty (5m)", m.Name, m.CacheTTL)
		}
		if ttl := m.AnthropicCacheTTL(); ttl != "" {
			t.Errorf("default model %q resolves to CacheTTL=%q; want \"\"", m.Name, ttl)
		}
	}
}

// TestAnthropicCacheTTLUnrecognizedNeverDisablesCaches is the core fail-safe
// property: a typo must NOT disable caching (it must fall back to the default 5m,
// not "off"), because silently turning caching off would be a silent cost
// regression in the opposite direction.
func TestAnthropicCacheTTLUnrecognizedNeverDisablesCaches(t *testing.T) {
	// These are genuinely unrecognized (not off/none/disabled, even after
	// trim+lowercase), so they must fall back to the 5m default rather than
	// silently turning caching off. ("Off " is intentionally excluded — trailing
	// whitespace is trimmed and legitimately disables.)
	for _, typo := range []string{"disable", "offf", "no", "false", "cachoff", "0", "null"} {
		m := &ModelConfig{CacheTTL: typo}
		if got := m.AnthropicCacheTTL(); got == "off" {
			t.Errorf("typo %q resolved to %q (disabled caching); unrecognized values must fall back to the 5m default, not disable", typo, got)
		}
	}
}
