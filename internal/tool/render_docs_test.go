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

func TestRenderToolDocsSurfacesInputExamples(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&Tool{
		Name:        "format_sensitive",
		Description: "Needs a precise shape.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":  map[string]interface{}{"type": "string"},
				"edits": map[string]interface{}{"type": "array"},
			},
			"required": []string{"path", "edits"},
		},
		InputExamples: []map[string]interface{}{
			{
				"path": "file.txt",
				"edits": []map[string]interface{}{
					{"find": "old", "replace": "new"},
				},
			},
			{"path": "other.txt", "edits": []interface{}{}},
		},
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			return nil, nil
		},
	})
	reg.Register(&Tool{
		Name:        "plain",
		Description: "No examples.",
		Execute: func(args map[string]interface{}, ctx ToolContext) (interface{}, error) {
			return nil, nil
		},
	})

	got := reg.RenderToolDocs()
	formatIdx := strings.Index(got, "### format_sensitive")
	plainIdx := strings.Index(got, "### plain")
	if formatIdx == -1 || plainIdx == -1 {
		t.Fatalf("rendered docs missing tools:\n%s", got)
	}
	formatSection := got[formatIdx:plainIdx]
	if !strings.Contains(formatSection, "Examples:\n") {
		t.Fatalf("format_sensitive section missing Examples header:\n%s", formatSection)
	}
	for _, want := range []string{
		`  {"edits":[{"find":"old","replace":"new"}],"path":"file.txt"}`,
		`  {"edits":[],"path":"other.txt"}`,
	} {
		if !strings.Contains(formatSection, want) {
			t.Errorf("format_sensitive examples missing %q:\n%s", want, formatSection)
		}
	}
	plainSection := got[plainIdx:]
	if strings.Contains(plainSection, "Examples:") {
		t.Fatalf("plain tool should not render empty examples:\n%s", plainSection)
	}
}
