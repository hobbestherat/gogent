package tool

import "testing"

func TestToolStrictDefaultsFalse(t *testing.T) {
	tl := &Tool{Name: "custom"}
	if tl.Strict {
		t.Fatal("zero-value Tool.Strict = true, want false")
	}

	reg := NewToolRegistry()
	reg.Register(tl)
	got := reg.Get("custom")
	if got == nil {
		t.Fatal("custom tool was not registered")
	}
	if got.Strict {
		t.Fatal("registered tool Strict = true, want default false")
	}
}

func TestIssue359ToolPackageStrictConstructors(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterCalcTool()
	reg.RegisterGitTool()

	for _, name := range []string{"calc", "git"} {
		t.Run(name, func(t *testing.T) {
			tl := reg.Get(name)
			if tl == nil {
				t.Fatalf("%s tool is not registered", name)
			}
			if !tl.Strict {
				t.Fatalf("%s Strict = false, want true", name)
			}
			schema, ok := tl.InputSchema.(map[string]interface{})
			if !ok {
				t.Fatalf("%s InputSchema = %T, want map[string]interface{}", name, tl.InputSchema)
			}
			if got := schema["additionalProperties"]; got != false {
				t.Fatalf("%s root additionalProperties = %v, want false", name, got)
			}
			if err := assertNoUnsupportedStrictSchemaKeywords(schema, "schema"); err != "" {
				t.Fatal(err)
			}
		})
	}
}

func TestStrictFlagSurvivesSchemaNormalization(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name:   "strict_custom",
		Strict: true,
		InputSchema: map[string]interface{}{
			"type":    "object",
			"default": map[string]interface{}{},
			"properties": map[string]interface{}{
				"value": map[string]interface{}{
					"type":    "string",
					"default": "x",
				},
			},
			"additionalProperties": false,
		},
	})

	got := reg.Get("strict_custom")
	if got == nil {
		t.Fatal("strict_custom tool was not registered")
	}
	if !got.Strict {
		t.Fatal("Strict = false after registration, want true")
	}
	schema := got.InputSchema.(map[string]interface{})
	if err := assertNoUnsupportedStrictSchemaKeywords(schema, "schema"); err != "" {
		t.Fatal(err)
	}
}

func assertNoUnsupportedStrictSchemaKeywords(schema map[string]interface{}, path string) string {
	for _, key := range []string{"$schema", "$id", "$ref", "$defs", "definitions", "default", "allOf", "pattern"} {
		if _, ok := schema[key]; ok {
			return path + " contains unsupported keyword " + key
		}
	}
	for key, value := range schema {
		if key == "properties" {
			props, ok := value.(map[string]interface{})
			if !ok {
				continue
			}
			for propName, propSchema := range props {
				if nested, ok := propSchema.(map[string]interface{}); ok {
					if err := assertNoUnsupportedStrictSchemaKeywords(nested, path+".properties."+propName); err != "" {
						return err
					}
				}
			}
			continue
		}
		if nested, ok := value.(map[string]interface{}); ok {
			if err := assertNoUnsupportedStrictSchemaKeywords(nested, path+"."+key); err != "" {
				return err
			}
		}
	}
	return ""
}
