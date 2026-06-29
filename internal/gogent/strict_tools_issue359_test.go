package gogent

import "testing"

var issue359StrictTools = []string{
	"read",
	"glob",
	"list",
	"calc",
	"git",
	"grep",
}

func TestIssue359StrictToolsRegisteredStrictWithClosedSchemas(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	reg := g.GetToolRegistry()

	for _, name := range issue359StrictTools {
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
			if err := assertStrictSchemaSubset(schema, "schema"); err != "" {
				t.Fatal(err)
			}
		})
	}
}

func TestIssue359SpawnSubagentRemainsNonStrictDespiteRichSchema(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	spawn := g.GetToolRegistry().Get("spawn_subagent")
	if spawn == nil {
		t.Fatal("spawn_subagent tool is not registered")
	}
	if spawn.Strict {
		t.Fatal("spawn_subagent Strict = true, want false because it uses union-typed subtasks and must preserve batched fan-out")
	}

	schema, ok := spawn.InputSchema.(map[string]interface{})
	if !ok {
		t.Fatalf("spawn_subagent InputSchema = %T, want map[string]interface{}", spawn.InputSchema)
	}
	subtasks, ok := propertySchema(schema, "subtasks")
	if !ok {
		t.Fatal("spawn_subagent schema missing subtasks property")
	}
	items, ok := subtasks["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("spawn_subagent subtasks.items = %T, want map[string]interface{}", subtasks["items"])
	}
	if _, ok := items["type"].([]string); !ok {
		t.Fatalf("spawn_subagent subtasks.items.type = %T, want []string union type", items["type"])
	}
}

func propertySchema(schema map[string]interface{}, name string) (map[string]interface{}, bool) {
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	prop, ok := props[name].(map[string]interface{})
	return prop, ok
}

func assertStrictSchemaSubset(schema map[string]interface{}, path string) string {
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
					if err := assertStrictSchemaSubset(nested, path+".properties."+propName); err != "" {
						return err
					}
				}
			}
			continue
		}
		switch v := value.(type) {
		case map[string]interface{}:
			if err := assertStrictSchemaSubset(v, path+"."+key); err != "" {
				return err
			}
		case []interface{}:
			for _, item := range v {
				if nested, ok := item.(map[string]interface{}); ok {
					if err := assertStrictSchemaSubset(nested, path+"."+key+"[]"); err != "" {
						return err
					}
				}
			}
		}
	}
	return ""
}
