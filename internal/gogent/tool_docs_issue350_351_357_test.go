package gogent

import (
	"regexp"
	"strings"
	"testing"
)

var toolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestInlineToolDescriptionsAreActionable(t *testing.T) {
	g := NewGogent(t.TempDir())

	for _, name := range []string{
		"read",
		"write",
		"edit",
		"shell",
		"structured_output",
		"launch_agent",
		"agent_status",
		"agent_send",
		"agent_terminate",
		"wait_agent_event",
	} {
		t.Run(name, func(t *testing.T) {
			tl := g.GetToolRegistry().Get(name)
			if tl == nil {
				t.Fatalf("tool %q is not registered", name)
			}
			if !toolNameRE.MatchString(tl.Name) {
				t.Fatalf("tool name %q does not match provider-safe format", tl.Name)
			}

			desc := strings.TrimSpace(tl.Description)
			if sentenceCount(desc) < 3 {
				t.Fatalf("description is too terse: got %d sentence(s): %q", sentenceCount(desc), desc)
			}

			lower := strings.ToLower(desc)
			if !containsAny(lower, "use ", "call ", "query ", "answer ", "terminate ", "launch ", "return ") {
				t.Errorf("description should explain when to use the tool: %q", desc)
			}
			if !containsAny(lower, "prefer ", "do not", "don't", "not ", "never ", "instead", "only ") {
				t.Errorf("description should differentiate from alternatives or state limitations: %q", desc)
			}
		})
	}
}

func TestInlineToolSchemaPropertiesHaveDescriptions(t *testing.T) {
	g := NewGogent(t.TempDir())

	for _, name := range []string{
		"read",
		"write",
		"edit",
		"shell",
		"structured_output",
		"launch_agent",
		"agent_status",
		"agent_send",
		"agent_terminate",
		"wait_agent_event",
	} {
		t.Run(name, func(t *testing.T) {
			tl := g.GetToolRegistry().Get(name)
			if tl == nil {
				t.Fatalf("tool %q is not registered", name)
			}
			assertSchemaPropertyDescriptions(t, name, tl.InputSchema)
		})
	}
}

func TestGetSystemPromptToolDocsComeFromRegistry(t *testing.T) {
	g := NewGogent(t.TempDir())

	docs := g.GetToolRegistry().RenderToolDocs()
	if docs == "" {
		t.Fatal("registry docs are empty")
	}

	prompt := g.GetSystemPrompt("session", "agent")
	if !strings.Contains(prompt, docs) {
		t.Fatal("system prompt does not include the registry-rendered tool docs")
	}

	for _, tl := range g.GetToolRegistry().ListEnabled() {
		header := "### " + tl.Name
		if !strings.Contains(prompt, header) {
			t.Errorf("system prompt missing enabled tool %q", tl.Name)
		}
	}

	for _, stale := range []string{
		"\t  \t",
		"read - Read",
		"write - Write",
		"edit - Edit",
		"shell - Execute",
	} {
		if strings.Contains(prompt, stale) {
			t.Errorf("system prompt still contains stale hand-maintained tool text %q", stale)
		}
	}
}

func assertSchemaPropertyDescriptions(t *testing.T, owner string, raw interface{}) {
	t.Helper()

	schema, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("%s schema has type %T, want object schema", owner, raw)
	}

	props, ok := schema["properties"].(map[string]interface{})
	if !ok || len(props) == 0 {
		t.Fatalf("%s schema has no properties", owner)
	}

	for propName, rawProp := range props {
		if !toolNameRE.MatchString(propName) {
			t.Errorf("%s property name %q does not match provider-safe format", owner, propName)
		}

		prop, ok := rawProp.(map[string]interface{})
		if !ok {
			t.Errorf("%s.%s schema has type %T, want object", owner, propName, rawProp)
			continue
		}
		desc, _ := prop["description"].(string)
		if strings.TrimSpace(desc) == "" {
			t.Errorf("%s.%s is missing a non-empty description", owner, propName)
		}
	}
}

func sentenceCount(s string) int {
	count := 0
	for _, r := range s {
		switch r {
		case '.', '!', '?':
			count++
		}
	}
	return count
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
