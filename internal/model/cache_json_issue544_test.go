package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file guards the byte-identity and back-compat contract of TokenUsage's JSON
// (issue #544 gate 3): read-only turns marshal byte-identically to pre-#544, the
// legacy cached_tokens key still loads, the three read sources resolve with fixed
// precedence, and cache_write_tokens round-trips. These would silently break stored
// turns and external readers if MarshalJSON changed key order/omitempty.

// TestTokenUsageMarshalGoldenBytes pins EXACT marshaled bytes. A read-only turn must
// be byte-identical to gogent's pre-#544 reflection output; a write turn appends
// cache_write_tokens LAST without shifting any existing key. If the key order, a
// tag, or an omitempty changes, the persisted-bytes contract breaks and this fails.
func TestTokenUsageMarshalGoldenBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    TokenUsage
		want string
	}{
		{
			"read-only byte-identical to pre-544",
			TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Cache: CacheStats{ReadTokens: 80}},
			`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":80}`,
		},
		{
			"no cache at all",
			TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
			`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`,
		},
		{
			"read+reasoning keeps reasoning's slot after cached",
			TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Cache: CacheStats{ReadTokens: 80}, ReasoningTokens: 7},
			`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":80,"reasoning_tokens":7}`,
		},
		{
			"write turn appends cache_write_tokens last",
			TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Cache: CacheStats{ReadTokens: 80, WriteTokens: 5}},
			`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":80,"cache_write_tokens":5}`,
		},
		{
			"write turn with reasoning: cached, reasoning, cache_write order",
			TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Cache: CacheStats{ReadTokens: 80, WriteTokens: 5}, ReasoningTokens: 7},
			`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cached_tokens":80,"reasoning_tokens":7,"cache_write_tokens":5}`,
		},
		{
			"write-only turn: cached_tokens omitted, cache_write present",
			TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Cache: CacheStats{WriteTokens: 5}},
			`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"cache_write_tokens":5}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.u)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("marshal bytes = %s\nwant             = %s", got, tc.want)
			}
		})
	}
}

// TestTokenUsageMarshalNoNestedCacheKey guards the flat-encoding decision: the
// Cache field's inner json tags must NEVER surface as a nested "cache" object or a
// "cache_read_tokens" key in TokenUsage's output — only the flat cached_tokens /
// cache_write_tokens keys. (The Cache field is json:"-" precisely to enforce this.)
func TestTokenUsageMarshalNoNestedCacheKey(t *testing.T) {
	b, err := json.Marshal(TokenUsage{PromptTokens: 1, Cache: CacheStats{ReadTokens: 2, WriteTokens: 3}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, bad := range []string{`"cache"`, "cache_read_tokens"} {
		if strings.Contains(s, bad) {
			t.Errorf("marshal produced forbidden key %q in %s (must stay flat)", bad, s)
		}
	}
}

// TestTokenUsageUnmarshalReadPrecedence pins the three-way READ resolution, most
// authoritative first: nested prompt_tokens_details.cached_tokens > top-level
// prompt_cache_hit_tokens > legacy top-level cached_tokens. A zero value at a higher
// tier falls through to the next.
func TestTokenUsageUnmarshalReadPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"nested beats hit beats legacy", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":80},"prompt_cache_hit_tokens":64,"cached_tokens":42}`, 80},
		{"hit beats legacy (no nested)", `{"prompt_tokens":100,"prompt_cache_hit_tokens":64,"cached_tokens":42}`, 64},
		{"legacy only", `{"prompt_tokens":100,"cached_tokens":42}`, 42},
		{"zero nested falls through to hit", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":64}`, 64},
		{"zero nested and zero hit fall through to legacy", `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":0,"cached_tokens":42}`, 42},
		{"no cache fields stays zero", `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u TokenUsage
			if err := json.Unmarshal([]byte(tc.in), &u); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if u.Cache.ReadTokens != tc.want {
				t.Errorf("Cache.ReadTokens = %d, want %d", u.Cache.ReadTokens, tc.want)
			}
		})
	}
}

// TestTokenUsageWriteRoundTripAndBackcompat covers the WRITE half and persistence:
// gogent's own cache_write_tokens key loads into Cache.WriteTokens, a legacy
// read-only turn still loads (write stays 0), and marshal→unmarshal preserves both
// halves exactly.
func TestTokenUsageWriteRoundTripAndBackcompat(t *testing.T) {
	t.Run("cache_write_tokens loads into WriteTokens", func(t *testing.T) {
		var u TokenUsage
		if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"cache_write_tokens":5}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.Cache.WriteTokens != 5 || u.Cache.ReadTokens != 0 {
			t.Errorf("Cache = %+v, want read=0 write=5", u.Cache)
		}
	})
	t.Run("legacy read-only turn loads with no write", func(t *testing.T) {
		var u TokenUsage
		if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"cached_tokens":42}`), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if u.Cache.ReadTokens != 42 || u.Cache.WriteTokens != 0 {
			t.Errorf("Cache = %+v, want read=42 write=0", u.Cache)
		}
	})
	t.Run("full read+write+reasoning round-trips through marshal", func(t *testing.T) {
		orig := TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
			Cache: CacheStats{ReadTokens: 80, WriteTokens: 5}, ReasoningTokens: 3}
		data, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got TokenUsage
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != orig {
			t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, orig)
		}
	})
}

// TestTokenUsageIsComparable pins that TokenUsage remains a comparable struct
// (CacheStats is two ints) so existing == / != struct-equality checks across the
// suite keep compiling and behaving. A non-comparable CacheStats would break this.
func TestTokenUsageIsComparable(t *testing.T) {
	a := TokenUsage{PromptTokens: 1, Cache: CacheStats{ReadTokens: 2, WriteTokens: 3}}
	b := TokenUsage{PromptTokens: 1, Cache: CacheStats{ReadTokens: 2, WriteTokens: 3}}
	c := TokenUsage{PromptTokens: 1, Cache: CacheStats{ReadTokens: 2, WriteTokens: 4}}
	if a != b {
		t.Error("equal TokenUsage values compared unequal")
	}
	if a == c {
		t.Error("TokenUsage values differing only in Cache.WriteTokens compared equal")
	}
}

// TestTokenUsageUnmarshalMalformed ensures bad JSON errors out instead of silently
// producing a zero value that could mask a corrupt persisted turn.
func TestTokenUsageUnmarshalMalformed(t *testing.T) {
	var u TokenUsage
	if err := json.Unmarshal([]byte(`{not json`), &u); err == nil {
		t.Error("unmarshal of malformed JSON: want error, got nil")
	}
}
