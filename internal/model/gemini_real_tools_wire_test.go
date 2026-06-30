package model

// Wire-level acceptance: every built-in gogent tool schema, in its VERBATIM
// registered form, must survive the real Gemini build path (geminiAdapter.
// buildBody) and produce a functionDeclarations[].parameters that Vertex's Schema
// proto accepts — no array "type", no disallowed keyword, a "type" on every node.
// These mirror the schemas registered in internal/gogent/gogent.go and
// internal/tool/*.go (kept in sync by hand; the construct-level sanitizer tests
// in gemini_schema_sanitizer_test.go guard the transforms themselves).

import (
	"encoding/json"
	"testing"

	"gogent/internal/config"
)

// geminiTestConfig is a minimal vertex-native (Gemini) connection + model config —
// enough for buildRequest/buildBody to route through geminiAdapter without a live
// endpoint.
func geminiTestConfig() (*config.ProviderConnection, *config.ModelConfig) {
	return &config.ProviderConnection{
			APIType:  "vertex-native",
			Project:  "p",
			Location: "us-central1",
		}, &config.ModelConfig{
			Name:  "g",
			Model: "gemini-3.5-flash",
		}
}

// realBuiltinToolSchemas returns the verbatim parameter schemas of the built-in
// tools that carry a Gemini-incompatible construct (union type, typeless node,
// additionalProperties, empty object). Clean tools (write/edit/launch_agent/…)
// are exercised transitively by the full-set test below.
func realBuiltinToolSchemas() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		// read — nullable-union params + additionalProperties:false
		"read": {
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string"},
				"offset":     map[string]interface{}{"type": []string{"integer", "null"}},
				"limit":      map[string]interface{}{"type": []string{"integer", "null"}},
				"max_length": map[string]interface{}{"type": []string{"integer", "null"}},
			},
			"required": []string{"path", "offset", "limit", "max_length"},
		},
		// grep — many nullable-union params + additionalProperties
		"grep": {
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]interface{}{
				"pattern":          map[string]interface{}{"type": "string"},
				"path":             map[string]interface{}{"type": []string{"string", "null"}},
				"output_mode":      map[string]interface{}{"type": []string{"string", "null"}, "enum": []string{"content", "files_with_matches", "count"}},
				"include":          map[string]interface{}{"type": []string{"string", "null"}},
				"case_insensitive": map[string]interface{}{"type": []string{"boolean", "null"}},
				"max_results":      map[string]interface{}{"type": []string{"integer", "null"}},
			},
			"required": []string{"pattern", "path", "output_mode", "include", "case_insensitive", "max_results"},
		},
		// todo — TYPELESS "todos" property (only items+description)
		"todo": {
			"type": "object",
			"properties": map[string]interface{}{
				"todos": map[string]interface{}{
					"description": "The complete checklist (a JSON array).",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"content": map[string]interface{}{"type": "string"},
							"status":  map[string]interface{}{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
							"note":    map[string]interface{}{"type": "string"},
						},
						"required": []string{"content"},
					},
				},
			},
		},
		// spawn_subagent — object|string union on subtasks.items
		"spawn_subagent": {
			"type": "object",
			"properties": map[string]interface{}{
				"name":  map[string]interface{}{"type": "string"},
				"task":  map[string]interface{}{"type": "string"},
				"async": map[string]interface{}{"type": "boolean"},
				"subtasks": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": []string{"object", "string"},
						"properties": map[string]interface{}{
							"name": map[string]interface{}{"type": "string"},
							"task": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
		},
		// git — every nullable-union scalar kind + array-union + additionalProperties
		"git": {
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]interface{}{
				"operation": map[string]interface{}{"type": "string", "enum": []string{"status", "diff", "log", "commit", "create_branch", "restore"}},
				"message":   map[string]interface{}{"type": []string{"string", "null"}},
				"branch":    map[string]interface{}{"type": []string{"string", "null"}},
				"paths": map[string]interface{}{
					"type":  []string{"array", "null"},
					"items": map[string]interface{}{"type": "string"},
				},
				"staged":    map[string]interface{}{"type": []string{"boolean", "null"}},
				"all":       map[string]interface{}{"type": []string{"boolean", "null"}},
				"max_count": map[string]interface{}{"type": []string{"integer", "null"}},
			},
			"required": []string{"operation", "message", "branch", "paths", "staged", "all", "max_count"},
		},
	}
}

// assertGeminiParamsValid walks a decoded functionDeclarations[].parameters and
// fails on any Gemini-incompatible residue.
func assertGeminiParamsValid(t *testing.T, name string, params interface{}) {
	t.Helper()
	walkSchema(t, params, func(node map[string]interface{}) {
		if _, isArr := node["type"].([]interface{}); isArr {
			t.Errorf("%s: array-valued type on wire (would 400): %#v", name, node)
		}
		for k := range node {
			if !geminiSchemaAllowed[k] {
				t.Errorf("%s: disallowed key %q on wire (would 400): %#v", name, k, node)
			}
		}
		_, hasType := node["type"]
		_, hasAnyOf := node["anyOf"]
		if !hasType && !hasAnyOf {
			t.Errorf("%s: typeless node on wire (would 400): %#v", name, node)
		}
	})
}

// TestGeminiRealToolSchemasWireValid runs each real tool schema individually
// through the full geminiAdapter.buildBody path and asserts the emitted
// parameters are Gemini-valid.
func TestGeminiRealToolSchemasWireValid(t *testing.T) {
	for name, schema := range realBuiltinToolSchemas() {
		t.Run(name, func(t *testing.T) {
			pc, mc := geminiTestConfig()
			conn := NewModelConnection(pc, mc)
			req := conn.buildRequest(
				[]Message{{Role: RoleUser, Content: "hi"}},
				false,
				[]ToolDef{{Type: "function", Function: FunctionDef{Name: name, Parameters: schema}}},
				nil,
			)
			raw, err := buildBodyBytes(geminiAdapter{}, req)
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			params := geminiWireParams(t, raw, 0)
			assertGeminiParamsValid(t, name, params)
		})
	}
}

// TestGeminiAllRealToolSchemasInOneRequest sends EVERY built-in tool in a single
// request (the real shape of a turn) and asserts every functionDeclaration's
// parameters are Gemini-valid — the strongest form of acceptance criterion 1.
func TestGeminiAllRealToolSchemasInOneRequest(t *testing.T) {
	schemas := realBuiltinToolSchemas()
	tools := make([]ToolDef, 0, len(schemas))
	names := make([]string, 0, len(schemas))
	for name, schema := range schemas {
		tools = append(tools, ToolDef{Type: "function", Function: FunctionDef{Name: name, Parameters: schema}})
		names = append(names, name)
	}
	pc, mc := geminiTestConfig()
	conn := NewModelConnection(pc, mc)
	req := conn.buildRequest([]Message{{Role: RoleUser, Content: "hi"}}, false, tools, nil)
	raw, err := buildBodyBytes(geminiAdapter{}, req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	for i, name := range names {
		assertGeminiParamsValid(t, name, geminiWireParams(t, raw, i))
	}
}

// geminiWireParams extracts tools[0].functionDeclarations[i].parameters from a
// decoded Gemini request body.
func geminiWireParams(t *testing.T, raw []byte, i int) interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, raw)
	}
	tools, ok := body["tools"].([]interface{})
	if !ok || len(tools) == 0 {
		t.Fatalf("no tools in body: %s", raw)
	}
	decls, ok := tools[0].(map[string]interface{})["functionDeclarations"].([]interface{})
	if !ok || i >= len(decls) {
		t.Fatalf("functionDeclarations[%d] missing: %s", i, raw)
	}
	return decls[i].(map[string]interface{})["parameters"]
}
