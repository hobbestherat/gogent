package model

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"gogent/internal/config"
)

// This suite exercises the #567 fix: the Anthropic (and Vertex-Anthropic) adapter
// must deep-copy and normalize each strict tool's input_schema so no property
// combines a nullable union "type" (e.g. ["string","null"]) with an "enum" — the
// exact combination Anthropic strict validation rejects with HTTP 400 before the
// model runs, which currently kills every first turn to a claude-* model.
//
// The oracle for "would Anthropic strict accept this?" is hasNullableUnionEnum
// below: it walks a decoded (or Go-native) schema and reports whether ANY node
// has both a null-bearing union "type" and an "enum". A passing request has none.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// grepToolSchema reproduces the real grep registration schema verbatim
// (internal/gogent/gogent.go:687-699): output_mode is the ONLY property that
// combines a nullable union with an enum; the other nullable fields (path,
// include, case_insensitive, max_results) have no enum and must be left
// untouched. The slice types are Go-native ([]string) exactly as the registry
// builds them, so the deep-copy / no-mutation tests exercise the real shapes.
func grepToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern": map[string]interface{}{"type": "string", "description": "Go regular expression to search for."},
			"path":    map[string]interface{}{"type": []string{"string", "null"}, "description": "search path"},
			"output_mode": map[string]interface{}{
				"type":        []string{"string", "null"},
				"enum":        []string{"content", "files_with_matches", "count"},
				"description": "Result shape (default/null content).",
			},
			"include":          map[string]interface{}{"type": []string{"string", "null"}, "description": "name glob"},
			"case_insensitive": map[string]interface{}{"type": []string{"boolean", "null"}, "description": "ignore case"},
			"max_results":      map[string]interface{}{"type": []string{"integer", "null"}, "description": "cap"},
		},
		"required":             []string{"pattern", "path", "output_mode", "include", "case_insensitive", "max_results"},
		"additionalProperties": false,
	}
}

func grepToolDef() ToolDef {
	return ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        "grep",
			Description: "Search file contents across the workspace for a regular expression.",
			Parameters:  grepToolSchema(),
			Strict:      true,
		},
	}
}

// syntheticMCPNestedSchema is a strict tool schema (the shape a user-supplied
// MCP tool might contribute) whose nullable-union+enum property is buried deep:
// top-level properties → an array-typed property's items (tuple form) → that
// item's properties → an anyOf branch. The normalizer must reach and fix it
// wherever it appears (acceptance #3 / regression for MCP + future tools).
func syntheticMCPNestedSchema() map[string]interface{} {
	deepBuggy := map[string]interface{}{
		"type":        []interface{}{"string", "null"},
		"enum":        []interface{}{"red", "green", "blue"},
		"description": "deep nullable enum under items/properties/anyOf",
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"matrix": map[string]interface{}{
				"type": []interface{}{"array", "null"},
				"items": []interface{}{ // tuple-form items: a list of schemas
					map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"color": map[string]interface{}{
								"anyOf": []interface{}{deepBuggy, map[string]interface{}{"type": "null"}},
							},
						},
					},
				},
			},
		},
		"required":             []string{"matrix"},
		"additionalProperties": false,
	}
}

// ---------------------------------------------------------------------------
// Oracle: "would Anthropic strict reject this schema?"
// ---------------------------------------------------------------------------

// typeSliceContainsNull reports whether v is a JSON-Schema "type" value that is a
// union array — either a Go-native []string (as the registry builds) or a decoded
// []interface{} (after a JSON round-trip) — containing the member "null".
func typeSliceContainsNull(v interface{}) bool {
	switch s := v.(type) {
	case []string:
		for _, m := range s {
			if m == "null" {
				return true
			}
		}
	case []interface{}:
		for _, m := range s {
			if str, ok := m.(string); ok && str == "null" {
				return true
			}
		}
	}
	return false
}

// hasNullableUnionEnum reports whether v — a decoded or Go-native JSON-Schema
// node — contains ANY property that combines a null-bearing union "type" with an
// "enum". That combination is precisely what Anthropic strict rejects (#567), so
// a request whose every tool schema returns false here would reach the model.
func hasNullableUnionEnum(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		if _, hasEnum := t["enum"]; hasEnum && typeSliceContainsNull(t["type"]) {
			return true
		}
		for _, val := range t {
			if hasNullableUnionEnum(val) {
				return true
			}
		}
	case []interface{}:
		for _, el := range t {
			if hasNullableUnionEnum(el) {
				return true
			}
		}
	}
	return false
}

// prop walks object.properties[path...] to fetch a nested property map, failing
// the test if any segment is missing or the wrong type. Keeps assertions terse.
func prop(t *testing.T, schema map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	cur := schema
	for _, k := range path {
		props, ok := cur["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("no properties at %v: %#v", path, cur)
		}
		cur, ok = props[k].(map[string]interface{})
		if !ok {
			t.Fatalf("property %q not a map: %#v", k, props[k])
		}
	}
	return cur
}

// requireStringType asserts a property's "type" is the scalar string want (the
// post-normalization shape for a collapsed nullable enum).
func requireStringType(t *testing.T, p map[string]interface{}, want string) {
	t.Helper()
	got, ok := p["type"].(string)
	if !ok {
		t.Fatalf("type = %#v, want scalar string %q", p["type"], want)
	}
	if got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Acceptance #1 — grep schema through buildBody no longer 400s (both paths)
// ---------------------------------------------------------------------------

func TestIssue567AnthropicNormalizesGrepOutputMode(t *testing.T) {
	req := CompletionRequest{
		Model:    "claude-opus-4-8",
		Messages: []Message{{Role: RoleUser, Content: "search the repo"}},
		Tools:    []ToolDef{grepToolDef()},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var got struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Strict      bool                   `json:"strict"`
			InputSchema map[string]interface{} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(got.Tools))
	}
	tool := got.Tools[0]

	// Acceptance #2: grep stays strict:true.
	if !tool.Strict {
		t.Fatalf("grep strict = false, want true (must remain Strict:true)")
	}

	schema := tool.InputSchema

	// Acceptance #1 / oracle: no property anywhere combines null-union + enum.
	if hasNullableUnionEnum(schema) {
		t.Fatalf("Anthropic input_schema still contains a nullable-union+enum "+
			"(would HTTP 400 under strict validation):\n%s", raw)
	}

	// output_mode collapsed to plain "string" + enum intact (the verified-200 shape).
	outMode := prop(t, schema, "output_mode")
	requireStringType(t, outMode, "string")
	enum, ok := outMode["enum"].([]interface{})
	if !ok || len(enum) != 3 {
		t.Fatalf("output_mode enum = %#v, want 3 members", outMode["enum"])
	}
	for i, want := range []string{"content", "files_with_matches", "count"} {
		if got, ok := enum[i].(string); !ok || got != want {
			t.Errorf("output_mode enum[%d] = %v, want %q", i, enum[i], want)
		}
	}
	if d, ok := outMode["description"].(string); !ok || d == "" {
		t.Errorf("output_mode description lost: %#v", outMode["description"])
	}

	// output_mode stays in required (the conservative all-required-strict choice).
	reqd, ok := schema["required"].([]interface{})
	if !ok {
		t.Fatalf("required = %#v, want array", schema["required"])
	}
	found := false
	for _, r := range reqd {
		if r == "output_mode" {
			found = true
		}
	}
	if !found {
		t.Errorf("output_mode dropped from required: %#v", reqd)
	}

	// additionalProperties survives (strict needs a closed object).
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
	}

	// No over-normalization: the non-enum nullable fields stay as ["T","null"] —
	// those are valid on Anthropic strict (issue: ["string","null"] no enum → 200)
	// and MUST be preserved so the OpenAI/Gemini wires (which share the source
	// schema) keep working.
	for name, wantScalar := range map[string]string{
		"path": "string", "include": "string",
		"case_insensitive": "boolean", "max_results": "integer",
	} {
		p := prop(t, schema, name)
		union, ok := p["type"].([]interface{})
		if !ok || len(union) != 2 {
			t.Errorf("%s type = %#v, want preserved 2-member union [%q,null]", name, p["type"], wantScalar)
			continue
		}
		first, _ := union[0].(string)
		second, _ := union[1].(string)
		if (first != wantScalar || second != "null") && (first != "null" || second != wantScalar) {
			t.Errorf("%s type = %#v, want [%q,null]", name, union, wantScalar)
		}
	}
}

func TestIssue567VertexAnthropicNormalizesGrepOutputMode(t *testing.T) {
	// The same adapter serves the direct API and Vertex (vertex flag); the tool
	// loop is unconditional, so BOTH paths must normalize. Regression guard: a
	// future refactor that branches the tool loop by a.vertex would silently
	// re-break Vertex.
	req := CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "search"}},
		Tools:    []ToolDef{grepToolDef()},
	}
	raw, err := buildBodyBytes(anthropicAdapter{vertex: true}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	var got struct {
		Tools []struct {
			Strict      bool                   `json:"strict"`
			InputSchema map[string]interface{} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(got.Tools))
	}
	if !got.Tools[0].Strict {
		t.Errorf("vertex grep strict = false, want true")
	}
	if hasNullableUnionEnum(got.Tools[0].InputSchema) {
		t.Fatalf("Vertex-Anthropic input_schema still contains nullable-union+enum:\n%s", raw)
	}
	requireStringType(t, prop(t, got.Tools[0].InputSchema, "output_mode"), "string")
}

// ---------------------------------------------------------------------------
// Acceptance #1 (default tool set) — a realistic request via buildRequest
// ---------------------------------------------------------------------------

func TestIssue567DefaultToolSetWouldPassAnthropicStrict(t *testing.T) {
	// Build the request the way the real connection does (buildRequest wires
	// max_tokens, sampling caps, etc.) carrying a strict tool set resembling the
	// default registry: grep (nullable+enum — the bug) plus a git-style tool
	// (plain string + enum — already valid). After normalization NO tool in the
	// set may carry the rejected combination, i.e. a turn would reach the model.
	conn := NewModelConnection(
		&config.ProviderConnection{APIType: "anthropic"},
		&config.ModelConfig{Model: "claude-opus-4-8"},
	)
	gitLike := ToolDef{Type: "function", Function: FunctionDef{
		Name: "git", Strict: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"operation": map[string]interface{}{
					"type": "string", // plain string + enum — already 200 on Anthropic
					"enum": []string{"status", "diff", "log"},
				},
				"message": map[string]interface{}{"type": []string{"string", "null"}},
			},
			"required":             []string{"operation", "message"},
			"additionalProperties": false,
		},
	}}
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "do stuff"}}, false, []ToolDef{grepToolDef(), gitLike}, nil)

	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		Tools []struct {
			Name        string                 `json:"name"`
			InputSchema map[string]interface{} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools len = %d, want 2", len(got.Tools))
	}
	for _, tool := range got.Tools {
		if hasNullableUnionEnum(tool.InputSchema) {
			t.Fatalf("tool %q still carries nullable-union+enum — would 400:\n%s", tool.Name, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// anthropicSchema unit tests — the normalization rule, table-driven
// ---------------------------------------------------------------------------

func TestIssue567AnthropicSchemaRule(t *testing.T) {
	// Each case feeds a single property map (the shape inside "properties") to
	// anthropicSchema and checks the resulting "type" and whether the combination
	// was reconciled. enum/required/description must never be touched.
	for _, tc := range []struct {
		name     string
		input    map[string]interface{}
		wantType interface{} // expected "type" after normalization
		stillBad bool        // does the result STILL combine null-union + enum?
	}{
		{
			name: "canonical grep case collapses to string",
			input: map[string]interface{}{
				"type": []interface{}{"string", "null"},
				"enum": []interface{}{"content", "files_with_matches", "count"},
			},
			wantType: "string", stillBad: false,
		},
		{
			name: "integer nullable enum collapses to integer",
			input: map[string]interface{}{
				"type": []interface{}{"integer", "null"},
				"enum": []interface{}{1, 2, 3},
			},
			wantType: "integer", stillBad: false,
		},
		{
			name: "boolean nullable enum collapses to boolean",
			input: map[string]interface{}{
				"type": []interface{}{"boolean", "null"},
				"enum": []interface{}{true, false},
			},
			wantType: "boolean", stillBad: false,
		},
		{
			name: "null-first ordering collapses to string",
			input: map[string]interface{}{
				"type": []interface{}{"null", "string"},
				"enum": []interface{}{"a", "b"},
			},
			wantType: "string", stillBad: false,
		},
		{
			name: "nullable WITHOUT enum left as union (200 path)",
			input: map[string]interface{}{
				"type": []interface{}{"string", "null"},
			},
			wantType: []interface{}{"string", "null"}, stillBad: false,
		},
		{
			name: "plain string enum left untouched (already 200)",
			input: map[string]interface{}{
				"type": "string",
				"enum": []interface{}{"x", "y"},
			},
			wantType: "string", stillBad: false,
		},
		{
			name: "union WITHOUT null + enum left untouched (not this bug)",
			input: map[string]interface{}{
				"type": []interface{}{"string", "integer"},
				"enum": []interface{}{"a", 1},
			},
			wantType: []interface{}{"string", "integer"}, stillBad: false,
		},
		{
			name: "multi-non-null nullable enum keeps array (best-effort)",
			// ["string","integer","null"] + enum → drop null → ["string","integer"].
			// Still a union (no such tool exists today; documents current behavior).
			input: map[string]interface{}{
				"type": []interface{}{"string", "integer", "null"},
				"enum": []interface{}{"a", 1},
			},
			wantType: []interface{}{"string", "integer"}, stillBad: false,
		},
		{
			name: "degenerate null-only type left as-is",
			input: map[string]interface{}{
				"type": []interface{}{"null"},
				"enum": []interface{}{"x"},
			},
			wantType: []interface{}{"null"}, stillBad: true, // nothing to collapse to
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := anthropicSchema(tc.input)
			got, ok := out.(map[string]interface{})
			if !ok {
				t.Fatalf("anthropicSchema returned %T, want map", out)
			}
			if !equalAsJSON(got["type"], tc.wantType) {
				t.Errorf("type = %#v, want %#v", got["type"], tc.wantType)
			}
			// enum (and any sibling) is never modified. Compare via JSON so the
			// round-trip's int→float64 number normalization doesn't cause a spurious
			// mismatch (the enum's VALUE is what matters, not its Go type).
			if tc.input["enum"] != nil {
				if !equalAsJSON(got["enum"], tc.input["enum"]) {
					t.Errorf("enum mutated: %#v vs %#v", got["enum"], tc.input["enum"])
				}
			}
			if hasNullableUnionEnum(got) != tc.stillBad {
				t.Errorf("hasNullableUnionEnum = %v, want %v", hasNullableUnionEnum(got), tc.stillBad)
			}
		})
	}
}

func TestIssue567AnthropicSchemaNilAndIdentity(t *testing.T) {
	if got := anthropicSchema(nil); got != nil {
		t.Errorf("anthropicSchema(nil) = %#v, want nil", got)
	}
	// A schema with no enum and no union is structurally unchanged.
	in := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	out := anthropicSchema(in)
	if hasNullableUnionEnum(out) {
		t.Errorf("plain schema flagged: %#v", out)
	}
}

func TestIssue567AnthropicSchemaAcceptsRawMessageAndStruct(t *testing.T) {
	// MCP tools and some callers hand the schema over as json.RawMessage; others
	// as a struct. anthropicSchema must accept any JSON-encodable value (it
	// round-trips through encoding/json), matching geminiSchema.
	raw := json.RawMessage(`{"type":["string","null"],"enum":["a","b"]}`)
	out := anthropicSchema(raw)
	got, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("rawmessage -> %T, want map", out)
	}
	requireStringType(t, got, "string")

	type stub struct {
		Type []string `json:"type"`
		Enum []string `json:"enum"`
	}
	out2 := anthropicSchema(stub{Type: []string{"string", "null"}, Enum: []string{"a"}})
	got2, ok := out2.(map[string]interface{})
	if !ok {
		t.Fatalf("struct -> %T, want map", out2)
	}
	requireStringType(t, got2, "string")
}

func TestIssue567AnthropicSchemaNonEncodableFallback(t *testing.T) {
	// A value encoding/json cannot marshal (e.g. a channel) must not panic and
	// must return WITHOUT walking/mutating the input (mirrors geminiSchema's
	// early return). The fallback is rare in practice but must stay safe.
	in := map[string]interface{}{"type": []interface{}{"string", "null"}, "ch": make(chan int)}
	out := anthropicSchema(in) // must not panic
	if out == nil {
		t.Fatal("anthropicSchema returned nil for non-encodable input")
	}
}

// ---------------------------------------------------------------------------
// Acceptance #3 — generic recursion covers MCP / nested / combinator schemas
// ---------------------------------------------------------------------------

func TestIssue567NormalizesNestedMCPStyleSchema(t *testing.T) {
	schema := syntheticMCPNestedSchema()
	if !hasNullableUnionEnum(schema) {
		t.Fatal("fixture should contain a deep nullable-union+enum before normalization")
	}
	out := anthropicSchema(schema)
	if hasNullableUnionEnum(out) {
		t.Fatalf("nested nullable-union+enum was NOT normalized (recursion incomplete):\n%#v", out)
	}
	// The deep "color" property's anyOf branch collapsed to "string", enum kept.
	o, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("out = %T", out)
	}
	matrix := prop(t, o, "matrix")
	items, ok := matrix["items"].([]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v", matrix["items"])
	}
	color := items[0].(map[string]interface{})["properties"].(map[string]interface{})["color"].(map[string]interface{})
	anyOf := color["anyOf"].([]interface{})
	branch := anyOf[0].(map[string]interface{})
	requireStringType(t, branch, "string")
	if got := branch["enum"].([]interface{}); len(got) != 3 {
		t.Errorf("deep enum = %#v, want 3 members", got)
	}
}

func TestIssue567NormalizesUnderDefsAndAllOf(t *testing.T) {
	// $defs and the allOf/oneOf combinators are other places a nullable enum can
	// hide; the generic walk (recurse into every map value / array element) must
	// reach them without a key whitelist.
	schema := map[string]interface{}{
		"type": "object",
		"$defs": map[string]interface{}{
			"mode": map[string]interface{}{
				"type": []interface{}{"string", "null"},
				"enum": []interface{}{"on", "off"},
			},
		},
		"allOf": []interface{}{
			map[string]interface{}{
				"properties": map[string]interface{}{
					"shape": map[string]interface{}{
						"type": []interface{}{"string", "null"},
						"enum": []interface{}{"circle", "square"},
					},
				},
			},
		},
	}
	out := anthropicSchema(schema)
	if hasNullableUnionEnum(out) {
		t.Fatalf("$defs/allOf nullable-union+enum not normalized:\n%#v", out)
	}
}

// ---------------------------------------------------------------------------
// Acceptance #4 — OpenAI / Gemini paths are NOT normalized (provider-scoped)
// ---------------------------------------------------------------------------

func TestIssue567OpenAIDoesNotNormalize(t *testing.T) {
	// OpenAI/Z.AI/OpenRouter use openAIAdapter (encodeJSON pass-through): the
	// grep schema must go out VERBATIM with ["string","null"] + enum, because
	// OpenAI strict accepts that combination. If the Anthropic normalizer leaked
	// here, this assertion catches it.
	req := CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []ToolDef{grepToolDef()},
	}
	raw, err := buildBodyBytes(openAIAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		Tools []struct {
			Function struct {
				Name       string                 `json:"name"`
				Strict     bool                   `json:"strict"`
				Parameters map[string]interface{} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(got.Tools))
	}
	params := got.Tools[0].Function.Parameters
	if !hasNullableUnionEnum(params) {
		t.Fatalf("OpenAI path was normalized — the nullable-union+enum must be "+
			"preserved verbatim on OpenAI strict (this is the provider-scoping guard):\n%s", raw)
	}
	if !got.Tools[0].Function.Strict {
		t.Errorf("OpenAI grep strict = false, want true")
	}
}

// TestIssue567GeminiCollapsesNullableEnumOwnShape guards the provider-scoping
// seam crossed by both #567 and #573: the Gemini path uses its OWN geminiSchema
// normalizer, NOT the Anthropic strict normalizer. As of #573, Gemini collapses a
// nullable-union+enum to its proto form — scalar "type":"STRING" + "nullable":true
// with the enum preserved — instead of leaving the union array, which Vertex 3.x
// rejects ("Proto field is not repeating, cannot start list"). A plain "string"
// with NO "nullable" would mean the Anthropic normalizer (which only drops null)
// leaked onto the Gemini path; a surviving array "type" would mean no collapse ran.
func TestIssue567GeminiCollapsesNullableEnumOwnShape(t *testing.T) {
	req := CompletionRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Tools:    []ToolDef{grepToolDef()},
	}
	raw, err := buildBodyBytes(geminiAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	decls := got["tools"].([]interface{})[0].(map[string]interface{})["functionDeclarations"].([]interface{})
	params := decls[0].(map[string]interface{})["parameters"].(map[string]interface{})
	outMode := params["properties"].(map[string]interface{})["output_mode"].(map[string]interface{})

	// Gemini proto form: scalar STRING type (a repeated/array "type" is what Vertex
	// 3.x rejects), with nullability expressed as "nullable": true.
	requireStringType(t, outMode, "STRING")
	nullable, ok := outMode["nullable"].(bool)
	if !ok || !nullable {
		t.Fatalf("Gemini output_mode nullable = %#v, want true (Gemini proto nullability)", outMode["nullable"])
	}
	// The enum survives, members left lower-cased (they are data values, not type
	// names that geminiSchema upper-cases).
	enum, ok := outMode["enum"].([]interface{})
	if !ok || len(enum) != 3 {
		t.Fatalf("Gemini output_mode enum = %#v, want 3 members preserved", outMode["enum"])
	}
}

// ---------------------------------------------------------------------------
// Acceptance #5 — deep copy: the caller's shared Parameters map is never mutated
// ---------------------------------------------------------------------------

func TestIssue567NoSharedMapMutation(t *testing.T) {
	def := grepToolDef()
	schema := def.Function.Parameters.(map[string]interface{})
	// Grab a reference to the inner output_mode map BEFORE buildBody runs.
	outMode := schema["properties"].(map[string]interface{})["output_mode"].(map[string]interface{})

	_, err := buildBodyBytes(anthropicAdapter{}, CompletionRequest{
		Model: "claude-opus-4-8", Messages: []Message{{Role: RoleUser, Content: "x"}}, Tools: []ToolDef{def},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}

	// The shared source map must still carry the original nullable union: the
	// deep copy (JSON round-trip) guarantees buildBody never rewrites the
	// registry's schema, so the next turn / another provider sees it intact.
	tt, ok := outMode["type"].([]string)
	if !ok || len(tt) != 2 || tt[0] != "string" || tt[1] != "null" {
		t.Fatalf("shared Parameters map was mutated by anthropic buildBody: "+
			"output_mode.type = %#v, want []string{\"string\",\"null\"}", outMode["type"])
	}
	if !hasNullableUnionEnum(schema) {
		t.Fatal("shared Parameters map lost its nullable-union+enum — deep copy failed")
	}
}

func TestIssue567ConcurrentReuseSafe(t *testing.T) {
	// The registry's tool schemas are shared across turns and providers and may
	// be built concurrently. Deep-copying means the shared map is only ever READ
	// (json.Marshal) during normalization, so parallel builds must not race or
	// corrupt it. Run under the race detector (go test -race).
	def := grepToolDef()
	schema := def.Function.Parameters.(map[string]interface{})
	outMode := schema["properties"].(map[string]interface{})["output_mode"].(map[string]interface{})

	const n = 64
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			// Each goroutine reuses the SAME shared tool definition (as the agent
			// loop does across turns/providers).
			_, errs[i] = buildBodyBytes(anthropicAdapter{}, CompletionRequest{
				Model: "claude-opus-4-8", Messages: []Message{{Role: RoleUser, Content: "x"}}, Tools: []ToolDef{def},
			})
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: buildBody: %v", i, err)
		}
	}
	// Shared map still intact after 64 concurrent normalizations.
	if tt, ok := outMode["type"].([]string); !ok || len(tt) != 2 || tt[0] != "string" || tt[1] != "null" {
		t.Fatalf("shared Parameters map corrupted under concurrency: %#v", outMode["type"])
	}
}

// ---------------------------------------------------------------------------
// Edge cases — nil params, no tools, empty enum
// ---------------------------------------------------------------------------

func TestIssue567NilParametersAndNoToolsUnaffected(t *testing.T) {
	// A no-argument tool (nil Parameters) gets the default object schema, which is
	// then deep-copied harmlessly — no enum anywhere, no panic, strict survives.
	noArgs := ToolDef{Type: "function", Function: FunctionDef{Name: "ping", Strict: true}}
	req := CompletionRequest{
		Model: "claude-opus-4-8", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Tools: []ToolDef{noArgs},
	}
	raw, err := buildBodyBytes(anthropicAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		Tools []struct {
			Strict      bool                   `json:"strict"`
			InputSchema map[string]interface{} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(got.Tools) != 1 || !got.Tools[0].Strict {
		t.Fatalf("no-args tool: len=%d strict=%v", len(got.Tools), got)
	}
	if hasNullableUnionEnum(got.Tools[0].InputSchema) {
		t.Errorf("default object schema flagged: %#v", got.Tools[0].InputSchema)
	}
	if got.Tools[0].InputSchema["type"] != "object" {
		t.Errorf("default schema type = %v, want object", got.Tools[0].InputSchema["type"])
	}

	// A request with no tools at all must still build cleanly.
	raw2, err := buildBodyBytes(anthropicAdapter{}, CompletionRequest{
		Model: "claude-opus-4-8", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("buildBody (no tools): %v", err)
	}
	if strings.Contains(string(raw2), `"input_schema"`) {
		t.Errorf("no-tools request leaked an input_schema: %s", raw2)
	}
}

func TestIssue567EmptyEnumIsNotFlagged(t *testing.T) {
	// An empty enum array carries no enum *members*, but the key is present.
	// The fix only drops "null" from the type; it must not choke on enum:[] and
	// the oracle must still reflect reality (a null-bearing union with an empty
	// enum is a degenerate schema, but the type collapse is harmless either way).
	in := map[string]interface{}{
		"type": []interface{}{"string", "null"},
		"enum": []interface{}{},
	}
	out := anthropicSchema(in)
	got := out.(map[string]interface{})
	requireStringType(t, got, "string")
}

// ---------------------------------------------------------------------------
// Oracle self-test — hasNullableUnionEnum flags exactly the bug shapes
// ---------------------------------------------------------------------------

func TestIssue567OraclePredicate(t *testing.T) {
	bad := []interface{}{
		map[string]interface{}{"type": []interface{}{"string", "null"}, "enum": []interface{}{"a"}},
		map[string]interface{}{"type": []string{"string", "null"}, "enum": []string{"a"}}, // Go-native form
		map[string]interface{}{"type": []interface{}{"null", "integer"}, "enum": []interface{}{1}},
		map[string]interface{}{"properties": map[string]interface{}{"x": map[string]interface{}{
			"type": []interface{}{"string", "null"}, "enum": []interface{}{"a"}}}},
	}
	for i, b := range bad {
		if !hasNullableUnionEnum(b) {
			t.Errorf("case %d: predicate missed a nullable-union+enum: %#v", i, b)
		}
	}
	good := []interface{}{
		map[string]interface{}{"type": "string", "enum": []interface{}{"a"}},
		map[string]interface{}{"type": []interface{}{"string", "null"}},                                // nullable, no enum
		map[string]interface{}{"type": []interface{}{"string", "integer"}, "enum": []interface{}{"a"}}, // union, no null
		map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		nil,
	}
	for i, g := range good {
		if hasNullableUnionEnum(g) {
			t.Errorf("case %d: predicate false-positive on a valid schema: %#v", i, g)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// jsonEqual reports whether two schema values are equal as JSON. Comparing the
// marshaled bytes side-steps Go-type differences introduced by a JSON round-trip
// (notably int → float64 for numeric enum members, and []string vs
// []interface{}), so it is the correct way to assert "this enum was preserved".
func equalAsJSON(a, b interface{}) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}
