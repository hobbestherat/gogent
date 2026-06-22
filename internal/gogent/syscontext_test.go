package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/skill"
)

func TestDiscoverAgentsDocsFindsWorkspaceFile(t *testing.T) {
	ws := t.TempDir()
	want := "# Project rules\nUse tabs."
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte(want), 0644); err != nil {
		t.Fatal(err)
	}

	docs := discoverAgentsDocs(ws, "")
	found := false
	for _, d := range docs {
		if strings.Contains(d.content, "Use tabs.") {
			found = true
		}
	}
	if !found {
		t.Fatalf("workspace AGENTS.md not discovered: %+v", docs)
	}
}

func TestDiscoverAgentsDocsGlobalFirst(t *testing.T) {
	ws := t.TempDir()
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "AGENTS.md"), []byte("global rules"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("workspace rules"), 0644); err != nil {
		t.Fatal(err)
	}

	docs := discoverAgentsDocs(ws, cfg)
	if len(docs) < 2 {
		t.Fatalf("expected at least 2 docs, got %d", len(docs))
	}
	if !strings.Contains(docs[0].content, "global rules") {
		t.Fatalf("expected global doc first, got %q", docs[0].content)
	}
	if !strings.Contains(docs[len(docs)-1].content, "workspace rules") {
		t.Fatalf("expected workspace doc last, got %q", docs[len(docs)-1].content)
	}
}

func TestRenderAgentsContextEmpty(t *testing.T) {
	if got := renderAgentsContext(nil); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestRenderAgentsContextHeaders(t *testing.T) {
	out := renderAgentsContext([]agentsDoc{{path: "/x/AGENTS.md", content: "do the thing"}})
	if !strings.Contains(out, "Project instructions") || !strings.Contains(out, "do the thing") || !strings.Contains(out, "/x/AGENTS.md") {
		t.Fatalf("missing expected content: %q", out)
	}
}

func TestBuildSystemContextIncludesSkillsAndAgents(t *testing.T) {
	// A skills dir with one SKILL.md.
	skillsDir := t.TempDir()
	calc := filepath.Join(skillsDir, "calc")
	if err := os.MkdirAll(calc, 0755); err != nil {
		t.Fatal(err)
	}
	skillBody := "---\nname: calc\ndescription: Do math\n---\n# Calc\nCompute things."
	if err := os.WriteFile(filepath.Join(calc, "SKILL.md"), []byte(skillBody), 0644); err != nil {
		t.Fatal(err)
	}
	reg := skill.NewSkillRegistry()
	if err := reg.LoadSkills(skillsDir); err != nil {
		t.Fatal(err)
	}

	g := &Gogent{skills: reg, agentsContext: "## Project instructions (AGENTS.md)\nbe nice"}
	ctx := g.buildSystemContext("")

	if !strings.Contains(ctx, "be nice") {
		t.Fatalf("missing AGENTS context: %q", ctx)
	}
	if !strings.Contains(ctx, "Available skills") || !strings.Contains(ctx, "calc: Do math") {
		t.Fatalf("missing skills index: %q", ctx)
	}

	// Deactivating the skill drops it from the index.
	reg.DeactivateSkill("calc")
	if strings.Contains(g.buildSystemContext(""), "calc: Do math") {
		t.Fatalf("deactivated skill still listed")
	}
}
