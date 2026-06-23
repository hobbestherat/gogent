package tool

import (
	"strings"
	"testing"
)

func TestRenderToolDocsUsesEnabledRegistryMetadata(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name:        "zeta",
		Description: "Zeta description.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"optional": map[string]interface{}{"type": "boolean", "description": "Optional flag."},
				"required": map[string]interface{}{"type": "string", "description": "Required value."},
			},
			"required": []string{"required"},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			return nil, nil
		},
	})
	reg.Register(&Tool{
		Name:        "alpha",
		Description: "Alpha description.",
		InputSchema: nil,
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			return nil, nil
		},
	})
	reg.Register(&Tool{
		Name:        "hidden",
		Description: "Hidden description.",
		InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			return nil, nil
		},
	})
	reg.SetEnabled("hidden", false)

	got := reg.RenderToolDocs()
	alpha := strings.Index(got, "### alpha")
	zeta := strings.Index(got, "### zeta")
	if alpha == -1 || zeta == -1 {
		t.Fatalf("rendered docs missing enabled tools:\n%s", got)
	}
	if alpha > zeta {
		t.Fatalf("rendered docs are not sorted by name:\n%s", got)
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("rendered docs included disabled tool:\n%s", got)
	}
	for _, want := range []string{
		"Alpha description.",
		"Zeta description.",
		"- optional (boolean): Optional flag.",
		"- required (required, string): Required value.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered docs missing %q:\n%s", want, got)
		}
	}
}

func TestRenderToolDocsHandlesMalformedSchemas(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name:        "loose",
		Description: "Loose description.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"bare": "not a schema object",
				"typed": map[string]interface{}{
					"type": "string",
				},
			},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			return nil, nil
		},
	})

	got := reg.RenderToolDocs()
	for _, want := range []string{
		"### loose",
		"Loose description.",
		"- bare",
		"- typed (string)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered docs missing %q:\n%s", want, got)
		}
	}
}
