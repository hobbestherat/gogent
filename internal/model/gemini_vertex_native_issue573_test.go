package model

// This suite exercises the #573 fix for the vertex-native (alias "gemini")
// adapter on Gemini 3.x. Two independent defects each yield an HTTP 400 from
// Vertex and are covered here:
//
//  1. Nullable-union "type" arrays (e.g. {"type":["string","null"],"enum":[...]})
//     reach the wire as a repeated type, which Vertex rejects ("Proto field is
//     not repeating, cannot start list"). Fix 1 collapses them to the scalar
//     Gemini type + "nullable": true inside geminiSchema/uppercaseSchemaTypes.
//  2. Gemini 3.x attaches a part-level thoughtSignature to functionCall parts
//     that MUST be echoed back when the call is replayed in conversation history,
//     else Vertex 400s ("Function call is missing a thought_signature"). Fix 2
//     captures it on parse, carries it on ToolCall, and re-emits it on build.
//
// All tests are unit-level (no live API), per acceptance criterion 7.

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Schema-walk helpers
// ---------------------------------------------------------------------------

// anyTypeIsArray reports whether a decoded JSON-Schema value still carries ANY
// "type" whose value is an array — the exact shape Vertex 3.x rejects with a
// 400 ("cannot start list"). A fully-normalized Gemini schema has none.
func anyTypeIsArray(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		if _, ok := t["type"].([]interface{}); ok {
			return true
		}
		if _, ok := t["type"].([]string); ok { // Go-native (pre round-trip) defensive
			return true
		}
		for _, val := range t {
			if anyTypeIsArray(val) {
				return true
			}
		}
	case []interface{}:
		for _, el := range t {
			if anyTypeIsArray(el) {
				return true
			}
		}
	}
	return false
}

// nullableOf reads a property node's "nullable" as a bool, failing the test on a
// non-bool shape (so a future regression that emits nullable as a string is
// caught rather than silently passing).
func nullableOf(t *testing.T, node map[string]interface{}) bool {
	t.Helper()
	v, ok := node["nullable"]
	if !ok {
		t.Fatalf("nullable missing on node: %#v", node)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("nullable = %#v, want bool", v)
	}
	return b
}

// ===========================================================================
// Fix 1 — collapse nullable-union "type" to scalar + nullable:true
// ===========================================================================

// TestIssue573GeminiSchemaCollapsesNullableStringUnionEnum is the headline Fix-1
// case: the exact grep.output_mode shape that triggered the "cannot start list"
// 400. After normalization it must be a scalar STRING type with the enum
// preserved (lowercase — enum values are data, not type names) and nullable set.
func TestIssue573GeminiSchemaCollapsesNullableStringUnionEnum(t *testing.T) {
	in := map[string]interface{}{
		"type": []interface{}{"string", "null"},
		"enum": []interface{}{"content", "files_with_matches", "count"},
	}
	out := geminiSchema(in).(map[string]interface{})

	requireStringType(t, out, "STRING")
	if !nullableOf(t, out) {
		t.Fatalf("nullable = false, want true")
	}
	// Enum members are preserved verbatim (not upper-cased) and in order.
	enum, ok := out["enum"].([]interface{})
	if !ok {
		t.Fatalf("enum = %#v, want slice", out["enum"])
	}
	want := []string{"content", "files_with_matches", "count"}
	if len(enum) != len(want) {
		t.Fatalf("enum len = %d, want %d (%v)", len(enum), len(want), enum)
	}
	for i, w := range want {
		if s, ok := enum[i].(string); !ok || s != w {
			t.Fatalf("enum[%d] = %v, want %q", i, enum[i], w)
		}
	}
	// No array-typed "type" survives — the property that actually caused the 400.
	if anyTypeIsArray(out) {
		t.Fatalf("normalized schema still has an array type (would 400):\n%#v", out)
	}
}

// TestIssue573GeminiSchemaCollapsesAllScalarNullableUnions covers the nullable
// union variants the default read/grep tools actually declare (string, integer,
// boolean) plus the remaining scalar kinds, so the collapse is not string-only.
func TestIssue573GeminiSchemaCollapsesAllScalarNullableUnions(t *testing.T) {
	cases := []struct {
		name    string
		members []string
		want    string
	}{
		{"string", []string{"string", "null"}, "STRING"},
		{"integer", []string{"integer", "null"}, "INTEGER"},
		{"boolean", []string{"boolean", "null"}, "BOOLEAN"},
		{"number", []string{"number", "null"}, "NUMBER"},
		{"array", []string{"array", "null"}, "ARRAY"},
		{"object", []string{"object", "null"}, "OBJECT"},
		// order-insensitive: null may lead.
		{"null-first", []string{"null", "string"}, "STRING"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			union := make([]interface{}, len(tc.members))
			for i, m := range tc.members {
				union[i] = m
			}
			out := geminiSchema(map[string]interface{}{"type": union}).(map[string]interface{})
			requireStringType(t, out, tc.want)
			if !nullableOf(t, out) {
				t.Fatalf("nullable = false, want true for %s", tc.name)
			}
		})
	}
}

// TestIssue573GeminiSchemaCollapsesRecursively verifies the collapse applies at
// any depth (nested object properties, array items), not only at the top level —
// acceptance criterion 4 ("recursively").
func TestIssue573GeminiSchemaCollapsesRecursively(t *testing.T) {
	in := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"tags": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": []interface{}{"string", "null"}},
			},
			"meta": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"label": map[string]interface{}{"type": []interface{}{"integer", "null"}},
				},
			},
		},
	}
	out := geminiSchema(in)

	tags := prop(t, out.(map[string]interface{}), "tags")
	requireStringType(t, tags, "ARRAY")
	items, ok := tags["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("tags.items = %#v, want map", tags["items"])
	}
	requireStringType(t, items, "STRING")
	if !nullableOf(t, items) {
		t.Fatalf("tags.items nullable = false, want true (nested collapse failed)")
	}

	meta := prop(t, out.(map[string]interface{}), "meta")
	requireStringType(t, meta, "OBJECT")
	label := prop(t, meta, "label")
	requireStringType(t, label, "INTEGER")
	if !nullableOf(t, label) {
		t.Fatalf("meta.label nullable = false, want true (deep collapse failed)")
	}
	if anyTypeIsArray(out) {
		t.Fatalf("recursive schema still has an array type (would 400):\n%#v", out)
	}
}

// TestIssue573GeminiSchemaNoArrayTypeSurvivesRealGrep runs the verbatim default
// grep schema (every nullable field: path, output_mode, include,
// case_insensitive, max_results) and asserts NO property anywhere keeps an array
// type. This is the strongest unit-level form of acceptance criterion 1.
func TestIssue573GeminiSchemaNoArrayTypeSurvivesRealGrep(t *testing.T) {
	out := geminiSchema(grepToolSchema())
	if anyTypeIsArray(out) {
		t.Fatalf("grep schema still has an array-typed property after normalization "+
			"(Vertex would 400 with \"cannot start list\"):\n%v", out)
	}
}

// TestIssue573GeminiSchemaDeepCopyDoesNotMutateInput is the no-shared-mutation
// gate (acceptance #4 / criterion 3): geminiSchema must deep-copy, so the
// caller's shared Parameters map — re-used across turns and providers and built
// concurrently — is only ever READ. The Go-native []string type must survive
// untouched.
func TestIssue573GeminiSchemaDeepCopyDoesNotMutateInput(t *testing.T) {
	schema := grepToolSchema()
	outMode := schema["properties"].(map[string]interface{})["output_mode"].(map[string]interface{})

	_ = geminiSchema(schema)

	got, ok := outMode["type"].([]string)
	if !ok {
		t.Fatalf("shared input type = %#v, want unchanged Go-native []string (deep copy mutated it)", outMode["type"])
	}
	if len(got) != 2 || got[0] != "string" || got[1] != "null" {
		t.Fatalf("shared input type = %#v, want [string null]", got)
	}
	// required / additionalProperties untouched too.
	if req, _ := schema["required"].([]string); len(req) != 6 {
		t.Fatalf("shared input required mutated: %#v", schema["required"])
	}
}

// TestIssue573GeminiSchemaPlainScalarUnaffected is a regression guard: a plain
// scalar "type" is still just upper-cased; the union branch must not interfere.
func TestIssue573GeminiSchemaPlainScalarUnaffected(t *testing.T) {
	out := geminiSchema(map[string]interface{}{"type": "string"}).(map[string]interface{})
	requireStringType(t, out, "STRING")
	if _, hasNullable := out["nullable"]; hasNullable {
		t.Fatalf("plain scalar type gained a spurious nullable: %#v", out)
	}
}

// TestIssue573GeminiSchemaPreExistingNullableSurvivesWithoutUnion: a node that
// already declares nullable:true with a plain scalar type is left intact (the
// union-clobber path is not taken, so an existing nullable is preserved).
func TestIssue573GeminiSchemaPreExistingNullableSurvivesWithoutUnion(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type":     "string",
		"nullable": true,
	}).(map[string]interface{})
	requireStringType(t, out, "STRING")
	if !nullableOf(t, out) {
		t.Fatalf("pre-existing nullable:true was dropped: %#v", out)
	}
}

// ---------------------------------------------------------------------------
// Fix 1 — documented edge cases (pinned, not "fixed")
// ---------------------------------------------------------------------------

// TestIssue573GeminiSchemaMultiNonNullUnionLeftAsArray pins the documented
// out-of-scope case: a genuine multi-non-null union (no "null" member, not in
// any default tool) is NOT collapsed and stays an array (Vertex would still
// reject it; representing it needs anyOf, which #573 does not introduce). The
// test documents the current behavior so a future change is intentional.
func TestIssue573GeminiSchemaMultiNonNullUnionLeftAsArray(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type": []interface{}{"string", "integer"},
	}).(map[string]interface{})
	if _, ok := out["type"].([]interface{}); !ok {
		t.Fatalf("multi-non-null union type = %#v, want left as array (out-of-scope)", out["type"])
	}
	if _, hasNullable := out["nullable"]; hasNullable {
		t.Fatalf("multi-non-null union gained nullable (only nullable unions are collapsed): %#v", out)
	}
}

// TestIssue573GeminiSchemaMultiNonNullNullableDropsNullButStaysArray pins the
// subtle mixed case ["string","integer","null"]: the null is dropped and
// nullable:true is set, but with >1 non-null survivor the type stays an array
// (still unrepresentable as a single scalar). Documents current behavior.
func TestIssue573GeminiSchemaMultiNonNullNullableDropsNullButStaysArray(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type": []interface{}{"string", "integer", "null"},
	}).(map[string]interface{})
	got, ok := out["type"].([]interface{})
	if !ok {
		t.Fatalf("type = %#v, want surviving array [string integer]", out["type"])
	}
	if len(got) != 2 {
		t.Fatalf("surviving members = %v, want null dropped ([string integer])", got)
	}
	for _, m := range got {
		if s, _ := m.(string); s == "null" {
			t.Fatalf("null member survived: %v", got)
		}
	}
	if !nullableOf(t, out) {
		t.Fatalf("nullable = false, want true (a null was dropped from the union)")
	}
}

// TestIssue573GeminiSchemaDegenerateNullOnlyLeftAsIs pins the degenerate case:
// a ["null"]-only type is left unchanged (dropNullFromType reports no change
// when nothing non-null survives), so no nullable is added.
func TestIssue573GeminiSchemaDegenerateNullOnlyLeftAsIs(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type": []interface{}{"null"},
	}).(map[string]interface{})
	if _, ok := out["type"].([]interface{}); !ok {
		t.Fatalf("degenerate [null] type = %#v, want left as-is", out["type"])
	}
	if _, hasNullable := out["nullable"]; hasNullable {
		t.Fatalf("degenerate [null] gained nullable: %#v", out)
	}
}

// ---------------------------------------------------------------------------
// Fix 1 — integration through buildBody (tool params + structured output)
// ---------------------------------------------------------------------------

// TestIssue573GeminiBuildBodyGrepSchemaEmitsNoArrayType is acceptance criterion
// 1 at the wire level: the default grep tool, run through the real build path,
// emits a tools schema with no array-typed property (so the first turn returns
// 200, not the "cannot start list" 400).
func TestIssue573GeminiBuildBodyGrepSchemaEmitsNoArrayType(t *testing.T) {
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "search"}},
		Tools:    []ToolDef{grepToolDef()},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	decls := got["tools"].([]interface{})[0].(map[string]interface{})["functionDeclarations"].([]interface{})
	params := decls[0].(map[string]interface{})["parameters"].(map[string]interface{})
	if anyTypeIsArray(params) {
		t.Fatalf("emitted grep parameters still contain an array type (would 400):\n%s", raw)
	}
	// The enum-bearing output_mode in particular is scalar + nullable.
	outMode := params["properties"].(map[string]interface{})["output_mode"].(map[string]interface{})
	requireStringType(t, outMode, "STRING")
	if !nullableOf(t, outMode) {
		t.Fatalf("output_mode nullable = false, want true")
	}
}

// TestIssue573GeminiBuildBodyCollapsesEveryGrepNullableField asserts each
// nullable-union field the default grep tool declares (path/output_mode/include/
// case_insensitive/max_results) is collapsed to scalar + nullable on the wire.
func TestIssue573GeminiBuildBodyCollapsesEveryGrepNullableField(t *testing.T) {
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Tools:    []ToolDef{grepToolDef()},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	json.Unmarshal(raw, &got)
	params := got["tools"].([]interface{})[0].(map[string]interface{})["functionDeclarations"].([]interface{})[0].(map[string]interface{})["parameters"].(map[string]interface{})["properties"].(map[string]interface{})

	for field, wantType := range map[string]string{
		"path":             "STRING",
		"output_mode":      "STRING",
		"include":          "STRING",
		"case_insensitive": "BOOLEAN",
		"max_results":      "INTEGER",
	} {
		node, ok := params[field].(map[string]interface{})
		if !ok {
			t.Fatalf("field %q missing/not a map: %#v", field, params[field])
		}
		requireStringType(t, node, wantType)
		if !nullableOf(t, node) {
			t.Errorf("field %q nullable = false, want true", field)
		}
	}
}

// TestIssue573GeminiResponseSchemaCollapsesNullableUnion is acceptance criterion
// 3: the structured-output path (generationConfig.responseSchema) shares
// geminiSchema, so a nullable union in a response schema is also collapsed —
// otherwise structured output would 400 with the same "cannot start list".
func TestIssue573GeminiResponseSchemaCollapsesNullableUnion(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"category": map[string]interface{}{
				"type": []interface{}{"string", "null"},
				"enum": []interface{}{"a", "b"},
			},
		},
	}
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages:       []Message{{Role: RoleUser, Content: "classify"}},
		ResponseFormat: JSONSchemaResponseFormat("result", schema),
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	json.Unmarshal(raw, &got)
	gen := got["generationConfig"].(map[string]interface{})
	rs := gen["responseSchema"].(map[string]interface{})
	if anyTypeIsArray(rs) {
		t.Fatalf("responseSchema still has an array type (structured output would 400):\n%s", raw)
	}
	cat := rs["properties"].(map[string]interface{})["category"].(map[string]interface{})
	requireStringType(t, cat, "STRING")
	if !nullableOf(t, cat) {
		t.Fatalf("responseSchema category nullable = false, want true")
	}
}

// ===========================================================================
// Fix 2 — thoughtSignature capture / carry / re-emit
// ===========================================================================

// signedFunctionCallBody builds a Gemini response whose single functionCall
// part carries a part-level thoughtSignature (sibling to functionCall), plus a
// leading thought part so the shape matches a real Gemini 3.x thinking turn.
func signedFunctionCallBody(sig string) []byte {
	return []byte(`{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"planning","thought":true},` +
		`{"functionCall":{"name":"grep","args":{"pattern":"foo"},"id":"c1"},"thoughtSignature":"` + sig + `"}` +
		`]},"finishReason":"STOP"}]}`)
}

// TestIssue573ParseResponseCapturesThoughtSignature: parseResponse must capture
// the part-level thoughtSignature onto the ToolCall so the agent loop can echo
// it later (acceptance #5 — capture half).
func TestIssue573ParseResponseCapturesThoughtSignature(t *testing.T) {
	resp, err := (geminiAdapter{}).parseResponse(signedFunctionCallBody("c2lnLXZlcnRleA=="))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	if got := resp.ToolCalls[0].ThoughtSignature; got != "c2lnLXZlcnRleA==" {
		t.Fatalf("ThoughtSignature = %q, want captured signature", got)
	}
	// The rest of the call is unaffected.
	tc := resp.ToolCalls[0]
	if tc.ID != "c1" || tc.Function.Name != "grep" || tc.Function.Arguments != `{"pattern":"foo"}` {
		t.Fatalf("ToolCall = %+v, want grep/c1", tc)
	}
}

// TestIssue573ParseResponseEmptySignatureWhenAbsent: a functionCall part without
// a thoughtSignature yields an empty (omittable) signature, not an error.
func TestIssue573ParseResponseEmptySignatureWhenAbsent(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"grep","args":{},"id":"c1"}}` +
		`]},"finishReason":"STOP"}]}`)
	resp, err := (geminiAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got := resp.ToolCalls[0].ThoughtSignature; got != "" {
		t.Fatalf("ThoughtSignature = %q, want empty when absent", got)
	}
}

// TestIssue573ParseStreamCapturesThoughtSignature: the DEFAULT delivery mode is
// streaming, so the signature must also be captured on the streaming parse path
// and carried on the terminal Done event's ToolCalls.
func TestIssue573ParseStreamCapturesThoughtSignature(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"plan","thought":true}],"role":"model"}}]}`,
		``,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"grep","args":{"pattern":"foo"},"id":"c1"},"thoughtSignature":"c3RyZWFtLXNpZw=="}],"role":"model"},"finishReason":"STOP"}]}`,
		``,
	}, "\n"))
	ch := make(chan StreamResponse, 10)
	if _, _, err := (geminiAdapter{}).parseStream(stream, ch); err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	close(ch)

	var done *StreamResponse
	for ev := range ch {
		if ev.Done {
			d := ev
			done = &d
		}
	}
	if done == nil {
		t.Fatal("no terminal Done event")
	}
	if len(done.ToolCalls) != 1 {
		t.Fatalf("Done ToolCalls len = %d, want 1", len(done.ToolCalls))
	}
	if got := done.ToolCalls[0].ThoughtSignature; got != "c3RyZWFtLXNpZw==" {
		t.Fatalf("streamed ThoughtSignature = %q, want captured", got)
	}
}

// TestIssue573GeminiPartsReEmitsThoughtSignature: geminiParts must emit the
// signature as a sibling of functionCall on the replayed model turn.
func TestIssue573GeminiPartsReEmitsThoughtSignature(t *testing.T) {
	role, parts := geminiParts(Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:               "c1",
			Type:             "function",
			Function:         FunctionCall{Name: "grep", Arguments: "{}"},
			ThoughtSignature: "cmVlbWl0LXNpZw==",
		}},
	})
	if role != "model" {
		t.Fatalf("role = %q, want model", role)
	}
	if len(parts) != 1 || parts[0].FunctionCall == nil {
		t.Fatalf("parts = %+v, want one functionCall part", parts)
	}
	if got := parts[0].ThoughtSignature; got != "cmVlbWl0LXNpZw==" {
		t.Fatalf("re-emitted ThoughtSignature = %q, want the carried signature", got)
	}
}

// TestIssue573GeminiPartsOmitsThoughtSignatureWhenEmpty: a tool call with no
// signature must NOT add a thoughtSignature key — byte-identical to pre-#573 so
// non-Gemini-origin tool calls (and turns before any signature existed) are
// unchanged (acceptance #6).
func TestIssue573GeminiPartsOmitsThoughtSignatureWhenEmpty(t *testing.T) {
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{{
				ID:       "c1",
				Type:     "function",
				Function: FunctionCall{Name: "grep", Arguments: "{}"},
				// ThoughtSignature intentionally empty.
			}},
		}},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if strings.Contains(string(raw), "thoughtSignature") {
		t.Fatalf("empty-signature turn still emits thoughtSignature (not byte-identical):\n%s", raw)
	}
}

// TestIssue573ThoughtSignatureRoundTripEndToEnd is THE acceptance test for
// Fix 2 (criterion 5): parse a signed functionCall -> carry it on a ToolCall in
// a replayed assistant turn -> build the next request -> the echoed functionCall
// part in history re-emits the SAME signature. This is what unblocks the second
// turn (otherwise "missing thought_signature" 400).
func TestIssue573ThoughtSignatureRoundTripEndToEnd(t *testing.T) {
	const sig = "Z2VtaW5pLTMueC1zaWc="
	resp, err := (geminiAdapter{}).parseResponse(signedFunctionCallBody(sig))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	captured := resp.ToolCalls[0].ThoughtSignature
	if captured != sig {
		t.Fatalf("capture failed: %q != %q", captured, sig)
	}

	// Replay the assistant tool-call turn in conversation history (exactly as the
	// agent loop appends resp.ToolCalls to the transcript), plus the tool result.
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{
			{Role: RoleUser, Content: "run grep"},
			{Role: RoleAssistant, ToolCalls: resp.ToolCalls},
			{Role: RoleTool, Name: "grep", ToolCallID: "c1", Content: `{"path":"foo.go"}`},
		},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	json.Unmarshal(raw, &got)
	contents := got["contents"].([]interface{})

	// Find the replayed model functionCall part and assert the signature rides on it.
	var foundSig interface{}
	var sawFunctionCall bool
	for _, c := range contents {
		turn := c.(map[string]interface{})
		if turn["role"] != "model" {
			continue
		}
		for _, p := range turn["parts"].([]interface{}) {
			part := p.(map[string]interface{})
			if part["functionCall"] != nil {
				sawFunctionCall = true
				foundSig = part["thoughtSignature"]
			}
		}
	}
	if !sawFunctionCall {
		t.Fatalf("no replayed functionCall part in history:\n%s", raw)
	}
	if gotSig, _ := foundSig.(string); gotSig != sig {
		t.Fatalf("replayed functionCall thoughtSignature = %v, want %q (round-trip lost it):\n%s", foundSig, sig, raw)
	}
}

// TestIssue573MultipleToolCallsEachCarrySignature: when a turn has several
// functionCall parts, each carries its OWN signature and each is re-emitted
// (Gemini 3.x requires the signature per functionCall part).
func TestIssue573MultipleToolCallsEachCarrySignature(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"a","args":{},"id":"1"},"thoughtSignature":"c2lnMQ=="},` +
		`{"functionCall":{"name":"b","args":{},"id":"2"},"thoughtSignature":"c2lnMg=="}` +
		`]},"finishReason":"STOP"}]}`)
	resp, err := (geminiAdapter{}).parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2", len(resp.ToolCalls))
	}
	want := map[string]string{"a": "c2lnMQ==", "b": "c2lnMg=="}
	for _, tc := range resp.ToolCalls {
		if sig, ok := want[tc.Function.Name]; !ok || tc.ThoughtSignature != sig {
			t.Fatalf("ToolCall %q sig = %q, want %q", tc.Function.Name, tc.ThoughtSignature, want[tc.Function.Name])
		}
	}

	// Re-emit both.
	raw, err := buildBodyBytes(geminiAdapter{}, CompletionRequest{
		Messages: []Message{{Role: RoleAssistant, ToolCalls: resp.ToolCalls}},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	for _, sig := range want {
		if !strings.Contains(string(raw), sig) {
			t.Fatalf("signature %q not re-emitted in history:\n%s", sig, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix 2 — provider scoping: ThoughtSignature is off-wire (json:"-")
// ---------------------------------------------------------------------------

// TestIssue573ToolCallThoughtSignatureOffWireDirect: a ToolCall with a signature
// marshals with NO thoughtSignature/thought_signature key, so the OpenAI/Z.AI/
// OpenRouter tool_calls wire (where a stray field can be rejected by strict
// APIs) is byte-identical to today (acceptance #6).
func TestIssue573ToolCallThoughtSignatureOffWireDirect(t *testing.T) {
	b, err := json.Marshal(ToolCall{
		ID:               "c1",
		Type:             "function",
		Function:         FunctionCall{Name: "grep", Arguments: "{}"},
		ThoughtSignature: "b2ZmLXdpcmUtc2ln",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(strings.ToLower(s), "thoughtsignature") || strings.Contains(s, "thought_signature") {
		t.Fatalf("ToolCall leaked ThoughtSignature onto the wire (must be json:\"-\"):\n%s", s)
	}
}

// TestIssue573ToolCallThoughtSignatureOffWireViaMessage: the same guarantee
// holds through Message.MarshalJSON (the transcript/Anthropic/OpenAI wire path),
// which serializes ToolCalls via struct tags.
func TestIssue573ToolCallThoughtSignatureOffWireViaMessage(t *testing.T) {
	b, err := json.Marshal(Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:               "c1",
			Type:             "function",
			Function:         FunctionCall{Name: "grep", Arguments: "{}"},
			ThoughtSignature: "b2ZmLXdpcmUtc2ln",
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(strings.ToLower(s), "thoughtsignature") || strings.Contains(s, "thought_signature") {
		t.Fatalf("Message leaked ThoughtSignature onto the wire:\n%s", s)
	}
}

// TestIssue573ThoughtSignatureNotPersistedAcrossTranscript pins the documented
// session-reopen limitation (#573 open question #1, matching the Anthropic
// ThinkingSignature precedent): ThoughtSignature is json:"-", so a transcript
// round-trip (marshal -> unmarshal) drops it. A session reopened mid-loop loses
// the signature. This test documents that behavior so a future move to a
// persisted-but-stripped-on-send field is a deliberate change.
func TestIssue573ThoughtSignatureNotPersistedAcrossTranscript(t *testing.T) {
	m := Message{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:               "c1",
			Type:             "function",
			Function:         FunctionCall{Name: "grep", Arguments: "{}"},
			ThoughtSignature: "dHJhbnNjcmlwdC1zaWc=",
		}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.ToolCalls) != 1 {
		t.Fatalf("round-trip ToolCalls len = %d, want 1", len(back.ToolCalls))
	}
	if back.ToolCalls[0].ThoughtSignature != "" {
		t.Fatalf("ThoughtSignature survived transcript round-trip = %q, want empty (json:\"-\" not persisted)",
			back.ToolCalls[0].ThoughtSignature)
	}
}
