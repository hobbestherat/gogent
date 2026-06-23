package tool

import (
	"strings"
	"testing"
)

// TestValidateArgs covers the schema validator directly: nil args, missing
// required keys, type mismatches, and the happy paths. Schemas mirror the shapes
// the tools declare in production (object with properties + required).
func TestValidateArgs(t *testing.T) {
	calcSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"expression": map[string]interface{}{"type": "string"},
		},
		"required": []string{"expression"},
	}
	writeSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":    map[string]interface{}{"type": "string"},
			"content": map[string]interface{}{"type": "string"},
		},
		"required": []string{"path", "content"},
	}
	mixedSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":       map[string]interface{}{"type": "string"},
			"timeout_ms": map[string]interface{}{"type": "number"},
			"final":      map[string]interface{}{"type": "boolean"},
			"items":      map[string]interface{}{"type": "array"},
			"meta":       map[string]interface{}{"type": "object", "properties": map[string]interface{}{"n": map[string]interface{}{"type": "number"}}},
		},
		"required": []string{"path"},
	}

	tests := []struct {
		name    string
		args    map[string]interface{}
		schema  interface{}
		wantErr string // non-empty substring expected; empty means no error
	}{
		// Top-level guards.
		{
			name:    "nil args rejected",
			args:    nil,
			schema:  calcSchema,
			wantErr: "args cannot be nil",
		},
		{
			name:   "non-map schema is a no-op",
			args:   map[string]interface{}{},
			schema: "not a schema",
		},
		{
			name:   "nil schema is a no-op",
			args:   map[string]interface{}{"anything": 1},
			schema: nil,
		},

		// Required keys.
		{
			name:    "missing required key",
			args:    map[string]interface{}{},
			schema:  calcSchema,
			wantErr: `missing required property "expression"`,
		},
		{
			name:    "one of several required keys missing",
			args:    map[string]interface{}{"path": "a.txt"},
			schema:  writeSchema,
			wantErr: `missing required property "content"`,
		},
		{
			name:   "all required keys present",
			args:   map[string]interface{}{"path": "a.txt", "content": "hi"},
			schema: writeSchema,
		},

		// Type checking.
		{
			name:    "wrong type for required string",
			args:    map[string]interface{}{"expression": 5.0},
			schema:  calcSchema,
			wantErr: "expected string, got number",
		},
		{
			name:    "null value fails string type",
			args:    map[string]interface{}{"expression": nil},
			schema:  calcSchema,
			wantErr: "expected string, got null",
		},
		{
			name:   "number accepts float64",
			args:   map[string]interface{}{"path": "a", "timeout_ms": 5000.0},
			schema: mixedSchema,
		},
		{
			name:    "boolean rejects string",
			args:    map[string]interface{}{"path": "a", "final": "true"},
			schema:  mixedSchema,
			wantErr: "expected boolean, got string",
		},
		{
			name:   "array type accepted",
			args:   map[string]interface{}{"path": "a", "items": []interface{}{"x", "y"}},
			schema: mixedSchema,
		},
		{
			name:    "array type rejects object",
			args:    map[string]interface{}{"path": "a", "items": map[string]interface{}{"x": 1}},
			schema:  mixedSchema,
			wantErr: "expected array, got object",
		},
		{
			name:   "object type accepted",
			args:   map[string]interface{}{"path": "a", "meta": map[string]interface{}{"n": 7.0}},
			schema: mixedSchema,
		},

		// Recursion into nested object properties.
		{
			name:    "nested property type enforced",
			args:    map[string]interface{}{"path": "a", "meta": map[string]interface{}{"n": "oops"}},
			schema:  mixedSchema,
			wantErr: "args.meta.n: expected number, got string",
		},

		// Leniency.
		{
			name:   "unknown properties are allowed",
			args:   map[string]interface{}{"path": "a", "surprise": 123},
			schema: mixedSchema,
		},
		{
			name:   "optional fields may be absent",
			args:   map[string]interface{}{"path": "a"},
			schema: mixedSchema,
		},
		{
			name:   "schema with no required accepts empty object",
			args:   map[string]interface{}{},
			schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"x": map[string]interface{}{"type": "string"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArgs(tc.args, tc.schema)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected no error, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRequiredKeysDecoding ensures required keys work whether declared as a Go
// []string (the in-code style) or decoded from JSON as []interface{}.
func TestRequiredKeysDecoding(t *testing.T) {
	t.Run("go string slice", func(t *testing.T) {
		schema := map[string]interface{}{"required": []string{"a", "b"}}
		got := requiredKeys(schema)
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("unexpected keys: %#v", got)
		}
	})
	t.Run("json interface slice", func(t *testing.T) {
		schema := map[string]interface{}{"required": []interface{}{"x", "y"}}
		got := requiredKeys(schema)
		if len(got) != 2 || got[0] != "x" || got[1] != "y" {
			t.Fatalf("unexpected keys: %#v", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if got := requiredKeys(map[string]interface{}{}); got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
	})
}

func TestValidateArgsEnumConstraints(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"mode": map[string]interface{}{
				"type": "string",
				"enum": []string{"content", "files_with_matches", "count"},
			},
			"optional": map[string]interface{}{
				"type": "string",
				"enum": []interface{}{"pending", "in_progress", "completed"},
			},
			"label": map[string]interface{}{"type": "string"},
		},
		"required": []string{"mode"},
	}

	tests := []struct {
		name    string
		args    map[string]interface{}
		wantErr string
	}{
		{
			name: "valid enum value passes",
			args: map[string]interface{}{"mode": "content"},
		},
		{
			name:    "invalid enum value is rejected with field and allowed values",
			args:    map[string]interface{}{"mode": "json"},
			wantErr: `args.mode: value must be one of [content files_with_matches count], got "json"`,
		},
		{
			name: "json decoded interface enum value passes",
			args: map[string]interface{}{"mode": "count", "optional": "in_progress"},
		},
		{
			name:    "json decoded interface enum value rejects",
			args:    map[string]interface{}{"mode": "count", "optional": "blocked"},
			wantErr: `args.optional: value must be one of [pending in_progress completed], got "blocked"`,
		},
		{
			name: "optional enum field may be absent",
			args: map[string]interface{}{"mode": "files_with_matches"},
		},
		{
			name: "field without enum is unaffected",
			args: map[string]interface{}{"mode": "content", "label": "anything-goes"},
		},
		{
			name:    "type error still wins before enum error",
			args:    map[string]interface{}{"mode": 7.0},
			wantErr: "args.mode: expected string, got number",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArgs(tc.args, schema)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected no error, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateArgsEnumConstraintsNestedArrayItems(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"todos": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{"type": "string"},
						"status": map[string]interface{}{
							"type": "string",
							"enum": []string{"pending", "in_progress", "completed"},
						},
					},
					"required": []string{"content"},
				},
			},
		},
		"required": []string{"todos"},
	}

	if err := validateArgs(map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "write tests", "status": "completed"},
			map[string]interface{}{"content": "run tests"},
		},
	}, schema); err != nil {
		t.Fatalf("valid nested enum args rejected: %v", err)
	}

	err := validateArgs(map[string]interface{}{
		"todos": []interface{}{
			map[string]interface{}{"content": "write tests", "status": "blocked"},
		},
	}, schema)
	if err == nil {
		t.Fatal("expected nested array item enum value to be rejected, got nil")
	}
	for _, want := range []string{"args.todos", "status", "[pending in_progress completed]", `"blocked"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("nested enum error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestValidateTypeTable exercises isType/validateType across every supported
// JSON Schema type name.
func TestValidateTypeTable(t *testing.T) {
	cases := []struct {
		value interface{}
		typ   string
		ok    bool
	}{
		{"hi", "string", true},
		{42, "string", false},
		{3.14, "number", true},
		{7, "number", true}, // defensive integer acceptance
		{true, "number", false},
		{9.0, "integer", true},
		{9.5, "integer", false},
		{3, "integer", true},
		{true, "boolean", true},
		{"true", "boolean", false},
		{[]interface{}{}, "array", true},
		{map[string]interface{}{}, "array", false},
		{map[string]interface{}{}, "object", true},
		{nil, "object", false},
		{nil, "null", true},  // unknown-but-matching-ish name is not enforced
		{"x", "weird", true}, // unknown schema type is not enforced
	}
	for _, c := range cases {
		err := validateType(c.value, c.typ, "p")
		if c.ok && err != nil {
			t.Errorf("validateType(%T, %q): expected ok, got %v", c.value, c.typ, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validateType(%T, %q): expected error, got nil", c.value, c.typ)
		}
	}
}

// TestExecuteToolCallValidatesArgs verifies that malformed args are rejected at
// the registry boundary (returning a failed ToolCallResponse, not invoking the
// tool's Execute) and that well-formed args still reach the tool.
func TestExecuteToolCallValidatesArgs(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()

	t.Run("missing required arg does not execute tool", func(t *testing.T) {
		resp, err := reg.ExecuteToolCall(&ToolCall{
			Tool: "calc",
			Args: map[string]interface{}{}, // missing "expression"
		}, ToolContext{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil || resp.Success {
			t.Fatalf("expected failed response, got %+v", resp)
		}
		if !strings.HasPrefix(resp.Error, "invalid args") {
			t.Fatalf("expected 'invalid args' prefix, got %q", resp.Error)
		}
		if !strings.Contains(resp.Error, "expression") {
			t.Fatalf("expected error to name missing key, got %q", resp.Error)
		}
	})

	t.Run("wrong-typed arg does not execute tool", func(t *testing.T) {
		resp, err := reg.ExecuteToolCall(&ToolCall{
			Tool: "calc",
			Args: map[string]interface{}{"expression": 123},
		}, ToolContext{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil || resp.Success {
			t.Fatalf("expected failed response, got %+v", resp)
		}
		if !strings.Contains(resp.Error, "expected string") {
			t.Fatalf("expected type error, got %q", resp.Error)
		}
	})

	t.Run("valid args execute the tool", func(t *testing.T) {
		resp, err := reg.ExecuteToolCall(&ToolCall{
			Tool: "calc",
			Args: map[string]interface{}{"expression": "2+2"},
		}, ToolContext{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil || !resp.Success {
			t.Fatalf("expected successful response, got %+v", resp)
		}
	})
}
