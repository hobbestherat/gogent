package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTokenUsageUnmarshalCachedTokens covers the provider-specific shapes that
// report prompt-cache hits, plus our own round-trip tag.
func TestTokenUsageUnmarshalCachedTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "openai/z.ai nested prompt_tokens_details",
			in:   `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}`,
			want: 80,
		},
		{
			name: "deepseek-style top-level prompt_cache_hit_tokens",
			in:   `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":64}`,
			want: 64,
		},
		{
			name: "nested takes precedence over top-level hit count",
			in:   `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":80},"prompt_cache_hit_tokens":10}`,
			want: 80,
		},
		{
			name: "no cache fields",
			in:   `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`,
			want: 0,
		},
		{
			name: "zero nested cached_tokens stays zero",
			in:   `{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0}}`,
			want: 0,
		},
		{
			name: "round-trips through own cached_tokens tag",
			in:   `{"prompt_tokens":100,"cached_tokens":42}`,
			want: 42,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var u TokenUsage
			if err := json.Unmarshal([]byte(tc.in), &u); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if u.CachedTokens != tc.want {
				t.Errorf("CachedTokens = %d, want %d", u.CachedTokens, tc.want)
			}
			if u.PromptTokens != 100 {
				t.Errorf("PromptTokens = %d, want 100 (other fields must still decode)", u.PromptTokens)
			}
		})
	}
}

// TestTokenUsageMarshalRoundTrip ensures CachedTokens survives gogent's own
// persistence (marshal -> unmarshal), since usage is carried in stored turns.
func TestTokenUsageMarshalRoundTrip(t *testing.T) {
	orig := TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedTokens: 80}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got TokenUsage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != orig {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, orig)
	}
}

// TestCompleteAccumulatesCachedTokens verifies the connection threads a
// provider's cached-token count into ModelStats / the snapshot.
func TestCompleteAccumulatesCachedTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Emit the nested OpenAI/Z.AI shape directly so we also exercise the wire
		// decode path (not just struct literals).
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop","index":0}],
			"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}
		}`))
	}))
	defer server.Close()

	c := NewModelConnection()
	c.SetURL(server.URL)

	resp, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage == nil || resp.Usage.CachedTokens != 80 {
		t.Fatalf("response cached tokens = %v, want 80", resp.Usage)
	}

	snap := c.StatsSnapshot()
	if snap.TotalTokensIn != 100 {
		t.Errorf("TotalTokensIn = %d, want 100", snap.TotalTokensIn)
	}
	if snap.TotalCachedTokensIn != 80 {
		t.Errorf("TotalCachedTokensIn = %d, want 80", snap.TotalCachedTokensIn)
	}
}
