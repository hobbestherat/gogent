package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoSkillPath returns the path to a committed SKILL.md given the skill's
// directory name, resolved relative to this test package (internal/gogent).
func repoSkillPath(name string) string {
	return filepath.Join("..", "..", "skills", name, "SKILL.md")
}

// --- B. spawn_subagent tool description --------------------------------------

// TestSpawnSubagentDescription_ParallelGuidance pins acceptance criterion B: the
// registered spawn_subagent tool description explains the subtasks-array
// concurrency, gives when-to-use guidance in the grep/diagnostics style, and
// stays accurate to one-shot mode (SUCCESS/FAILURE).
func TestSpawnSubagentDescription_ParallelGuidance(t *testing.T) {
	g := NewGogent(t.TempDir())
	tl := g.GetToolRegistry().Get("spawn_subagent")
	if tl == nil {
		t.Fatal("spawn_subagent tool is not registered")
	}
	desc := tl.Description
	lower := strings.ToLower(desc)

	// concurrency of a single subtasks-array call.
	if !strings.Contains(desc, "subtasks") {
		t.Error("description never mentions the \"subtasks\" array")
	}
	if !strings.Contains(lower, "concurrent") {
		t.Error("description does not state entries run concurrently")
	}

	// when-to-use / latency framing (grep & diagnostics style "prefer it over...").
	if !strings.Contains(lower, "latency") && !strings.Contains(lower, "prefer it") {
		t.Error("description lacks when-to-use / latency framing in the grep/diagnostics style")
	}

	// one-shot accuracy: SUCCESS/FAILURE contract present.
	if !strings.Contains(desc, "SUCCESS") || !strings.Contains(desc, "FAILURE") {
		t.Error("description omits the one-shot SUCCESS/FAILURE contract")
	}

	// must not regress into a single terse line.
	if len(desc) < 200 {
		t.Errorf("description is only %d chars; expected enriched multi-sentence guidance", len(desc))
	}
}

// --- C. parallel-research skill ----------------------------------------------

// TestParallelResearchSkill_FileContent validates the committed
// skills/parallel-research/SKILL.md against acceptance criterion C: correct
// frontmatter, the three concrete recipes, a worked subtasks example, and
// granularity guidance.
func TestParallelResearchSkill_FileContent(t *testing.T) {
	raw, err := os.ReadFile(repoSkillPath("parallel-research"))
	if err != nil {
		t.Fatalf("read parallel-research SKILL.md: %v", err)
	}
	content := string(raw)
	lower := strings.ToLower(content)

	// Frontmatter: name + description, matching the existing skill format.
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		t.Error("SKILL.md does not start with YAML frontmatter")
	}
	if !strings.Contains(content, "name: parallel-research") {
		t.Error("frontmatter missing `name: parallel-research`")
	}
	if !strings.Contains(content, "description:") {
		t.Error("frontmatter missing a description (the loader rejects this skill otherwise)")
	}

	// The three concrete recipes from the issue.
	for _, want := range []string{"audit", "validate", "investigate"} {
		if !strings.Contains(lower, want) {
			t.Errorf("skill missing the %q recipe", want)
		}
	}

	// A worked spawn_subagent example with a subtasks array.
	if !strings.Contains(content, "spawn_subagent") || !strings.Contains(content, "subtasks") {
		t.Error("skill lacks a worked spawn_subagent subtasks example")
	}

	// Granularity guidance: delegate >=2 tool calls, do trivial single reads inline.
	if !strings.Contains(lower, "granularity") && !strings.Contains(lower, "inline") {
		t.Error("skill lacks granularity guidance (when to delegate vs do inline)")
	}
}

// TestParallelResearchSkill_FollowsFrontmatterConvention cross-checks that the
// new skill's frontmatter shape matches the pre-existing skills it should mirror
// (code-review, debugging, git-commit) — same two keys, in the same block.
func TestParallelResearchSkill_FollowsFrontmatterConvention(t *testing.T) {
	for _, name := range []string{"code-review", "debugging", "git-commit", "parallel-research"} {
		raw, err := os.ReadFile(repoSkillPath(name))
		if err != nil {
			t.Fatalf("read %s SKILL.md: %v", name, err)
		}
		c := string(raw)
		if !strings.Contains(c, "name:") || !strings.Contains(c, "description:") {
			t.Errorf("skill %q: frontmatter missing name/description", name)
		}
	}
}

// TestParallelResearchSkill_LoadsAndInjects is the end-to-end wiring check for
// criterion C: when the skills tree contains parallel-research, the registry
// loads and activates it, and buildSystemContext injects it into the per-turn
// "## Available skills" index (so it survives compaction). It uses the real
// committed SKILL.md copied into an isolated workspace.
func TestParallelResearchSkill_LoadsAndInjects(t *testing.T) {
	raw, err := os.ReadFile(repoSkillPath("parallel-research"))
	if err != nil {
		t.Fatalf("read parallel-research SKILL.md: %v", err)
	}

	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, "skills", "parallel-research")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), raw, 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	g := NewGogentWithWorkspace(t.TempDir(), workspace)

	reg := g.GetSkillRegistry()
	if reg == nil {
		t.Fatal("skill registry is nil")
	}
	if sk := reg.GetSkill("parallel-research"); sk == nil {
		t.Fatal("parallel-research skill not loaded from the workspace skills dir")
	}
	var active bool
	for _, sk := range reg.ListActiveSkills() {
		if sk.Name == "parallel-research" {
			active = true
			break
		}
	}
	if !active {
		t.Fatal("parallel-research is loaded but not active (won't be injected)")
	}

	ctx, _ := g.buildSystemContext("")
	if !strings.Contains(ctx, "## Available skills") {
		t.Fatal("buildSystemContext produced no \"## Available skills\" section")
	}
	if !strings.Contains(ctx, "parallel-research") {
		t.Error("parallel-research not present in the injected skills index (won't survive compaction)")
	}
}
