package model

import (
	"testing"

	"gogent/internal/config"
)

func TestNormalizeModelID(t *testing.T) {
	cases := []struct {
		apiType APIType
		id      string
		want    string
	}{
		{APITypeOpenRouter, "z-ai/glm-5.2", "glm-5.2"},
		{APITypeOpenRouter, "anthropic/claude-sonnet-5:free", "claude-sonnet-5"},
		{APITypeVertex, "google/gemini-3.5-flash", "gemini-3.5-flash"},
		{APITypeVertexNative, "gemini-3.5-flash", "gemini-3.5-flash"},
		{APITypeVertexAnthropic, "claude-3-5-haiku@20241022", "claude-3-5-haiku"},
		{APITypeZAI, "GLM-4.6", "glm-4.6"},
		{APITypeAnthropic, "  claude-opus-4-8  ", "claude-opus-4-8"},
	}
	for _, tc := range cases {
		if got := NormalizeModelID(tc.apiType, tc.id); got != tc.want {
			t.Errorf("NormalizeModelID(%s, %q) = %q, want %q", tc.apiType, tc.id, got, tc.want)
		}
	}
}

func TestFamilyKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"llama-3.3-70b-instruct-turbo", "llama-3.3-70b"},
		{"llama-3.3-70b-versatile", "llama-3.3-70b"},
		{"gemini-2.5-flash", "gemini-2.5"}, // trailing non-version qualifier dropped
		{"deepseek-chat", "deepseek-chat"}, // no version token → unchanged
	}
	for _, tc := range cases {
		if got := FamilyKey(tc.in); got != tc.want {
			t.Errorf("FamilyKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMergeCapsPrecedence(t *testing.T) {
	live := &config.ModelCapabilities{ContextWindow: 200000, MaxOutput: 64000, Vision: true}
	catalog := &config.ModelCapabilities{
		ContextWindow:  1000000, // overridden by live
		MaxOutput:      8192,    // overridden by live
		ToolCall:       true,    // filled from catalog
		InputCostPerM:  5,       // filled from catalog
		OutputCostPerM: 25,
	}

	// live ▸ catalog: live numerics win, catalog fills gaps, booleans OR.
	got := MergeCaps(live, catalog)
	if got.ContextWindow != 200000 || got.MaxOutput != 64000 {
		t.Errorf("live numerics should win: %+v", got)
	}
	if !got.Vision || !got.ToolCall {
		t.Errorf("booleans should OR: %+v", got)
	}
	if got.InputCostPerM != 5 || got.OutputCostPerM != 25 {
		t.Errorf("catalog pricing should fill: %+v", got)
	}
	if got.Source != SourceMerged {
		t.Errorf("Source = %q, want merged", got.Source)
	}

	// catalog only.
	if got := MergeCaps(nil, catalog); got.Source != SourceCatalog {
		t.Errorf("nil live → Source %q, want catalog", got.Source)
	}
	// neither.
	if got := MergeCaps(nil, nil); got.Source != SourceManual {
		t.Errorf("nil/nil → Source %q, want manual", got.Source)
	}
}

// stubLookup is an in-memory CatalogLookup for MergeDiscovery tests.
type stubLookup struct {
	exact  map[string]config.ModelCapabilities
	family map[string]config.ModelCapabilities
	all    []CatalogEntry
}

func (s stubLookup) Exact(_ APIType, norm string) (config.ModelCapabilities, bool) {
	c, ok := s.exact[norm]
	return c, ok
}
func (s stubLookup) Family(_ APIType, fam string) (config.ModelCapabilities, bool) {
	c, ok := s.family[fam]
	return c, ok
}
func (s stubLookup) All(_ APIType) []CatalogEntry { return s.all }

func TestMergeDiscoveryAvailabilityAndCatalogOnly(t *testing.T) {
	live := []ModelInfo{{ID: "glm-5.2"}, {ID: "glm-4.6"}}
	cat := stubLookup{
		exact: map[string]config.ModelCapabilities{
			"glm-5.2": {ContextWindow: 1000000, Source: "catalog"},
			"glm-9":   {ContextWindow: 123, Source: "catalog"}, // catalog-only
		},
		all: []CatalogEntry{
			{ID: "glm-5.2", NormID: "glm-5.2", Caps: config.ModelCapabilities{ContextWindow: 1000000}},
			{ID: "glm-9", NormID: "glm-9", Caps: config.ModelCapabilities{ContextWindow: 123}},
		},
	}

	got := MergeDiscovery(APITypeZAI, live, cat)

	byID := map[string]DiscoveredModel{}
	for _, d := range got {
		byID[d.ID] = d
	}
	// Live + catalog hit → available, in-catalog, caps merged.
	if d := byID["glm-5.2"]; !d.Available || !d.InCatalog || d.Caps.ContextWindow != 1000000 {
		t.Errorf("glm-5.2 = %+v, want available+catalog+ctx", d)
	}
	// Live without catalog → available, not in catalog.
	if d := byID["glm-4.6"]; !d.Available || d.InCatalog {
		t.Errorf("glm-4.6 = %+v, want available, not in catalog", d)
	}
	// Catalog-only → present but flagged unavailable.
	if d, ok := byID["glm-9"]; !ok || d.Available || !d.InCatalog {
		t.Errorf("glm-9 = %+v (ok=%v), want catalog-only unavailable", d, ok)
	}
}

func TestMergeDiscoveryNoCatalog(t *testing.T) {
	live := []ModelInfo{{ID: "local-model"}}
	got := MergeDiscovery(APITypeOpenAI, live, nil)
	if len(got) != 1 || !got[0].Available || got[0].InCatalog {
		t.Fatalf("no-catalog discovery = %+v, want one available non-catalog entry", got)
	}
	if got[0].Caps.Source != SourceManual {
		t.Errorf("no caps source = %q, want manual", got[0].Caps.Source)
	}
}
