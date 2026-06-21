package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

// buildBodyBytes is the test-only convenience for the buffer-writing buildBody:
// it marshals into a throwaway buffer and returns the bytes. Production callers
// reuse a pooled buffer (see acquireReqBodyBuf in connection.go); tests just
// want the marshaled bytes back.
func buildBodyBytes(a adapter, req CompletionRequest) ([]byte, error) {
	var buf bytes.Buffer
	if err := a.buildBody(req, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeJSON must stay byte-for-byte identical to json.Marshal (same HTML
// escaping, no trailing newline) so the wire body does not change just because
// marshaling moved into a caller-owned buffer. This guards that invariant across
// the request shapes gogent actually sends.
func TestEncodeJSONMatchesJSONMarshal(t *testing.T) {
	tools := []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "f", Parameters: map[string]interface{}{"type": "object"}},
	}}
	cases := []struct {
		name string
		v    any
	}{
		{"completion_request", CompletionRequest{
			Model:    "gpt-4o",
			Messages: []Message{{Role: RoleUser, Content: "hi <b>&amp;</b>"}},
			Tools:    tools,
		}},
		{"map_and_slice", map[string]interface{}{"a": 1, "b": []int{2, 3}}},
		{"html_chars", "a < b > c & d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var buf bytes.Buffer
			if err := encodeJSON(&buf, tc.v); err != nil {
				t.Fatalf("encodeJSON: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("encodeJSON diverges from json.Marshal:\n got: %s\nwant: %s", buf.Bytes(), want)
			}
		})
	}
}

// buildBody now writes into a caller-owned buffer; verify both adapters produce
// valid, newline-free JSON through that path.
func TestBuildBodyWritesToBuffer(t *testing.T) {
	tools := []ToolDef{{
		Type:     "function",
		Function: FunctionDef{Name: "f", Parameters: map[string]interface{}{"type": "object"}},
	}}
	cases := []struct {
		name string
		adp  adapter
		req  CompletionRequest
	}{
		{"openai", openAIAdapter{}, CompletionRequest{
			Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Tools: tools,
		}},
		{"anthropic", anthropicAdapter{}, CompletionRequest{
			Model: "claude-x", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Tools: tools,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.adp.buildBody(tc.req, &buf); err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			raw := buf.Bytes()
			if len(raw) == 0 {
				t.Fatal("buildBody wrote an empty buffer")
			}
			if raw[len(raw)-1] == '\n' {
				t.Errorf("buildBody left a trailing newline: %q", raw)
			}
			if !json.Valid(raw) {
				t.Errorf("buildBody produced invalid JSON: %s", raw)
			}
		})
	}
}

// A buffer taken from the request-body pool must be safe to reuse: when the pool
// does hand a released buffer back, encodeJSON resets it first so stale bytes
// from a prior turn can never leak into a new body. We do NOT assert pointer
// identity across release/acquire — sync.Pool is explicitly allowed to drop
// entries (it does so aggressively under the race detector), so that assertion
// was flaky and tested an unguaranteed implementation detail rather than gogent
// behaviour. The reset/no-leak guarantee below holds whether or not the pool
// actually reused the buffer.
func TestReqBodyPoolReuseResetsBuffer(t *testing.T) {
	b1 := acquireReqBodyBuf()
	b1.WriteString("payload-from-previous-turn")
	releaseReqBodyBuf(b1)

	b2 := acquireReqBodyBuf()
	defer releaseReqBodyBuf(b2)

	// The reused buffer must still marshal correctly: encodeJSON resets it first,
	// so stale bytes from the previous turn cannot leak into the new body.
	req := CompletionRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "fresh"}}}
	if err := encodeJSON(b2, req); err != nil {
		t.Fatalf("encodeJSON into reused buffer: %v", err)
	}
	if bytes.Contains(b2.Bytes(), []byte("payload-from-previous-turn")) {
		t.Errorf("stale bytes from a prior turn leaked into the reused buffer: %s", b2.Bytes())
	}
	var got CompletionRequest
	if err := json.Unmarshal(b2.Bytes(), &got); err != nil {
		t.Fatalf("reused-buffer output is not valid JSON: %v", err)
	}
}

// makeLargeRequest builds a request big enough that re-allocating its marshaled
// body each call would be measurable, exercising the pool's cross-call reuse.
func makeLargeRequest() CompletionRequest {
	msgs := make([]Message, 200)
	for i := range msgs {
		msgs[i] = Message{Role: RoleUser, Content: fmt.Sprintf("message %d with some filler text to pad the body", i)}
	}
	tools := make([]ToolDef, 20)
	for i := range tools {
		tools[i] = ToolDef{Type: "function", Function: FunctionDef{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "a tool with a description",
			Parameters:  map[string]interface{}{"type": "object"},
		}}
	}
	return CompletionRequest{Model: "gpt-4o", Messages: msgs, Tools: tools}
}

// BenchmarkBuildBodyPooled vs BenchmarkBuildBodyFresh shows the allocation win
// from reusing the marshal buffer across calls: pooled reuses one growing
// buffer, fresh allocates (and would GC) it each call.
func BenchmarkBuildBodyPooled(b *testing.B) {
	req := makeLargeRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := acquireReqBodyBuf()
		if err := (openAIAdapter{}).buildBody(req, buf); err != nil {
			b.Fatal(err)
		}
		releaseReqBodyBuf(buf)
	}
}

func BenchmarkBuildBodyFresh(b *testing.B) {
	req := makeLargeRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(req); err != nil {
			b.Fatal(err)
		}
	}
}
