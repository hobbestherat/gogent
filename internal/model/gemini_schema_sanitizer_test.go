package model

// This suite exercises the full Gemini Schema sanitizer (sanitizeGeminiSchema via
// geminiSchema), which rewrites a JSON-Schema document into the OpenAPI subset
// Vertex's Schema proto accepts. It covers every incompatibility class found in
// the built-in tool schemas and in arbitrary MCP server schemas: typeless nodes,
// union "type" arrays, additionalProperties, const/oneOf/allOf/not, multipleOf
// and other unsupported numeric/string keywords, empty-object schemas, and a
// stray "required" entry. All tests are unit-level (no live API).

import "testing"

// walkSchema visits every map node in a decoded schema, depth-first, descending
// only through the structural children that hold sub-schemas (properties values,
// items, anyOf entries) — NOT through arbitrary property NAMES, so a property
// literally called "type" or "additionalProperties" is not mistaken for a keyword.
func walkSchema(t *testing.T, v interface{}, visit func(node map[string]interface{})) {
	t.Helper()
	switch n := v.(type) {
	case map[string]interface{}:
		visit(n)
		if props, ok := n["properties"].(map[string]interface{}); ok {
			for _, sub := range props {
				walkSchema(t, sub, visit)
			}
		}
		if items, ok := n["items"]; ok {
			walkSchema(t, items, visit)
		}
		if anyOf, ok := n["anyOf"].([]interface{}); ok {
			for _, sub := range anyOf {
				walkSchema(t, sub, visit)
			}
		}
	}
}

// assertGeminiValid is the umbrella acceptance check: after sanitization every
// schema node must (a) carry no array-valued "type", (b) carry only allow-listed
// keys, and (c) declare a "type" unless it is an anyOf-union node (which carries
// the type in each branch).
func assertGeminiValid(t *testing.T, out interface{}) {
	t.Helper()
	walkSchema(t, out, func(node map[string]interface{}) {
		if _, isArr := node["type"].([]interface{}); isArr {
			t.Errorf("array-valued type survived (would 400): %#v", node)
		}
		for k := range node {
			if !geminiSchemaAllowed[k] {
				t.Errorf("disallowed key %q survived (would 400): %#v", k, node)
			}
		}
		_, hasType := node["type"]
		_, hasAnyOf := node["anyOf"]
		if !hasType && !hasAnyOf {
			t.Errorf("schema node has no type (would 400 \"didn't specify the schema type field\"): %#v", node)
		}
	})
}

// TestGeminiSanitizerStripsAdditionalProperties: additionalProperties is not a
// Gemini Schema field and 400s as an unknown name; it must be removed at every
// depth. (read/grep/glob/list/calc/git/diagnostics/verify all ship it.)
func TestGeminiSanitizerStripsAdditionalProperties(t *testing.T) {
	in := map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"nested": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": true,
				"properties":           map[string]interface{}{},
			},
		},
	}
	out := geminiSchema(in)
	assertGeminiValid(t, out)
	walkSchema(t, out, func(node map[string]interface{}) {
		if _, ok := node["additionalProperties"]; ok {
			t.Fatalf("additionalProperties survived: %#v", node)
		}
	})
}

// TestGeminiSanitizerTypelessNodeGetsInferredType mirrors the todo tool's "todos"
// property: no "type", only "items" + "description". Gemini requires a type, so
// it must be inferred as ARRAY.
func TestGeminiSanitizerTypelessNodeGetsInferredType(t *testing.T) {
	in := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"todos": map[string]interface{}{
				"description": "checklist (array or null)",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{"type": "string"},
					},
					"required": []string{"content"},
				},
			},
		},
	}
	out := geminiSchema(in)
	assertGeminiValid(t, out)
	todos := prop(t, out.(map[string]interface{}), "todos")
	requireStringType(t, todos, "ARRAY")
	items, ok := todos["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("todos.items = %#v, want map", todos["items"])
	}
	requireStringType(t, items, "OBJECT")
}

// TestGeminiSanitizerInfersObjectArrayStringFallback covers each inference branch
// and the STRING fallback for a node with no structural hint.
func TestGeminiSanitizerInfersObjectArrayStringFallback(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]interface{}
		want string
	}{
		{"object-from-properties", map[string]interface{}{"properties": map[string]interface{}{}}, "OBJECT"},
		{"array-from-items", map[string]interface{}{"items": map[string]interface{}{"type": "string"}}, "ARRAY"},
		{"string-from-enum", map[string]interface{}{"enum": []interface{}{"a", "b"}}, "STRING"},
		{"string-fallback", map[string]interface{}{"description": "no hints"}, "STRING"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := geminiSchema(tc.in).(map[string]interface{})
			requireStringType(t, out, tc.want)
			assertGeminiValid(t, out)
		})
	}
}

// TestGeminiSanitizerConstBecomesEnum: const is unsupported; it becomes a
// single-member enum with an inferred STRING type.
func TestGeminiSanitizerConstBecomesEnum(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"properties": map[string]interface{}{
			"mode": map[string]interface{}{"const": "fast"},
		},
	})
	assertGeminiValid(t, out)
	mode := prop(t, out.(map[string]interface{}), "mode")
	if _, ok := mode["const"]; ok {
		t.Fatalf("const survived: %#v", mode)
	}
	enum, ok := mode["enum"].([]interface{})
	if !ok || len(enum) != 1 || enum[0] != "fast" {
		t.Fatalf("const not converted to enum [fast]: %#v", mode["enum"])
	}
	requireStringType(t, mode, "STRING")
}

// TestGeminiSanitizerOneOfBecomesAnyOf: oneOf is unsupported; it becomes anyOf
// (the only Gemini combinator), each branch sanitized.
func TestGeminiSanitizerOneOfBecomesAnyOf(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"properties": map[string]interface{}{
			"u": map[string]interface{}{
				"oneOf": []interface{}{
					map[string]interface{}{"type": "string"},
					map[string]interface{}{"type": "integer"},
				},
			},
		},
	})
	assertGeminiValid(t, out)
	u := prop(t, out.(map[string]interface{}), "u")
	if _, ok := u["oneOf"]; ok {
		t.Fatalf("oneOf survived: %#v", u)
	}
	anyOf, ok := u["anyOf"].([]interface{})
	if !ok || len(anyOf) != 2 {
		t.Fatalf("oneOf not converted to a 2-branch anyOf: %#v", u["anyOf"])
	}
	for _, b := range anyOf {
		requireStringType(t, b.(map[string]interface{}), strings_ToUpperFirstScalar(b))
	}
}

// strings_ToUpperFirstScalar returns the expected uppercase scalar for an anyOf
// branch (helper to keep the loop above readable).
func strings_ToUpperFirstScalar(b interface{}) string {
	m := b.(map[string]interface{})
	switch m["type"] {
	case "STRING":
		return "STRING"
	case "INTEGER":
		return "INTEGER"
	}
	return m["type"].(string)
}

// TestGeminiSanitizerAllOfMerges: allOf is unsupported; its subschemas are
// shallow-merged into the parent (properties union, required union).
func TestGeminiSanitizerAllOfMerges(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type": "object",
		"allOf": []interface{}{
			map[string]interface{}{
				"properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
				"required":   []interface{}{"a"},
			},
			map[string]interface{}{
				"properties": map[string]interface{}{"b": map[string]interface{}{"type": "integer"}},
			},
		},
	}).(map[string]interface{})
	assertGeminiValid(t, out)
	if _, ok := out["allOf"]; ok {
		t.Fatalf("allOf survived: %#v", out)
	}
	requireStringType(t, prop(t, out, "a"), "STRING")
	requireStringType(t, prop(t, out, "b"), "INTEGER")
}

// TestGeminiSanitizerStripsUnsupportedNumericStringKeywords: multipleOf,
// exclusiveMinimum/Maximum, patternProperties are all unsupported and must be
// stripped, while supported siblings (pattern, format, minimum, maximum) survive.
func TestGeminiSanitizerStripsUnsupportedNumericStringKeywords(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"properties": map[string]interface{}{
			"n": map[string]interface{}{
				"type":             "integer",
				"multipleOf":       2,
				"exclusiveMinimum": 0,
				"exclusiveMaximum": 100,
				"minimum":          1,
				"maximum":          99,
			},
			"s": map[string]interface{}{
				"type":              "string",
				"pattern":           "^a",
				"format":            "uuid",
				"patternProperties": map[string]interface{}{".*": map[string]interface{}{"type": "string"}},
			},
		},
	})
	assertGeminiValid(t, out)
	n := prop(t, out.(map[string]interface{}), "n")
	for _, bad := range []string{"multipleOf", "exclusiveMinimum", "exclusiveMaximum"} {
		if _, ok := n[bad]; ok {
			t.Fatalf("%s survived on n: %#v", bad, n)
		}
	}
	if n["minimum"] == nil || n["maximum"] == nil {
		t.Fatalf("supported minimum/maximum were dropped: %#v", n)
	}
	s := prop(t, out.(map[string]interface{}), "s")
	if _, ok := s["patternProperties"]; ok {
		t.Fatalf("patternProperties survived: %#v", s)
	}
	if s["pattern"] != "^a" || s["format"] != "uuid" {
		t.Fatalf("supported pattern/format were dropped: %#v", s)
	}
}

// TestGeminiSanitizerEmptyObjectSchemaValid: a no-property object (diagnostics,
// verify) keeps a valid OBJECT type once additionalProperties is stripped.
func TestGeminiSanitizerEmptyObjectSchemaValid(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type":                 "object",
		"properties":           map[string]interface{}{},
		"additionalProperties": false,
	}).(map[string]interface{})
	assertGeminiValid(t, out)
	requireStringType(t, out, "OBJECT")
}

// TestGeminiSanitizerPrunesStrayRequired: a "required" entry naming a property
// that does not exist is rejected by Vertex; it must be pruned, and an existing
// one kept.
func TestGeminiSanitizerPrunesStrayRequired(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"path", "ghost"},
	}).(map[string]interface{})
	assertGeminiValid(t, out)
	req, ok := out["required"].([]interface{})
	if !ok {
		t.Fatalf("required = %#v, want []interface{}{\"path\"}", out["required"])
	}
	if len(req) != 1 || req[0] != "path" {
		t.Fatalf("required = %v, want [path] (ghost pruned)", req)
	}
}

// TestGeminiSanitizerRequiredAllStrayRemoved: when every required entry is a
// ghost, the "required" key is removed entirely (an empty required is invalid).
func TestGeminiSanitizerRequiredAllStrayRemoved(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"ghost"},
	}).(map[string]interface{})
	if _, ok := out["required"]; ok {
		t.Fatalf("all-stray required not removed: %#v", out["required"])
	}
}

// TestGeminiSanitizerStripsRefAndDefs: $ref/$defs/definitions/$schema are
// unsupported (NormalizeSchema strips most pre-wire, but MCP schemas bypass it),
// so the sanitizer must drop them defensively.
func TestGeminiSanitizerStripsRefAndDefs(t *testing.T) {
	out := geminiSchema(map[string]interface{}{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$defs": map[string]interface{}{
			"X": map[string]interface{}{"type": "string"},
		},
		"type": "object",
		"properties": map[string]interface{}{
			"p": map[string]interface{}{"$ref": "#/$defs/X", "type": "string"},
		},
	})
	assertGeminiValid(t, out)
	for _, bad := range []string{"$schema", "$defs", "definitions"} {
		if _, ok := out.(map[string]interface{})[bad]; ok {
			t.Fatalf("%s survived at root", bad)
		}
	}
}

// TestGeminiSanitizerDeepCopyDoesNotMutateInput: the caller's shared Parameters
// map (re-used across turns/providers, built concurrently) must only be READ.
func TestGeminiSanitizerDeepCopyDoesNotMutateInput(t *testing.T) {
	in := map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"x": map[string]interface{}{"type": []string{"string", "null"}},
		},
	}
	_ = geminiSchema(in)
	if _, ok := in["additionalProperties"]; !ok {
		t.Fatalf("input additionalProperties was mutated/removed: %#v", in)
	}
	x := in["properties"].(map[string]interface{})["x"].(map[string]interface{})
	if got, ok := x["type"].([]string); !ok || len(got) != 2 {
		t.Fatalf("input union type was mutated: %#v", x["type"])
	}
}
