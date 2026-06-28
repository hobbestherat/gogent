package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// Tests for the analyzeError wiring of the provider-error-reason fix (issue #555).
// The fix splices a bounded error.message excerpt into every branch's Message so
// ModelError.Error() surfaces the actual rejection reason instead of an opaque
// status. These tests cover: the headline case, per-branch wiring, the
// Type/counter invariants (no classification regression), the bounding +
// raw-body-preservation invariants, the empty-body no-dangling-separator guard,
// the regression guard for the descriptive 404 message, end-to-end through the
// blocking and streaming HTTP paths, and the holistic "internal/model must not
// import ui/tui" seam.

// headlineAnthropicMaxTokensBody is the real-shaped body from the issue: an
// Anthropic over-limit max_tokens 400. It contains neither "context" nor
// "length", so analyzeError's 400 sniff must NOT classify it as an overflow and
// it must fall through to the generic catch-all — where the reason now surfaces.
const headlineAnthropicMaxTokensBody = `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 8192 is greater than the maximum allowed for claude-opus-4-8. Please reduce max_tokens and try again."}}`

// TestAnalyzeError_Headline_AnthropicOverLimitMaxTokens is the single most
// important assertion for #555: the issue's exact example must surface the
// offending field ("max_tokens ... greater than the maximum") in the
// user-visible error string, not just "unexpected error: status 400".
func TestAnalyzeError_Headline_AnthropicOverLimitMaxTokens(t *testing.T) {
	conn := NewModelConnection()
	me := conn.analyzeError(400, headlineAnthropicMaxTokensBody)

	// Falls through to the generic catch-all (body has no "context"/"length").
	if me.Type != ErrorGeneric {
		t.Errorf("over-limit max_tokens must classify as ErrorGeneric (sniff does not match), got %q", me.Type)
	}
	got := me.Error()
	if !strings.Contains(got, "max_tokens") || !strings.Contains(got, "greater than the maximum") {
		t.Errorf("headline error must surface the provider reason; got: %q", got)
	}
	if !strings.Contains(got, "status 400") {
		t.Errorf("the status must still be present alongside the reason; got: %q", got)
	}
}

// TestAnalyzeError_EveryBranchAppendsReason: each status branch splices the
// bounded reason into its Message. A body that names the offending field must
// appear in Message for overflow(400), refusal(403), 404, 429, 504, and the
// generic catch-all.
func TestAnalyzeError_EveryBranchAppendsReason(t *testing.T) {
	// Each case picks a body that (a) triggers the branch's sniff where one exists
	// and (b) carries an error.message naming the offending field.
	tests := []struct {
		name       string
		status     int
		body       string
		wantType   ModelErrorType
		wantReason string // a substring expected in me.Message
	}{
		{
			name:       "400 context overflow carries the real numbers",
			status:     400,
			body:       `{"error":{"message":"This model's maximum context length is 200000 tokens. However your messages resulted in 250000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			wantType:   ErrorContextOverflow,
			wantReason: "maximum context length is 200000 tokens",
		},
		{
			name:       "403 refusal carries the reason",
			status:     403,
			body:       `{"error":{"message":"content was flagged as a content_policy_violation","type":"invalid_request_error"}}`,
			wantType:   ErrorRefusal,
			wantReason: "content_policy_violation",
		},
		{
			name:       "404 carries the reason",
			status:     404,
			body:       `{"error":{"message":"model 'glm-5.2' not found in this region","type":"invalid_request_error"}}`,
			wantType:   ErrorGeneric,
			wantReason: "not found in this region",
		},
		{
			name:       "429 rate limit carries the reason",
			status:     429,
			body:       `{"error":{"message":"rate limit reached for tier free","type":"rate_limit_error"}}`,
			wantType:   ErrorRateLimit,
			wantReason: "rate limit reached for tier free",
		},
		{
			name:       "504 gateway timeout carries the reason",
			status:     504,
			body:       `{"error":{"message":"upstream timed out after 60s","type":"server_error"}}`,
			wantType:   ErrorTimeout,
			wantReason: "upstream timed out after 60s",
		},
		{
			name:       "generic 500 carries the reason",
			status:     500,
			body:       `{"error":{"message":"internal engine fault id=abc123","type":"server_error"}}`,
			wantType:   ErrorGeneric,
			wantReason: "internal engine fault id=abc123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := NewModelConnection()
			me := conn.analyzeError(tt.status, tt.body)
			if me.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", me.Type, tt.wantType)
			}
			if !strings.Contains(me.Message, tt.wantReason) {
				t.Errorf("Message must contain the provider reason %q; got: %q", tt.wantReason, me.Message)
			}
			// And the surfaced .Error() (Type: Message) must carry it too.
			if !strings.Contains(me.Error(), tt.wantReason) {
				t.Errorf(".Error() must contain the reason; got: %q", me.Error())
			}
		})
	}
}

// TestAnalyzeError_TypeAndCountersUnchangedByReason is the no-regression guard
// for classification: appending a reason to Message must not change Type,
// HTTPStatusCode, the branch-specific counter, or the overall ErrorCount. It
// mirrors TestAnalyzeErrorCounters but with reason-bearing bodies so the wiring
// is exercised end to end.
func TestAnalyzeError_TypeAndCountersUnchangedByReason(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantType ModelErrorType
		counter  func(*ModelStats) int
	}{
		{"rate_limit with reason", 429, `{"error":{"message":"slow down"}}`, ErrorRateLimit, func(s *ModelStats) int { return s.RateLimitCount }},
		{"context_overflow with reason", 400, `{"error":{"message":"context length exceeded"}}`, ErrorContextOverflow, func(s *ModelStats) int { return s.ContextWindowOverflowCount }},
		{"refusal with reason", 403, `{"error":{"message":"content refusal"}}`, ErrorRefusal, func(s *ModelStats) int { return s.RefusalCount }},
		{"timeout with reason", 504, `{"error":{"message":"gateway"}}`, ErrorTimeout, func(s *ModelStats) int { return s.TimeoutCount }},
		{"generic with reason", 500, `{"error":{"message":"boom"}}`, ErrorGeneric, func(s *ModelStats) int { return s.GenericErrorCount }},
		{"404 with reason", 404, `{"error":{"message":"missing model"}}`, ErrorGeneric, func(s *ModelStats) int { return s.GenericErrorCount }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewModelConnection()
			me := c.analyzeError(tt.status, tt.body)
			if me.Type != tt.wantType {
				t.Errorf("type = %q, want %q", me.Type, tt.wantType)
			}
			if me.HTTPStatusCode != tt.status {
				t.Errorf("HTTPStatusCode = %d, want %d", me.HTTPStatusCode, tt.status)
			}
			stats := c.GetStats()
			if got := tt.counter(stats); got != 1 {
				t.Errorf("branch counter = %d, want 1", got)
			}
			if stats.ErrorCount != 1 {
				t.Errorf("ErrorCount = %d, want 1 (reason must not double-count)", stats.ErrorCount)
			}
		})
	}
}

// TestAnalyzeError_RawResponseNotTruncated_FullBodyPreserved guards the "no
// session-store regression" half of the fix: analyzeError must store the FULL
// body in RawResponse (truncation is session_store.go's job, not the model
// layer's). A pathological multi-KB body must round-trip verbatim while its
// Message excerpt stays bounded.
func TestAnalyzeError_RawResponseNotTruncated_FullBodyPreserved(t *testing.T) {
	// A unique sentinel placed at the FAR END of a 20 KiB message value — past the
	// 8 KiB session-store cap. If analyzeError truncated RawResponse, this would be
	// the first content dropped. It must survive (truncation is session_store's job).
	const sentinel = "END_SENTINEL_FAR_END"
	hugeReason := strings.Repeat("x", 20*1024) // 20 KiB — well past the 8 KiB session cap
	body := `{"error":{"message":"` + hugeReason + sentinel + `"}}`

	conn := NewModelConnection()
	me := conn.analyzeError(500, body)

	// Full body preserved byte-for-byte (Go strings are immutable; analyzeError
	// must hand back the exact response it was given).
	if me.RawResponse != body {
		t.Errorf("RawResponse must be the full untruncated body (len %d), got len %d", len(body), len(me.RawResponse))
	}
	if !strings.Contains(me.RawResponse, sentinel) {
		t.Error("far-end sentinel lost — analyzeError truncated the body (it must not; truncation belongs to session_store)")
	}
	// Message excerpt stays bounded: no multi-KB page dumped into the message.
	if n := runeLen(me.Message); n > modelErrReasonMaxRunes+128 {
		t.Errorf("Message must stay bounded; got %d runes (reason cap is %d)", n, modelErrReasonMaxRunes)
	}
	if !strings.HasSuffix(me.Message, "…") {
		t.Errorf("an over-cap reason must end with the ellipsis marker; got: %q", me.Message)
	}
}

// TestAnalyzeError_EmptyOrBlankBody_MessageIdenticalToStatusOnly is the
// no-dangling-separator guard: an empty or whitespace-only body yields reason ""
// and withReason must leave the prior status-only Message byte-for-byte
// unchanged (so nothing downstream sees a stray ": ").
func TestAnalyzeError_EmptyOrBlankBody_MessageIdenticalToStatusOnly(t *testing.T) {
	wantGeneric := "unexpected error: status 500"
	for _, body := range []string{"", "   ", "\n\t\r\n  "} {
		conn := NewModelConnection()
		me := conn.analyzeError(500, body)
		if me.Message != wantGeneric {
			t.Errorf("empty/blank body must keep status-only message %q; body=%q got %q", wantGeneric, body, me.Message)
		}
		if strings.HasSuffix(me.Message, ": ") {
			t.Errorf("message must never end with a dangling separator; got %q", me.Message)
		}
	}
	// 429 with an empty body keeps its fixed phrase verbatim too.
	conn := NewModelConnection()
	if me := conn.analyzeError(429, ""); me.Message != "rate limit exceeded" {
		t.Errorf("empty-body 429 must keep %q; got %q", "rate limit exceeded", me.Message)
	}
}

// TestAnalyzeError_404_PreservesDescriptiveSubstrings guards a real coupling the
// implementation touches: the descriptive 404 branch is asserted (in
// routable_config_validation_test.go) to contain "404" and "endpoint". The
// appended reason must preserve both substrings. The non-JSON body here also
// exercises the raw-body fallback rung.
func TestAnalyzeError_404_PreservesDescriptiveSubstrings(t *testing.T) {
	conn := NewModelConnection()
	me := conn.analyzeError(404, "not found body")
	if !strings.Contains(me.Message, "404") {
		t.Errorf("404 message must still mention 404; got: %q", me.Message)
	}
	if !strings.Contains(me.Message, "endpoint") {
		t.Errorf("404 message must still mention endpoint; got: %q", me.Message)
	}
	if !strings.Contains(me.Message, "not found body") {
		t.Errorf("raw-body fallback reason must be appended; got: %q", me.Message)
	}
}

// TestAnalyzeError_MultilineReason_OnlyFirstLineInMessage: a reason spanning
// multiple lines is collapsed to its first line in the surfaced message so a
// verbose provider error cannot inject newlines into the transcript line.
func TestAnalyzeError_MultilineReason_OnlyFirstLineInMessage(t *testing.T) {
	// Build the body via json.Marshal so the embedded newline is wire-correct
	// (a provider sends "\n" as a 2-char JSON escape that decodes to a real LF).
	inner := struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}{}
	inner.Error.Message = "first offending line\nsecond line that must not appear"
	body, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	conn := NewModelConnection()
	me := conn.analyzeError(400, string(body))
	if !strings.Contains(me.Message, "first offending line") {
		t.Errorf("first line of the reason must be present; got: %q", me.Message)
	}
	if strings.Contains(me.Message, "second line that must not appear") {
		t.Errorf("only the first line may be surfaced; got: %q", me.Message)
	}
}

// ---------------------------------------------------------------------------
// End-to-end through the real HTTP paths (blocking + streaming). These prove the
// reason actually reaches the user-visible *ModelError returned by the public
// API — not just analyzeError in isolation.
// ---------------------------------------------------------------------------

// errorServer returns the given status + JSON body for every request.
func errorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestComplete_400WithBody_SurfacesReasonInError: the blocking completion path
// (provider.go doJSON → analyzeError) returns a *ModelError whose .Error()
// contains the provider reason. This is the full emit chain minus the TUI.
func TestComplete_400WithBody_SurfacesReasonInError(t *testing.T) {
	server := errorServer(t, http.StatusBadRequest, headlineAnthropicMaxTokensBody)
	c := newTestConn(server.URL)

	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected a 400 error")
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T: %v", err, err)
	}
	if !strings.Contains(me.Error(), "max_tokens") || !strings.Contains(me.Error(), "greater than the maximum") {
		t.Errorf("the surfaced error must contain the provider reason; got: %q", me.Error())
	}
	if me.RawResponse != headlineAnthropicMaxTokensBody {
		t.Errorf("RawResponse must carry the full body through the HTTP path; got len %d", len(me.RawResponse))
	}
}

// TestComplete_429WithBody_SurfacesReasonInError: a retryable status still
// surfaces the reason after exhausting retries (the final-attempt analyzeError).
func TestComplete_429WithBody_SurfacesReasonInError(t *testing.T) {
	const body = `{"error":{"message":"quota exceeded for org","type":"rate_limit_error"}}`
	server := errorServer(t, http.StatusTooManyRequests, body)
	c := newTestConn(server.URL)

	_, err := c.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("expected a 429 error")
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T: %v", err, err)
	}
	if me.Type != ErrorRateLimit {
		t.Errorf("Type = %q, want ErrorRateLimit", me.Type)
	}
	if !strings.Contains(me.Error(), "quota exceeded for org") {
		t.Errorf("the surfaced error must contain the rate-limit reason; got: %q", me.Error())
	}
}

// TestCompleteStream_400WithBody_SurfacesReasonInError: the streaming path
// (completeStream → analyzeError) also surfaces the reason on its error channel.
// Guards that the fix is not silently bypassed for streaming sessions.
func TestCompleteStream_400WithBody_SurfacesReasonInError(t *testing.T) {
	server := errorServer(t, http.StatusBadRequest, headlineAnthropicMaxTokensBody)
	c := newTestConn(server.URL)

	streamCh, errCh := c.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	deltas, _, err := drain(t, streamCh, errCh)
	if err == nil {
		t.Fatalf("expected a 400 error (deltas=%v)", deltas)
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T: %v", err, err)
	}
	if !strings.Contains(me.Error(), "max_tokens") {
		t.Errorf("streaming error must surface the provider reason; got: %q", me.Error())
	}
}

// TestCompleteWithToolsStreamCtx_400WithBody_SurfacesReasonInError: the
// live-thinking streaming entry (the one the agent loop actually calls) also
// surfaces the reason — the closest we get to the user-visible path without a TUI.
func TestCompleteWithToolsStreamCtx_400WithBody_SurfacesReasonInError(t *testing.T) {
	server := errorServer(t, http.StatusBadRequest, headlineAnthropicMaxTokensBody)
	c := newTestConn(server.URL)

	resp, err := c.CompleteWithToolsStreamCtx(context.Background(),
		[]Message{{Role: RoleUser, Content: "hi"}}, nil, func(string) {})
	if err == nil {
		t.Fatalf("expected a 400 error, got resp=%+v", resp)
	}
	if resp != nil {
		t.Errorf("expected a nil response on error, got %+v", resp)
	}
	me, ok := err.(*ModelError)
	if !ok {
		t.Fatalf("expected *ModelError, got %T: %v", err, err)
	}
	if !strings.Contains(me.Error(), "greater than the maximum") {
		t.Errorf("streaming-with-tools error must surface the provider reason; got: %q", me.Error())
	}
}

// ---------------------------------------------------------------------------
// Holistic seam guard (criterion 4): internal/model must NOT import ui/tui. The
// boundedReason helper is intentionally duplicated rather than imported, so this
// package must never acquire a dependency on gogent/ui/tui.
// ---------------------------------------------------------------------------

func TestInternalModel_DoesNotImportUITUI(t *testing.T) {
	// `go list -json .` from within the package gives the authoritative direct
	// imports. No transient/flaky heuristics: if gogent/ui/tui shows up here, the
	// seam has been broken.
	out, err := exec.Command("go", "list", "-json", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -json . failed: %v\n%s", err, out)
	}
	var info struct {
		Imports     []string `json:"Imports"`
		TestImports []string `json:"TestImports"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("cannot parse go list output: %v", err)
	}
	const forbidden = "gogent/ui/tui"
	for _, imp := range append(append([]string{}, info.Imports...), info.TestImports...) {
		if imp == forbidden {
			t.Errorf("internal/model must not import %s (seam: duplicate firstLine-style helpers in-package)", forbidden)
		}
	}
}
