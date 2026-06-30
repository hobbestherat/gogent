package model

import (
	"bytes"
	"encoding/json"
	"testing"

	"gogent/internal/config"
)

// ---------------------------------------------------------------------------
// Issue #545 — Anthropic explicit cache-control: multi-breakpoint + TTL.
//
// These tests cover the request-side fix in anthropicAdapter.buildBody:
//   - Gap A: a turn that adds >=20 content blocks still emits <=4 breakpoints,
//     placed so the cache-read lookback finds a recent write (the previous turn's
//     end-of-prefix boundary is re-emitted as a breakpoint — the exact-position
//     read hit).
//   - Gap B: ttl:"1h" only when configured; default emits no ttl; "off" suppresses
//     every cache_control breakpoint (system + transcript).
//   - Regression guard: a small transcript stays byte-identical to the prior
//     single-transcript-breakpoint behavior; the volatile tail never carries a
//     breakpoint; CacheTTL never leaks onto the OpenAI-compatible wire.
// ---------------------------------------------------------------------------

// cacheBP is one placed cache_control breakpoint in a marshaled Anthropic body.
type cacheBP struct {
	tier string // "system" or the message role ("user"/"assistant")
	ttl  string // "" (default 5m) or "1h"
}

// anthropicBreakpoints walks a marshaled Anthropic request and returns every
// cache_control breakpoint in document order: the system block(s) first, then
// every content block of every message.
func anthropicBreakpoints(t *testing.T, raw []byte) []cacheBP {
	t.Helper()
	var got anthropicRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal anthropic body: %v", err)
	}
	var bps []cacheBP
	if sys, ok := got.System.([]interface{}); ok {
		for _, s := range sys {
			if sb, ok := s.(map[string]interface{}); ok {
				if cc, ok := sb["cache_control"].(map[string]interface{}); ok {
					bps = append(bps, cacheBP{tier: "system", ttl: ttlString(cc)})
				}
			}
		}
	}
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				bps = append(bps, cacheBP{tier: m.Role, ttl: b.CacheControl.Ttl})
			}
		}
	}
	return bps
}

// ttlString extracts the ttl value from a generically-decoded cache_control map.
func ttlString(cc map[string]interface{}) string {
	if v, ok := cc["ttl"].(string); ok {
		return v
	}
	return ""
}

// findBlockCC returns the CacheControl of the LAST content block whose
// tool_result Content or text Text equals want, or nil if no such block exists.
// Breakpoints are always placed on the last block of a message, so when content
// repeats across a merged tool-result turn the relevant block is the last match.
func findBlockCC(t *testing.T, raw []byte, want string) *anthropicCacheControl {
	t.Helper()
	var got anthropicRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal anthropic body: %v", err)
	}
	var found *anthropicCacheControl
	for mi := range got.Messages {
		for bi := range got.Messages[mi].Content {
			b := &got.Messages[mi].Content[bi]
			if b.Content == want || b.Text == want {
				found = b.CacheControl // keep the last match
			}
		}
	}
	return found
}

// bodyHasTTL reports whether the marshaled body contains a "ttl" JSON key at all.
func bodyHasTTL(raw []byte) bool { return bytes.Contains(raw, []byte(`"ttl"`)) }

// bigAssistantTurn builds an assistant message with n tool calls (so it expands to
// 1 text + n tool_use = n+1 content blocks on the Anthropic wire).
func bigAssistantTurn(n int, prefix string) Message {
	tcs := make([]ToolCall, n)
	for i := range tcs {
		tcs[i] = ToolCall{
			ID:   "toolu_" + prefix + "_" + index(i),
			Type: "function",
			Function: FunctionCall{
				Name:      "read",
				Arguments: `{"path":"` + prefix + index(i) + `"}`,
			},
		}
	}
	return Message{Role: RoleAssistant, Content: prefix + " text", ToolCalls: tcs}
}

// bigToolResults builds n tool-result messages, one per call id, each its own
// tool_result block (the adapter merges them into one user turn).
func bigToolResults(n int, prefix, content string) []Message {
	ms := make([]Message, n)
	for i := range ms {
		ms[i] = Message{
			Role:       RoleTool,
			ToolCallID: "toolu_" + prefix + "_" + index(i),
			Content:    content,
		}
	}
	return ms
}

// index renders a small int as a stable string for unique ids/content.
func index(i int) string {
	const digits = "abcdefghijklmnopqrstuvwxyz0123456789"
	if i < 0 || i >= len(digits) {
		return "z" + jsonInt(i)
	}
	return string(digits[i])
}

func jsonInt(i int) string { b, _ := json.Marshal(i); return string(b) }

// ===========================================================================
// Gap B — TTL emission
// ===========================================================================

// TestAnthropicCacheTTLDefaultEmitsNoTTL: a default (CacheTTL="") request emits
// {type:"ephemeral"} breakpoints with NO ttl field anywhere — byte-identical to
// the pre-#545 wire output. This is the no-regression invariant for the default.
func TestAnthropicCacheTTLDefaultEmitsNoTTL(t *testing.T) {
	req := CompletionRequest{
		CacheTTL: "",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if bodyHasTTL(raw) {
		t.Errorf("default body must not emit ttl; got %s", raw)
	}
	for _, bp := range anthropicBreakpoints(t, raw) {
		if bp.ttl != "" {
			t.Errorf("default breakpoint has ttl %q; want none", bp.ttl)
		}
		if bp.tier == "" {
			t.Errorf("breakpoint tier empty")
		}
	}
}

// TestAnthropicCacheTTL5mEqualsDefault: an explicit "5m" resolves to the default,
// so the wire body is byte-identical to the empty/unset case (no ttl emitted).
func TestAnthropicCacheTTL5mEqualsDefault(t *testing.T) {
	base := CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	}
	def, _ := buildBodyBytes(anthropicAdapter{}, base)
	fivem := base
	fivem.CacheTTL = "5m"
	fm, err := buildBodyBytes(anthropicAdapter{}, fivem)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if !bytes.Equal(def, fm) {
		t.Errorf("CacheTTL=\"5m\" body differs from default body:\n default: %s\n 5m:     %s", def, fm)
	}
}

// TestAnthropicCacheTTL1hEmitsTTL: CacheTTL="1h" attaches ttl:"1h" to EVERY
// breakpoint (system + transcript) — a single, consistent TTL per request (the
// TTL-mixing rule is trivially satisfied while emitting one TTL).
func TestAnthropicCacheTTL1hEmitsTTL(t *testing.T) {
	req := CompletionRequest{
		CacheTTL: "1h",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	bps := anthropicBreakpoints(t, raw)
	if len(bps) < 2 {
		t.Fatalf("want system + transcript breakpoints with 1h, got %d: %+v", len(bps), bps)
	}
	for _, bp := range bps {
		if bp.ttl != "1h" {
			t.Errorf("1h breakpoint tier=%s has ttl %q; want \"1h\" (single consistent TTL)", bp.tier, bp.ttl)
		}
	}
}

// TestAnthropicCacheTTLUnrecognizedNeverEmitsInvalidTTL: even if an unnormalized
// value reached the adapter, only the literal "1h" ever emits a ttl — so a stray
// "2h"/"30m" can never reach the wire and 400 the request.
func TestAnthropicCacheTTLUnrecognizedNeverEmitsInvalidTTL(t *testing.T) {
	for _, bad := range []string{"2h", "30m", "1 hour", "60m"} {
		req := CompletionRequest{
			CacheTTL: bad,
			Messages: []Message{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "hi"},
			},
		}
		raw, err := buildBodyBytes(anthropicAdapter{}, req)
		if err != nil {
			t.Fatalf("buildBody(%q): %v", bad, err)
		}
		if bodyHasTTL(raw) {
			t.Errorf("unrecognized CacheTTL=%q leaked a ttl onto the wire: %s", bad, raw)
		}
	}
}

// TestAnthropicCacheTTLOffSuppressesAll: CacheTTL="off" emits NO cache_control at
// all — neither on the system block nor anywhere in the transcript. This is the
// kill switch.
func TestAnthropicCacheTTLOffSuppressesAll(t *testing.T) {
	req := CompletionRequest{
		CacheTTL: "off",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
			bigAssistantTurn(2, "a"),
		},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if bps := anthropicBreakpoints(t, raw); len(bps) != 0 {
		t.Errorf("off must suppress all breakpoints; got %d: %+v", len(bps), bps)
	}
	if bytes.Contains(raw, []byte("cache_control")) {
		t.Errorf("off body must contain no cache_control: %s", raw)
	}
}

// ===========================================================================
// Gap B — gating: adapter emits by default; only "off" suppresses
// ===========================================================================

// TestAnthropicCacheDefaultOnForDirectRequest: a zero-value CompletionRequest
// (CacheTTL unset, as the existing direct-construction tests and any caller that
// does not go through buildRequest produce) keeps caching ON. This protects the
// existing TestAnthropicBuildBody / vertex tests from the positive-gate regression.
func TestAnthropicCacheDefaultOnForDirectRequest(t *testing.T) {
	req := CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
		},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if bps := anthropicBreakpoints(t, raw); len(bps) < 2 {
		t.Errorf("zero-value request must still emit system + transcript breakpoints; got %d", len(bps))
	}
}

// ===========================================================================
// Gap A — multi-breakpoint placement
// ===========================================================================

// TestAnthropicMultiBreakpointLargeTurnStaysWithinLookback is the central Gap A
// test. A transcript whose LAST turn added >=20 content blocks (an assistant turn
// with 10 tool calls: 1 text + 10 tool_use, then 10 tool_result) must still emit
// <=4 breakpoints, AND the previous turn's end-of-prefix boundary (the
// "PREV_TURN_END" tool result) must be re-emitted as a breakpoint at the SAME
// block position — the exact-position cache read hit that the single-breakpoint
// design lost.
func TestAnthropicMultiBreakpointLargeTurnStaysWithinLookback(t *testing.T) {
	const M = 10 // 10 tool calls -> last turn adds ~2M+1 = 21 blocks (>20 lookback)
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "q"},
		bigAssistantTurn(1, "a0"),
	}
	// Previous turn's tool results — its LAST result is the prior end-of-prefix.
	msgs = append(msgs, bigToolResults(1, "a0", "PREV_TURN_END")...)
	// The big turn that pushes the single end-of-prefix breakpoint >20 blocks past
	// the previous write.
	msgs = append(msgs, bigAssistantTurn(M, "BIG"))
	msgs = append(msgs, bigToolResults(M, "BIG", "big-result")...)

	req := CompletionRequest{Messages: msgs}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	// (a) total breakpoints never exceed the API ceiling of 4.
	bps := anthropicBreakpoints(t, raw)
	if len(bps) > maxAnthropicBreakpoints {
		t.Fatalf("emitted %d breakpoints; API 400s on >4: %+v", len(bps), bps)
	}

	// (b) the previous turn's end-of-prefix boundary is re-emitted as a breakpoint
	// (exact-position read hit). With the old single-breakpoint code this block
	// would NOT carry cache_control once the big turn pushed past the lookback.
	cc := findBlockCC(t, raw, "PREV_TURN_END")
	if cc == nil {
		t.Fatalf("PREV_TURN_END block not found in body: %s", raw)
	}
	if cc.Type != "ephemeral" {
		t.Errorf("PREV_TURN_END breakpoint = %+v; want ephemeral (exact-position read hit)", cc)
	}

	// (c) the very last block of the stable prefix still carries a breakpoint
	// (today's placement is preserved).
	lastCC := findBlockCC(t, raw, "big-result")
	if lastCC == nil || lastCC.Type != "ephemeral" {
		t.Errorf("end-of-prefix breakpoint missing/non-ephemeral: %+v", lastCC)
	}
}

// TestAnthropicMultiBreakpointNeverExceedsFour hammers the >4→400 invariant on a
// long transcript that would otherwise want many breakpoints, both with and
// without a system prompt (the budget accounting differs: system present → 3
// transcript breakpoints; absent → 4).
func TestAnthropicMultiBreakpointNeverExceedsFour(t *testing.T) {
	build := func(withSystem bool) []Message {
		var msgs []Message
		if withSystem {
			msgs = append(msgs, Message{Role: RoleSystem, Content: "sys"})
		}
		msgs = append(msgs, Message{Role: RoleUser, Content: "q"})
		// Eight turns, each with 8 tool calls (~17 blocks/turn) — well past any
		// reasonable lookback many times over.
		for i := 0; i < 8; i++ {
			p := "t" + index(i)
			msgs = append(msgs, bigAssistantTurn(8, p))
			msgs = append(msgs, bigToolResults(8, p, p+"-res")...)
		}
		return msgs
	}
	for _, withSystem := range []bool{true, false} {
		req := CompletionRequest{Messages: build(withSystem)}
		raw, err := buildBodyBytes(anthropicAdapter{}, req)
		if err != nil {
			t.Fatalf("buildBody(withSystem=%v): %v", withSystem, err)
		}
		bps := anthropicBreakpoints(t, raw)
		if len(bps) > maxAnthropicBreakpoints {
			t.Errorf("withSystem=%v: emitted %d breakpoints (>4 → API 400): %+v", withSystem, len(bps), bps)
		}
		// A long transcript with a big last turn must place MORE than one
		// breakpoint (the fix's whole point); a single breakpoint here would mean
		// the multi-breakpoint path never engaged.
		if len(bps) < 2 {
			t.Errorf("withSystem=%v: only %d breakpoints on a long big-turn transcript; multi-breakpoint path did not engage", withSystem, len(bps))
		}
	}
}

// TestAnthropicSmallTranscriptSingleBreakpoint: a SHORT stable prefix (whole
// prefix < cacheBreakpointSpacing) emits exactly ONE transcript breakpoint, on the
// last non-volatile block — byte-identical to the pre-#545 single-breakpoint
// behavior. No extra breakpoints, and no breakpoint on any earlier block.
func TestAnthropicSmallTranscriptSingleBreakpoint(t *testing.T) {
	req := CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "q"},
			bigAssistantTurn(1, "a"), // 2 blocks
		},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Exactly one transcript breakpoint.
	var transcriptBPs int
	for _, m := range got.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				transcriptBPs++
			}
		}
	}
	if transcriptBPs != 1 {
		t.Errorf("small transcript: %d transcript breakpoints, want exactly 1 (byte-identical to prior behavior)", transcriptBPs)
	}
	// That one breakpoint is on the LAST block of the LAST message.
	lastMsg := got.Messages[len(got.Messages)-1]
	lastBlock := lastMsg.Content[len(lastMsg.Content)-1]
	if lastBlock.CacheControl == nil || lastBlock.CacheControl.Type != "ephemeral" {
		t.Errorf("last block of last message must carry the breakpoint: %+v", lastBlock.CacheControl)
	}
}

// TestAnthropicBreakpointNeverOnVolatileTail: the trailing volatile message (live
// git status/todos) carries fast-changing content and must sit AFTER every
// breakpoint, or it would invalidate the cached prefix each turn. Even with a
// large prior turn (multi-breakpoint engaged) and the volatile tail merging into
// the last user turn, NO breakpoint may land on or after the volatile block.
func TestAnthropicBreakpointNeverOnVolatileTail(t *testing.T) {
	const M = 10
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "q"},
		bigAssistantTurn(M, "BIG"),
	}
	msgs = append(msgs, bigToolResults(M, "BIG", "res")...)
	// Volatile tail, same role (user) as the tool-result turn → it MERGES into that
	// turn's content array, so a naive end-of-message index would land on it.
	msgs = append(msgs, Message{Role: RoleUser, Content: "VOLATILE_TAIL", Volatile: true})

	req := CompletionRequest{Messages: msgs}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	// The volatile block itself has no breakpoint.
	if cc := findBlockCC(t, raw, "VOLATILE_TAIL"); cc != nil {
		t.Errorf("volatile tail must NOT carry a breakpoint; got %+v", cc)
	}
	// The last non-volatile block (a tool result) DOES carry the breakpoint, i.e.
	// the breakpoint landed before the merged volatile blocks within the turn.
	if cc := findBlockCC(t, raw, "res"); cc == nil || cc.Type != "ephemeral" {
		t.Errorf("last non-volatile block must carry the breakpoint; got %+v", cc)
	}
	// And still within the API ceiling.
	if bps := anthropicBreakpoints(t, raw); len(bps) > maxAnthropicBreakpoints {
		t.Errorf("volatile case emitted %d breakpoints (>4)", len(bps))
	}
}

// TestAnthropicOnlyVolatileStillBreaksOnPrefix: when the ONLY content is the
// system prompt plus a volatile tail (no stable transcript), no transcript
// breakpoint is emitted (boundaries is empty), but the system breakpoint still is.
func TestAnthropicOnlyVolatileStillBreaksOnPrefix(t *testing.T) {
	req := CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "only-volatile", Volatile: true},
		},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	bps := anthropicBreakpoints(t, raw)
	if len(bps) != 1 || bps[0].tier != "system" {
		t.Errorf("only-volatile: want exactly the system breakpoint, got %+v", bps)
	}
}

// TestAnthropicNoSystemPromptBudget: with no system prompt the system breakpoint
// is absent, so the transcript may use the full 4-breakpoint budget. Verify the
// total still never exceeds 4 and no phantom system breakpoint appears.
func TestAnthropicNoSystemPromptBudget(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "q"}}
	for i := 0; i < 6; i++ {
		p := "n" + index(i)
		msgs = append(msgs, bigAssistantTurn(8, p))
		msgs = append(msgs, bigToolResults(8, p, p+"-res")...)
	}
	req := CompletionRequest{Messages: msgs}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	bps := anthropicBreakpoints(t, raw)
	for _, bp := range bps {
		if bp.tier == "system" {
			t.Errorf("no system prompt but a system breakpoint appeared: %+v", bp)
		}
	}
	if len(bps) > maxAnthropicBreakpoints {
		t.Errorf("no-system case emitted %d breakpoints (>4)", len(bps))
	}
}

// TestAnthropicBreakpointPolicySharedOnVertex: Claude-on-Vertex uses the same
// anthropicAdapter.buildBody, so the multi-breakpoint policy and TTL apply there
// too (issue #545 is for BOTH the direct API and Vertex). A big turn must still
// stay within the 4-breakpoint ceiling on the vertex adapter.
func TestAnthropicBreakpointPolicySharedOnVertex(t *testing.T) {
	const M = 12
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "q"},
		bigAssistantTurn(M, "V"),
	}
	msgs = append(msgs, bigToolResults(M, "V", "vres")...)
	req := CompletionRequest{CacheTTL: "1h", Messages: msgs}
	raw, err := buildBodyBytes(anthropicAdapter{vertex: true}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	bps := anthropicBreakpoints(t, raw)
	if len(bps) > maxAnthropicBreakpoints {
		t.Errorf("vertex: emitted %d breakpoints (>4)", len(bps))
	}
	for _, bp := range bps {
		if bp.ttl != "1h" {
			t.Errorf("vertex 1h breakpoint tier=%s has ttl %q; want 1h", bp.tier, bp.ttl)
		}
	}
}

// ===========================================================================
// Gap B — buildRequest wiring + capability gating + json:"-" leak guard
// ===========================================================================

// TestBuildRequestCacheTTLFromConfig verifies buildRequest resolves CacheTTL from
// the model config: default/5m → "", 1h → "1h", off → "off".
func TestBuildRequestCacheTTLFromConfig(t *testing.T) {
	cases := []struct {
		cfg  string
		want string
	}{
		{"", ""},
		{"5m", ""},
		{"1h", "1h"},
		{"off", "off"},
		{"none", "off"},
		{"2h", ""}, // typo → default
	}
	for _, tc := range cases {
		conn := NewModelConnection(
			&config.ProviderConnection{APIType: "anthropic"},
			&config.ModelConfig{
				Model:    "claude-x",
				CacheTTL: tc.cfg,
			},
		)
		req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
		if req.CacheTTL != tc.want {
			t.Errorf("anthropic config CacheTTL=%q → req.CacheTTL=%q, want %q", tc.cfg, req.CacheTTL, tc.want)
		}
	}
}

// TestBuildRequestCacheTTLOffForNonBreakpointProviders: a provider that does NOT
// advertise the CacheControlBreakpoints capability (e.g. OpenAI-compatible, Z.AI)
// must have CacheTTL forced to "off" even when the config asks for "1h", so the
// directive never reaches a backend that does not understand it. (The OpenAI/Gemini
// adapters ignore CacheTTL regardless, but the resolved value must still be "off"
// to document the capability gate.)
func TestBuildRequestCacheTTLOffForNonBreakpointProviders(t *testing.T) {
	for _, apiType := range []string{"openai", "zai", "openrouter"} {
		conn := NewModelConnection(
			&config.ProviderConnection{
				APIType:  apiType,
				Endpoint: "https://example.test/v1/chat/completions",
			},
			&config.ModelConfig{
				Model:    "m",
				CacheTTL: "1h",
			},
		)
		req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
		if req.CacheTTL != "off" {
			t.Errorf("%s provider with config 1h → req.CacheTTL=%q, want \"off\" (no CacheControlBreakpoints capability)", apiType, req.CacheTTL)
		}
	}
}

// TestBuildRequestCacheTTLHonoredOnVertexAnthropic: Claude-on-Vertex advertises
// CacheControlBreakpoints, so the config TTL is honored (not forced off).
func TestBuildRequestCacheTTLHonoredOnVertexAnthropic(t *testing.T) {
	conn := NewModelConnection(
		&config.ProviderConnection{
			APIType:  "vertex-anthropic",
			Project:  "p",
			Location: "us-central1",
		},
		&config.ModelConfig{
			Model:    "claude-x",
			CacheTTL: "1h",
		},
	)
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
	if req.CacheTTL != "1h" {
		t.Errorf("vertex-anthropic config 1h → req.CacheTTL=%q, want \"1h\"", req.CacheTTL)
	}
}

// TestBuildRequestCacheTTLNilConfig: a connection whose Config is nil must not
// panic in buildRequest (AnthropicCacheTTL is nil-safe) and must resolve to the
// default.
func TestBuildRequestCacheTTLNilConfig(t *testing.T) {
	conn := NewModelConnection(&config.ProviderConnection{APIType: "anthropic"}, &config.ModelConfig{Model: "claude-x"})
	conn.Config = nil // simulate a nil config
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, nil, nil)
	if req.CacheTTL != "" {
		t.Errorf("nil config → req.CacheTTL=%q, want \"\" (default, no panic)", req.CacheTTL)
	}
}

// TestCompletionRequestCacheTTLNotOnOpenAIWire: CacheTTL is tagged json:"-"
// because the OpenAI-compatible adapter marshals the WHOLE CompletionRequest
// (adapter.go openAIAdapter.buildBody → encodeJSON(buf, req)). If the tag were
// missing, cache_ttl would leak onto every OpenAI/Z.AI/OpenRouter/Gemini-compat
// request and could 400 a strict backend. This guards the json:"-" invariant.
func TestCompletionRequestCacheTTLNotOnOpenAIWire(t *testing.T) {
	req := CompletionRequest{
		CacheTTL: "1h",
		Model:    "gpt-x",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}
	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if bytes.Contains(raw, []byte("cache_ttl")) || bytes.Contains(raw, []byte(`"ttl"`)) {
		t.Errorf("CacheTTL leaked onto the OpenAI-compatible wire: %s", raw)
	}
}

// TestCompletionRequestCacheTTLDoesNotSerialize directly exercises the json:"-"
// tag: marshaling a CompletionRequest with CacheTTL set must never emit the field.
func TestCompletionRequestCacheTTLDoesNotSerialize(t *testing.T) {
	b, err := json.Marshal(CompletionRequest{CacheTTL: "1h", Model: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("cache_ttl")) {
		t.Errorf("CompletionRequest.CacheTTL serialized; json:\"-\" was removed: %s", b)
	}
}
