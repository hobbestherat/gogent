package tool

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeSchema(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want map[string]interface{}
	}{
		{
			name: "nil becomes empty object schema",
			in:   nil,
			want: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			name: "non-object input becomes empty object schema",
			in:   "not a schema",
			want: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			name: "missing top-level type and properties are added",
			in: map[string]interface{}{
				"required": []string{"path"},
			},
			want: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   []string{"path"},
			},
		},
		{
			name: "non-object top-level type is forced to object",
			in: map[string]interface{}{
				"type":       "string",
				"properties": map[string]interface{}{},
			},
			want: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			name: "unsupported keywords are stripped, supported ones kept",
			in: map[string]interface{}{
				"$schema":     "https://json-schema.org/draft/2020-12/schema",
				"$id":         "urn:x",
				"type":        "object",
				"definitions": map[string]interface{}{"X": map[string]interface{}{}},
				"allOf":       []interface{}{map[string]interface{}{"type": "object"}},
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":    "string",
						"default": "/tmp", // stripped
						"$ref":    "#/definitions/X", // stripped
					},
				},
				"required": []string{"path"},
			},
			want: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		{
			name: "a property literally named default is preserved",
			in: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"default": map[string]interface{}{"type": "string"},
				},
			},
			want: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"default": map[string]interface{}{"type": "string"},
				},
			},
		},
		{
			name: "nested item schemas are normalized recursively",
			in: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tags": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type":    "string",
							"default": "x", // stripped inside items
						},
					},
				},
			},
			want: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tags": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeSchema(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NormalizeSchema()\n got = %#v\nwant = %#v", got, tt.want)
			}
		})
	}
}

// NormalizeSchema must not mutate the caller's schema (it is reused across
// registries and the Resources display).
func TestNormalizeSchemaDoesNotMutateInput(t *testing.T) {
	in := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string", "default": "/tmp"},
		},
	}
	_ = NormalizeSchema(in)
	props := in["properties"].(map[string]interface{})
	path := props["path"].(map[string]interface{})
	if _, ok := path["default"]; !ok {
		t.Fatal("NormalizeSchema mutated the caller's schema (stripped default in place)")
	}
}

// Register normalizes the advertised schema, so a tool registered with a nil or
// keyword-laden schema is exposed as a portable object schema.
func TestRegisterNormalizesSchema(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{Name: "noargs", InputSchema: nil})
	reg.Register(&Tool{
		Name: "dirty",
		InputSchema: map[string]interface{}{
			"type":    "object",
			"$schema": "x",
			"properties": map[string]interface{}{
				"p": map[string]interface{}{"type": "string", "default": "z"},
			},
		},
	})

	noargs := reg.Get("noargs").InputSchema.(map[string]interface{})
	if noargs["type"] != "object" {
		t.Errorf("noargs schema type = %v, want object", noargs["type"])
	}
	if _, ok := noargs["properties"]; !ok {
		t.Error("noargs schema must carry a properties map")
	}

	dirty := reg.Get("dirty").InputSchema.(map[string]interface{})
	if _, ok := dirty["$schema"]; ok {
		t.Error("$schema should be stripped at registration")
	}
	p := dirty["properties"].(map[string]interface{})["p"].(map[string]interface{})
	if _, ok := p["default"]; ok {
		t.Error("nested default should be stripped at registration")
	}
}

// The normalized schema must survive a JSON round-trip unchanged in shape, which
// is what providers actually receive on the wire.
func TestNormalizeSchemaJSONShape(t *testing.T) {
	got := NormalizeSchema(map[string]interface{}{"$ref": "#/x"})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["type"] != "object" {
		t.Errorf("round-tripped type = %v, want object", back["type"])
	}
	if _, ok := back["$ref"]; ok {
		t.Error("$ref leaked through to the wire form")
	}
}
