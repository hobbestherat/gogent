package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Tests for the provider-agnostic error.reason extractor (issue #555). The whole
// point of the fix is that every provider nests the human rejection reason at the
// same JSON path — error.message — so a single stdlib decode handles all of them
// with no per-provider branching. These tests lock that property down per provider
// shape and exercise the fallback ladder + bounding helper that protects the
// transcript from a multi-KB body.

func TestExtractProviderMessage_AllProviderShapes(t *testing.T) {
	// Each fixture is a real-shaped error body from a provider gogent targets. A
	// single struct{ Error struct{ Message string } } must recover the reason from
	// every one — that is the "no per-provider branching" guarantee of the fix.
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "openai invalid_request_error",
			body: `{"error":{"message":"The model ` + "`gpt-4`" + ` does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			want: "The model `gpt-4` does not exist",
		},
		{
			name: "zai openai-compatible",
			body: `{"error":{"message":"Invalid authentication credentials","type":"invalid_request_error"}}`,
			want: "Invalid authentication credentials",
		},
		{
			name: "openrouter openai-compatible",
			body: `{"error":{"message":"No provider exists for the requested model","code":404}}`,
			want: "No provider exists for the requested model",
		},
		{
			// The headline case from the issue: an Anthropic over-limit max_tokens
			// rejection. The reason sits under an outer {"type":"error",...} wrapper
			// but is still at error.message — the single struct decodes it directly.
			name: "anthropic over-limit max_tokens",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 8192 is greater than the maximum allowed for claude-opus-4-8. Please reduce max_tokens and try again."}}`,
			want: "max_tokens: 8192 is greater than the maximum allowed for claude-opus-4-8. Please reduce max_tokens and try again.",
		},
		{
			// Vertex-Anthropic uses the identical wire shape to direct Anthropic.
			name: "vertex-anthropic same shape as anthropic",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate between user and assistant"}}`,
			want: "messages: roles must alternate between user and assistant",
		},
		{
			// Vertex native Gemini nests message under error alongside code/status.
			name: "vertex native gemini invalid argument",
			body: `{"error":{"code":400,"message":"Invalid value at 'contents[0].parts[0].text'","status":"INVALID_ARGUMENT"}}`,
			want: "Invalid value at 'contents[0].parts[0].text'",
		},
		{
			// Extra, unrelated top-level fields must not disturb the decode.
			name: "openai with extra top-level fields ignored",
			body: `{"error":{"message":"rate limited","type":"rate_limit_error"},"request_id":"req_abc","trace":"xyz"}`,
			want: "rate limited",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractProviderMessage(tt.body)
			if got != tt.want {
				t.Errorf("extractProviderMessage = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestExtractProviderMessage_FallbackLadder(t *testing.T) {
	// Off-spec gateways: the ladder picks the first non-empty signal, in priority
	// order, then gives up to the raw body. "first non-empty wins" is the contract
	// analyzeError depends on for its no-dangling-separator guarantee.
	tests := []struct {
		name string
		body string
		want string
	}{
		{"nested error.message beats top-level message (priority)",
			`{"message":"top-level reason","error":{"message":"nested reason"}}`, "nested reason"},
		{"top-level message when no error object",
			`{"message":"plain top-level reason"}`, "plain top-level reason"},
		{"error rendered as a bare JSON string",
			`{"error":"bare string reason"}`, "bare string reason"},
		{"non-JSON plain text surfaces raw",
			`upstream connect error`, "upstream connect error"},
		{"non-JSON html surfaces raw first line region",
			`<html><head><title>502</title></head>`, "<html><head><title>502</title></head>"},
		{"empty body yields empty (caller keeps status-only message)", ``, ""},
		{"whitespace-only body yields empty", "   \n\t\r  ", ""},
		{"leading/trailing whitespace around json is trimmed", "  \n {\"error\":{\"message\":\"x\"}} \n  ", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractProviderMessage(tt.body)
			if got != tt.want {
				t.Errorf("extractProviderMessage = %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestExtractProviderMessage_EmptySignalFallsBackToRaw pins the (minor, by-design)
// wart: when a JSON body is recognisable but carries no usable message, the helper
// falls through to the raw body so the user still sees *something*. These cases are
// documented here so a future change to the ladder is deliberate, not accidental.
func TestExtractProviderMessage_EmptySignalFallsBackToRaw(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty json object", `{}`, `{}`},
		{"error object with no message", `{"error":{"code":400}}`, `{"error":{"code":400}}`},
		{"explicitly-empty error.message", `{"error":{"message":""}}`, `{"error":{"message":""}}`},
		{"error.message is a number not a string", `{"error":{"message":123}}`, `{"error":{"message":123}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractProviderMessage(tt.body)
			if got != tt.want {
				t.Errorf("extractProviderMessage = %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestBoundedReason(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \t\n  ", ""},
		{"single short line unchanged", "all good", "all good"},
		{"leading/trailing space trimmed", "  reason  ", "reason"},
		{"newline splits to first line", "line one\nline two", "line one"},
		{"crlf splits to first line", "line one\r\nline two", "line one"},
		{"trailing space before newline trimmed", "x   \nrest", "x"},
		{"exactly cap runes: no ellipsis", strings.Repeat("a", modelErrReasonMaxRunes), strings.Repeat("a", modelErrReasonMaxRunes)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boundedReason(tt.in); got != tt.want {
				t.Errorf("boundedReason(%q) = %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestBoundedReason_OverlongRuneCap verifies the bound: a reason longer than
// modelErrReasonMaxRunes runes is truncated to exactly that many runes plus an
// ellipsis, and — critically for "rune-safe, no mid-codepoint split" — a
// multibyte (CJK) reason must remain valid UTF-8 after truncation.
func TestBoundedReason_OverlongRuneCap(t *testing.T) {
	t.Run("ascii overlong", func(t *testing.T) {
		in := strings.Repeat("a", modelErrReasonMaxRunes+5)
		got := boundedReason(in)
		want := strings.Repeat("a", modelErrReasonMaxRunes) + "…"
		if got != want {
			t.Errorf("boundedReason overlong ascii = %q (len runes %d)\nwant %q", got, runeLen(got), want)
		}
	})
	t.Run("multibyte overlong stays valid utf8 and rune-capped", func(t *testing.T) {
		// '世' is 3 bytes / 1 rune. 350 runes = 1050 bytes; truncation must not
		// slice mid-codepoint.
		in := strings.Repeat("世", modelErrReasonMaxRunes+50)
		got := boundedReason(in)
		if !utf8.ValidString(got) {
			t.Errorf("boundedReason produced invalid UTF-8 (mid-codepoint split): % x", got)
		}
		// Exactly cap runes + the single ellipsis rune.
		if n := runeLen(got); n != modelErrReasonMaxRunes+1 {
			t.Errorf("boundedReason rune count = %d, want %d (cap+ellipsis)", n, modelErrReasonMaxRunes+1)
		}
		// And the non-ellipsis prefix is exactly cap runes of the input.
		prefixRunes := []rune(got)[:modelErrReasonMaxRunes]
		if string(prefixRunes) != strings.Repeat("世", modelErrReasonMaxRunes) {
			t.Error("truncated prefix lost input content")
		}
	})
}

func TestWithReason(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		reason string
		want   string
	}{
		{"empty reason returns base unchanged", "unexpected error: status 500", "", "unexpected error: status 500"},
		{"non-empty reason joins with colon-space", "rate limit exceeded", "too many requests", "rate limit exceeded: too many requests"},
		{"reason with internal spaces preserved", "gateway timeout", "upstream took 61s", "gateway timeout: upstream took 61s"},
		{"empty base edge", "", "orphan reason", ": orphan reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withReason(tt.base, tt.reason); got != tt.want {
				t.Errorf("withReason(%q, %q) = %q\nwant %q", tt.base, tt.reason, got, tt.want)
			}
		})
	}
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
