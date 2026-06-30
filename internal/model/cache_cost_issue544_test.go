package model

import (
	"testing"

	"gogent/internal/config"
)

// This file is the TESTER's adversarial suite for issue #544 (provider-agnostic
// prompt-cache reporting & cost model). It targets the four design gates:
// GOAL MATCH (lossless read/write; per-provider cost weighting; DeepSeek override;
// CacheControlKind declared), USABILITY (cost-accurate budget), NO REGRESSIONS
// (0⇒1.0 fallback ⇒ raw tokens; lossless adapters), HOLISTIC (per-model layer reuse).

// Compile-time guard: *ModelConnection must satisfy the optional budget capability
// the agent layer consults (mirrors MaxTokensReporter). A regression that drops the
// method or renames it fails the build here, not at runtime in the agent path.
var _ CacheCostReporter = (*ModelConnection)(nil)

// TestOrOne pins the 0⇒1.0 fallback — the no-regression contract that an unset
// cache multiplier prices tokens at face value.
func TestOrOne(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want float64
	}{
		{0, 1}, {0.0, 1}, {1, 1}, {0.5, 0.5}, {0.1, 0.1}, {0.25, 0.25}, {1.25, 1.25}, {2, 2},
	} {
		if got := orOne(tc.in); got != tc.want {
			t.Errorf("orOne(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestCostWeightedInputFormula pins the cost-weighting math directly on CacheStats:
// full-price remainder (prompt − reads − writes) plus reads at readMult plus writes
// at writeMult, rounded to the nearest whole token. 0 multiplier ⇒ 1.0 (face value).
func TestCostWeightedInputFormula(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cs        CacheStats
		prompt    int
		readMult  float64
		writeMult float64
		want      int
	}{
		{"anthropic read+write (0.1/1.25)", CacheStats{ReadTokens: 800, WriteTokens: 100}, 1000, 0.1, 1.25, 305},
		{"openai read only (0.5)", CacheStats{ReadTokens: 500}, 1000, 0.5, 0, 750},
		{"deepseek read (0.1)", CacheStats{ReadTokens: 500}, 1000, 0.1, 0, 550},
		{"no multipliers equals raw prompt", CacheStats{ReadTokens: 800, WriteTokens: 100}, 1000, 0, 0, 1000},
		{"zero cache equals prompt", CacheStats{}, 1000, 0.5, 1.25, 1000},
		{"rounds to nearest token", CacheStats{ReadTokens: 7}, 10, 0.1, 0, 4}, // base 3 + 0.7 = 3.7 → 4
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.costWeightedInput(tc.prompt, tc.readMult, tc.writeMult); got != tc.want {
				t.Errorf("costWeightedInput(prompt=%d, read=%v, write=%v) = %d, want %d",
					tc.prompt, tc.readMult, tc.writeMult, got, tc.want)
			}
		})
	}
}

// TestCostWeightedInputFlooredAtZero pins the defensive floor (math.Max(0, …)) that
// stops a malformed/gateway response over-reporting cached tokens (Read+Write >
// Prompt) from producing a NEGATIVE cost and rewinding the agent budget. With
// face-value (1.0) multipliers the sum always equals prompt, so the floor only
// bites when a <1 discount is applied to over-reported reads — exactly the cases
// below. Removing the floor would let these return negatives.
func TestCostWeightedInputFlooredAtZero(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cs        CacheStats
		prompt    int
		readMult  float64
		writeMult float64
	}{
		{"over-reported reads, openai 0.5 (would be -50)", CacheStats{ReadTokens: 300}, 100, 0.5, 0},
		{"over-reported read+write, anthropic 0.1/1.25 (would be -300)", CacheStats{ReadTokens: 500, WriteTokens: 200}, 100, 0.1, 1.25},
		{"boundary: 2x reads at 0.5 lands exactly 0", CacheStats{ReadTokens: 200}, 100, 0.5, 0},
		{"deepseek 0.1 with read>prompt (would be -80)", CacheStats{ReadTokens: 200}, 100, 0.1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cs.costWeightedInput(tc.prompt, tc.readMult, tc.writeMult)
			if got < 0 {
				t.Errorf("costWeightedInput = %d, want >= 0 (floored; over-report must not rewind budget)", got)
			}
			if tc.name == "boundary: 2x reads at 0.5 lands exactly 0" && got != 0 {
				t.Errorf("costWeightedInput = %d, want exactly 0 (genuine zero must be preserved, not just negatives clamped)", got)
			}
		})
	}
	// Sweep: the floor must hold across a range of over-reports and discounts.
	for _, readMult := range []float64{0.1, 0.2, 0.25, 0.5} {
		cs := CacheStats{ReadTokens: 10_000} // far exceeds any prompt below
		for _, prompt := range []int{0, 1, 100, 1000} {
			if got := cs.costWeightedInput(prompt, readMult, 0); got < 0 {
				t.Errorf("costWeightedInput(prompt=%d, read=%d, mult=%v) = %d, want >= 0", prompt, cs.ReadTokens, readMult, got)
			}
		}
	}
}

// TestCostWeightedInputPerProvider drives the FULL two-axis resolution through a
// real *ModelConnection built from config: Capabilities (per api_type) overridden
// by ModelQuirks (per provider×model). This is where a mis-set multiplier, a missing
// DeepSeek row, or a broken resolveModelQuirks wiring would surface.
func TestCostWeightedInputPerProvider(t *testing.T) {
	// usage: prompt 1000, 500 cache reads, 100 cache writes (Anthropic-style).
	usage := TokenUsage{PromptTokens: 1000, Cache: CacheStats{ReadTokens: 500, WriteTokens: 100}}
	for _, tc := range []struct {
		name    string
		apiType string
		model   string
		want    int
	}{
		// base = 1000 − 500 − 100 = 400 in every case.
		{"anthropic read0.10 write1.25", "anthropic", "claude-opus-4-8", 575}, // 400 + 50 + 125
		{"vertex-anthropic same as anthropic", "vertex-anthropic", "claude-opus-4-8", 575},
		{"openai gpt-4o read0.50", "openai", "gpt-4o", 750},                             // 400 + 250 + 100(write 1.0)
		{"deepseek-chat override beats openai default", "openai", "deepseek-chat", 550}, // 400 + 50 + 100
		{"deepseek-reasoner override", "openai", "deepseek-reasoner", 550},
		{"zai glm read0.20", "zai", "glm-4.6", 600}, // 400 + 100 + 100
		{"openrouter passthrough read1.0 (documented-inaccurate)", "openrouter", "anthropic/claude-opus-4-8", 1000},
		{"vertex-native gemini read0.25", "vertex-native", "gemini-2.5-pro", 625}, // 400 + 125 + 100
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewModelConnectionFromConfig(&config.ModelConfig{APIType: tc.apiType, Model: tc.model})
			if got := conn.CostWeightedInput(usage); got != tc.want {
				t.Errorf("CostWeightedInput(%s/%s) = %d, want %d", tc.apiType, tc.model, got, tc.want)
			}
		})
	}
}

// TestDeepSeekOverrideDistinctFromOpenAI isolates the per-model override: the SAME
// api_type ("openai") yields a deeper discount for DeepSeek models than for native
// OpenAI models. This is the exact case Capabilities alone cannot express.
func TestDeepSeekOverrideDistinctFromOpenAI(t *testing.T) {
	usage := TokenUsage{PromptTokens: 1000, Cache: CacheStats{ReadTokens: 500}}
	openai := NewModelConnectionFromConfig(&config.ModelConfig{APIType: "openai", Model: "gpt-4o"})
	deepseek := NewModelConnectionFromConfig(&config.ModelConfig{APIType: "openai", Model: "deepseek-chat"})
	gotO, gotD := openai.CostWeightedInput(usage), deepseek.CostWeightedInput(usage)
	if gotO != 750 {
		t.Errorf("gpt-4o = %d, want 750 (read 0.5)", gotO)
	}
	if gotD != 550 {
		t.Errorf("deepseek-chat = %d, want 550 (read 0.1 override)", gotD)
	}
	if gotD >= gotO {
		t.Errorf("deepseek discount (%d) must be deeper than openai (%d)", gotD, gotO)
	}
}

// TestCostWeightedInputProviderlessIsRawPrompt is the no-regression invariant for
// the budget path: a connection with NO provider (the zero-value *ModelConnection)
// prices everything at face value, so cost-weighting equals raw PromptTokens
// regardless of cache counts (empty caps ⇒ 0 ⇒ orOne ⇒ 1.0).
func TestCostWeightedInputProviderlessIsRawPrompt(t *testing.T) {
	conn := &ModelConnection{} // zero value: provider nil, no ModelQuirks row
	for _, prompt := range []int{0, 1, 999, 100000} {
		usage := TokenUsage{PromptTokens: prompt, Cache: CacheStats{ReadTokens: prompt / 2, WriteTokens: prompt / 4}}
		if got := conn.CostWeightedInput(usage); got != prompt {
			t.Errorf("provider-less CostWeightedInput(prompt=%d) = %d, want %d (raw, no weighting)", prompt, got, prompt)
		}
	}
}

// TestBareNewModelConnectionIsPricedAsOpenAI documents a real behavior the design's
// prose was loose about: NewModelConnection() is NOT provider-less — it wires the
// OpenAI provider (api_type openai, read mult 0.5). So a bare connection DOES
// discount cache reads, unlike a zero-value connection. No regression today (no test
// sends cache tokens through a bare connection's budget), but pinning it keeps the
// budget math honest if that ever changes.
func TestBareNewModelConnectionIsPricedAsOpenAI(t *testing.T) {
	conn := NewModelConnection()
	usage := TokenUsage{PromptTokens: 1000, Cache: CacheStats{ReadTokens: 500}}
	// OpenAI 0.5: (1000-500) + 500*0.5 = 750, NOT the raw 1000.
	if got := conn.CostWeightedInput(usage); got != 750 {
		t.Errorf("bare NewModelConnection CostWeightedInput = %d, want 750 (OpenAI 0.5, not raw)", got)
	}
}

// TestDeepSeekModelOverridesExist guards the model_overrides.go rows: both DeepSeek
// chat models carry a 0.10 read override and inherit the write multiplier (nil),
// while a native OpenAI model gets no override.
func TestDeepSeekModelOverridesExist(t *testing.T) {
	for _, m := range []string{"deepseek-chat", "deepseek-reasoner"} {
		mc := resolveModelQuirks(APITypeOpenAI, m)
		if mc.CacheReadMultiplier == nil || *mc.CacheReadMultiplier != 0.10 {
			t.Errorf("resolveModelQuirks(openai, %s) read = %v, want *0.10", m, mc.CacheReadMultiplier)
		}
		if mc.CacheWriteMultiplier != nil {
			t.Errorf("resolveModelQuirks(openai, %s) write = %v, want nil (inherit OpenAI default)", m, mc.CacheWriteMultiplier)
		}
	}
	if mc := resolveModelQuirks(APITypeOpenAI, "gpt-4o"); mc.CacheReadMultiplier != nil {
		t.Errorf("resolveModelQuirks(openai, gpt-4o) read = %v, want nil (no override for native OpenAI)", mc.CacheReadMultiplier)
	}
}

// TestCacheControlKindPerProvider pins the DECLARATION-only capability flag used by
// #545/#547. anthropic/vertex-anthropic → Breakpoints; vertex-native → CachedContent;
// every automatic-caching provider → None.
func TestCacheControlKindPerProvider(t *testing.T) {
	for _, tc := range []struct {
		apiType string
		want    CacheControlKind
	}{
		{"anthropic", CacheControlBreakpoints},
		{"vertex-anthropic", CacheControlBreakpoints},
		{"vertex-native", CacheControlCachedContent},
		{"openai", CacheControlNone},
		{"zai", CacheControlNone},
		{"openrouter", CacheControlNone},
		{"vertex", CacheControlNone},
	} {
		conn := NewModelConnectionFromConfig(&config.ModelConfig{APIType: tc.apiType, Model: "m"})
		if got := conn.caps().CacheControl; got != tc.want {
			t.Errorf("%s CacheControl = %v, want %v", tc.apiType, got, tc.want)
		}
	}
}

// TestAdaptersPopulateCacheSplit covers the two adapter parse sites: Anthropic
// retains BOTH tiers (read + the previously-discarded write), Gemini carries reads
// only. This is the lossless fix at the heart of #544.
func TestAdaptersPopulateCacheSplit(t *testing.T) {
	t.Run("anthropic retains write", func(t *testing.T) {
		got := (anthropicUsage{
			InputTokens: 100, CacheReadInputTokens: 50, CacheCreationInputTokens: 30, OutputTokens: 20,
		}).toTokenUsage(0)
		if got == nil {
			t.Fatal("toTokenUsage returned nil")
		}
		if got.Cache.ReadTokens != 50 || got.Cache.WriteTokens != 30 {
			t.Errorf("Cache = %+v, want read=50 write=30 (write no longer discarded)", got.Cache)
		}
		if got.PromptTokens != 180 { // 100 + 50 + 30
			t.Errorf("PromptTokens = %d, want 180 (sum of all three input counters)", got.PromptTokens)
		}
		if got.CachedTokens() != 50 {
			t.Errorf("CachedTokens() = %d, want 50 (back-compat alias of Cache.ReadTokens)", got.CachedTokens())
		}
	})
	t.Run("anthropic nil when nothing reported", func(t *testing.T) {
		if got := (anthropicUsage{}).toTokenUsage(0); got != nil {
			t.Errorf("empty anthropic usage = %+v, want nil", got)
		}
	})
	t.Run("gemini read only", func(t *testing.T) {
		got := (&geminiUsageMetadata{
			PromptTokenCount: 100, CandidatesTokenCount: 20, TotalTokenCount: 120, CachedContentTokenCount: 40,
		}).toTokenUsage()
		if got == nil {
			t.Fatal("toTokenUsage returned nil")
		}
		if got.Cache.ReadTokens != 40 || got.Cache.WriteTokens != 0 {
			t.Errorf("Cache = %+v, want read=40 write=0 (Gemini reports no writes)", got.Cache)
		}
	})
	t.Run("gemini nil pointer returns nil", func(t *testing.T) {
		if got := (*geminiUsageMetadata)(nil).toTokenUsage(); got != nil {
			t.Errorf("nil gemini metadata = %+v, want nil", got)
		}
	})
}
