package agent

import (
	"strings"
	"testing"

	"gogent/internal/model"
	"gogent/internal/tool"
)

func TestBuildMessageWithToolsUsesRegistryRenderedDocs(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("tool-docs", m)
	ag := NewAgent("agent", s)
	us := NewUserSession("session", ag)

	reg := tool.NewToolRegistry()
	reg.Register(&tool.Tool{
		Name:        "alpha",
		Description: "Alpha description from the live registry.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Path supplied by the live schema.",
				},
			},
			"required": []string{"path"},
		},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			return nil, nil
		},
	})
	reg.Register(&tool.Tool{
		Name:        "hidden",
		Description: "Hidden description must not be rendered.",
		InputSchema: map[string]interface{}{"type": "object"},
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			return nil, nil
		},
	})
	reg.SetEnabled("hidden", false)

	got := us.buildMessageWithTools(reg, "please inspect the file")
	docs := reg.RenderToolDocs()
	if !strings.Contains(got, docs) {
		t.Fatalf("message does not include registry-rendered docs:\n%s", got)
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("message includes disabled or stale tool docs:\n%s", got)
	}
	for _, want := range []string{
		"You have access to the following tools:",
		"### alpha",
		"- path (required, string): Path supplied by the live schema.",
		"please inspect the file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{
		"\t  \t",
		"read - Read",
		"write - Write",
		"shell - Execute",
	} {
		if strings.Contains(got, stale) {
			t.Errorf("message still contains stale hand-maintained tool text %q", stale)
		}
	}
}

func TestBuildMessageWithToolsNilRegistryOmitsCatalog(t *testing.T) {
	m := model.NewModelConnection()
	s := model.NewModelSession("tool-docs-nil", m)
	ag := NewAgent("agent", s)
	us := NewUserSession("session", ag)

	got := us.buildMessageWithTools(nil, "plain request")
	if strings.Contains(got, "You have access to the following tools:") {
		t.Fatalf("nil registry should not render an empty tool catalog:\n%s", got)
	}
	if !strings.Contains(got, "plain request") {
		t.Fatalf("message body was not preserved:\n%s", got)
	}
	if !strings.Contains(got, "IMPORTANT INSTRUCTIONS:") {
		t.Fatalf("legacy instructions were not preserved:\n%s", got)
	}
}
