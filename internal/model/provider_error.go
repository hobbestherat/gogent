package model

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// modelErrReasonMaxRunes bounds the provider error reason spliced into
// ModelError.Message. It is larger than ui/tui's 120-rune notification cap
// because this is a diagnostic one-liner shown in the error stream, not a toast,
// yet small enough that a pathological body can never dump a multi-KB page into
// the transcript. The full untruncated body still lives in ModelError.RawResponse.
const modelErrReasonMaxRunes = 300

// extractProviderMessage pulls the human-readable rejection reason out of a
// provider error body. Every provider gogent targets nests the reason at the same
// path, error.message:
//
//	OpenAI / Z.AI / OpenRouter / Vertex-OpenAI shim:
//	    {"error":{"message":"...","type":"invalid_request_error"}}
//	Anthropic / Vertex-Anthropic:
//	    {"type":"error","error":{"type":"invalid_request_error","message":"..."}}
//	Vertex native Gemini:
//	    {"error":{"code":400,"message":"...","status":"INVALID_ARGUMENT"}}
//
// A single struct{ Error struct{ Message string } } covers all three with no
// per-provider branching. For off-spec gateways it falls back, first non-empty
// wins, through: a top-level "message", an error rendered as a bare JSON string,
// and finally the raw body itself (non-JSON: HTML error pages, plain text,
// gateway blurbs). It returns "" when nothing usable is found (e.g. an empty
// body) so the caller can preserve the prior status-only message rather than
// emit a dangling separator.
func extractProviderMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}

	// 1 + 3: error.message (object form) or error rendered as a bare string.
	// json.RawMessage defers decoding error so both shapes can be tried.
	var objErr struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"` // 2: top-level message
	}
	if err := json.Unmarshal([]byte(trimmed), &objErr); err == nil {
		if len(objErr.Error) > 0 {
			// error: {"message":"..."}
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(objErr.Error, &nested) == nil && nested.Message != "" {
				return nested.Message
			}
			// error: "some string"
			var errStr string
			if json.Unmarshal(objErr.Error, &errStr) == nil && strings.TrimSpace(errStr) != "" {
				return errStr
			}
		}
		// 2: {"message":"..."} at the top level
		if objErr.Message != "" {
			return objErr.Message
		}
	}

	// 4: non-JSON (or JSON we don't recognise) — surface the raw body. The caller
	// bounds it, so an HTML page or plain-text blurb is still capped and harmless.
	return trimmed
}

// boundedReason normalises a provider reason for inclusion in an error message:
// it trims, keeps only the first line, and caps the result at
// modelErrReasonMaxRunes runes without splitting a UTF-8 codepoint. It mirrors
// ui/tui.firstLine but is duplicated here so internal/model never imports ui/tui.
func boundedReason(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= modelErrReasonMaxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:modelErrReasonMaxRunes]) + "…"
}

// withReason joins a base error message with a bounded provider reason. When the
// reason is empty it returns base unchanged, guaranteeing the prior status-only
// message (and its tests) are preserved for empty or unparsable bodies.
func withReason(base, reason string) string {
	if reason == "" {
		return base
	}
	return base + ": " + reason
}
