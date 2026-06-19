package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillFixture creates dir/SKILL.md with the given content, for the
// load-error tests below.
func writeSkillFixture(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func TestSkillRegistryLoadSkills(t *testing.T) {
	registry := NewSkillRegistry()

	err := registry.LoadSkills("./test_skills")
	if err != nil {
		t.Errorf("Failed to load skills: %v", err)
	}

	skills := registry.ListSkills()
	if len(skills) == 0 {
		t.Error("Expected at least one skill")
	}
}

func TestSkillRegistryGetSkill(t *testing.T) {
	registry := NewSkillRegistry()

	skill := registry.GetSkill("nonexistent")
	if skill != nil {
		t.Error("Expected nil for non-existent skill")
	}
}

func TestSkillRegistryListSkills(t *testing.T) {
	registry := NewSkillRegistry()

	registry.skills["test"] = &Skill{
		Name:        "test",
		Description: "Test skill",
	}

	skills := registry.ListSkills()
	if len(skills) != 1 {
		t.Errorf("Expected 1 skill, got %d", len(skills))
	}
}

func TestSkillRegistryActiveSkills(t *testing.T) {
	registry := NewSkillRegistry()

	registry.skills["skill1"] = &Skill{Name: "skill1"}
	registry.skills["skill2"] = &Skill{Name: "skill2"}

	registry.activeSkills["skill1"] = true
	registry.activeSkills["skill2"] = false

	active := registry.ListActiveSkills()
	if len(active) != 1 {
		t.Errorf("Expected 1 active skill, got %d", len(active))
	}

	if !registry.IsSkillActive("skill1") {
		t.Error("Expected skill1 to be active")
	}

	if registry.IsSkillActive("skill2") {
		t.Error("Expected skill2 to be inactive")
	}
}

func TestSkillRegistryUsageStats(t *testing.T) {
	registry := NewSkillRegistry()

	registry.RecordSkillUsage("skill1", true)
	registry.RecordSkillUsage("skill1", true)
	registry.RecordSkillUsage("skill1", false)

	stats := registry.GetSkillStats("skill1")
	if stats.TotalCalls != 3 {
		t.Errorf("Expected 3 calls, got %d", stats.TotalCalls)
	}
	if stats.Success != 2 {
		t.Errorf("Expected 2 successes, got %d", stats.Success)
	}
	if stats.Failure != 1 {
		t.Errorf("Expected 1 failure, got %d", stats.Failure)
	}
}

func TestParseFrontmatter(t *testing.T) {
	content := `---
name: calc
description: Calculate mathematical expressions
---

# Calculation Skill
`

	desc, err := parseFrontmatter(content)
	if err != nil {
		t.Errorf("Failed to parse: %v", err)
	}

	if desc != "Calculate mathematical expressions" {
		t.Errorf("Expected 'Calculate mathematical expressions', got %q", desc)
	}
}

func TestParseFrontmatterWithQuotes(t *testing.T) {
	content := `---
name: calc
description: "Calculate mathematical expressions"
---

# Calculation Skill
`

	desc, err := parseFrontmatter(content)
	if err != nil {
		t.Errorf("Failed to parse: %v", err)
	}

	if desc != "Calculate mathematical expressions" {
		t.Errorf("Expected 'Calculate mathematical expressions', got %q", desc)
	}
}

func TestParseFrontmatterMissingDescription(t *testing.T) {
	content := `---
name: calc
---

# Calculation Skill
`

	_, err := parseFrontmatter(content)
	if err == nil {
		t.Error("Expected error for missing description")
	}

	if !strings.Contains(err.Error(), "description") {
		t.Errorf("Expected description in error, got: %v", err)
	}
}

func TestSkillRegistrySingleRead(t *testing.T) {
	registry := NewSkillRegistry()

	// Load skills
	err := registry.LoadSkills("./test_skills")
	if err != nil {
		t.Errorf("Failed to load skills: %v", err)
	}

	skills1 := registry.ListSkills()

	// Load again - should not duplicate
	err = registry.LoadSkills("./test_skills")
	if err != nil {
		t.Errorf("Failed to load skills second time: %v", err)
	}

	skills2 := registry.ListSkills()

	// Should have same count (single read)
	if len(skills1) != len(skills2) {
		t.Errorf("Skills count changed after second load: %d -> %d", len(skills1), len(skills2))
	}
}

func TestSkillRegistryConcurrency(t *testing.T) {
	registry := NewSkillRegistry()

	// Load skills
	err := registry.LoadSkills("./test_skills")
	if err != nil {
		t.Errorf("Failed to load skills: %v", err)
	}

	done := make(chan bool, 100)

	// Concurrent reads
	for i := 0; i < 50; i++ {
		go func() {
			registry.ListSkills()
			done <- true
		}()
	}

	// Concurrent writes
	for i := 0; i < 50; i++ {
		go func(n int) {
			registry.RecordSkillUsage("calc", n%2 == 0)
			done <- true
		}(i)
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	// Should not panic and stats should be correct
	stats := registry.GetSkillStats("calc")
	if stats == nil {
		t.Error("Expected stats for calc")
	}
}

// TestLoadSkillsSurfacesParseErrors guards issue #17: an unreadable or
// unparseable skill used to vanish silently (loadSkillFile returned nothing).
// Now its error is returned, and — because errors are aggregated, not fatal —
// the well-formed sibling still loads.
func TestLoadSkillsSurfacesParseErrors(t *testing.T) {
	dir := t.TempDir()
	writeSkillFixture(t, filepath.Join(dir, "good"),
		"---\nname: good\ndescription: A good skill\n---\n# Good\n")
	// Missing the required description → parseFrontmatter fails.
	writeSkillFixture(t, filepath.Join(dir, "bad"),
		"---\nname: bad\n---\n# Bad\n")

	reg := NewSkillRegistry()
	err := reg.LoadSkills(dir)
	if err == nil {
		t.Fatal("expected an error for the unparseable skill, got nil")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("expected the error to name the bad skill, got: %v", err)
	}
	if reg.GetSkill("good") == nil {
		t.Error("expected the well-formed skill to still load alongside the bad one")
	}
	if reg.GetSkill("bad") != nil {
		t.Error("the unparseable skill must not be registered")
	}
}

// TestLoadSkillsMissingDirIsNotAnError: the skills directories are optional, so
// their absence is a no-op (nil error), not a noisy failure.
func TestLoadSkillsMissingDirIsNotAnError(t *testing.T) {
	reg := NewSkillRegistry()
	if err := reg.LoadSkills(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("loading a missing optional skills dir should be a no-op, got: %v", err)
	}
}

// TestLoadSkillsLoadsNestedSkill confirms the containment/symlink guards do not
// break legitimate, in-tree skills organized into nested directories.
func TestLoadSkillsLoadsNestedSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkillFixture(t, filepath.Join(dir, "category", "tool"),
		"---\nname: tool\ndescription: nested tool\n---\n# tool\n")

	reg := NewSkillRegistry()
	if err := reg.LoadSkills(dir); err != nil {
		t.Fatalf("expected the nested skill to load cleanly, got: %v", err)
	}
	if reg.GetSkill("tool") == nil {
		t.Error("expected the nested skill to load")
	}
}

// TestLoadSkillsRefusesSymlinkedSkillFile guards issue #15 / CWE-59: a SKILL.md
// that is a symlink (here to a secret file outside the skills root) must be
// refused rather than followed and injected into the model context.
func TestLoadSkillsRefusesSymlinkedSkillFile(t *testing.T) {
	dir := t.TempDir()

	// A "secret" file outside the skills root.
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	skillDir := filepath.Join(dir, "evil")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	// SKILL.md is a symlink to the out-of-tree secret file.
	if err := os.Symlink(secret, filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	reg := NewSkillRegistry()
	err := reg.LoadSkills(dir)
	if err == nil {
		t.Fatal("expected an error refusing the symlinked skill file, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected the error to mention the symlink, got: %v", err)
	}
	if reg.GetSkill("evil") != nil {
		t.Error("a symlinked skill file must not be loaded")
	}
	for _, s := range reg.ListSkills() {
		if strings.Contains(s.Content, "TOPSECRET") {
			t.Errorf("out-of-tree content must not leak into skill %q", s.Name)
		}
	}
}

// TestLoadSkillsDoesNotTraverseSymlinkedDir guards issue #15: a symlinked
// directory inside the skills root is skipped, so skills reachable only through
// it are never loaded.
func TestLoadSkillsDoesNotTraverseSymlinkedDir(t *testing.T) {
	dir := t.TempDir()

	// A real skill tree that lives outside the skills root.
	outside := t.TempDir()
	writeSkillFixture(t, filepath.Join(outside, "leaked"),
		"---\nname: leaked\ndescription: should not load\n---\n# leaked\n")

	// A symlink inside the root pointing at the outside tree.
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	reg := NewSkillRegistry()
	if err := reg.LoadSkills(dir); err != nil {
		t.Errorf("a symlinked entry should be skipped silently, got: %v", err)
	}
	if reg.GetSkill("leaked") != nil {
		t.Error("a skill behind a symlinked directory must not be loaded")
	}
	if got := len(reg.ListSkills()); got != 0 {
		t.Errorf("expected zero skills loaded, got %d", got)
	}
}

// TestLoadSkillsRefusesOversizedSkillFile guards the file-size bound from
// issue #15: an oversized SKILL.md is refused instead of being read in full.
func TestLoadSkillsRefusesOversizedSkillFile(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "big")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", skillDir, err)
	}
	// Valid frontmatter but a body well over the size limit.
	content := "---\nname: big\ndescription: too large\n---\n" +
		strings.Repeat("x", maxSkillFileSize+1)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}

	reg := NewSkillRegistry()
	err := reg.LoadSkills(dir)
	if err == nil {
		t.Fatal("expected an error for the oversized skill file, got nil")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("expected the error to mention the size limit, got: %v", err)
	}
	if reg.GetSkill("big") != nil {
		t.Error("an oversized skill file must not be loaded")
	}
}

// TestLoadSkillsBoundsTreeDepth guards the recursion-depth bound from issue #15:
// a skills tree nested beyond the limit is refused instead of recursing forever.
func TestLoadSkillsBoundsTreeDepth(t *testing.T) {
	dir := t.TempDir()
	// Nest one level beyond the limit and drop a skill at the bottom.
	deep := dir
	for i := 0; i <= maxSkillTreeDepth+1; i++ {
		deep = filepath.Join(deep, "d")
	}
	writeSkillFixture(t, deep,
		"---\nname: deep\ndescription: too deep\n---\n# deep\n")

	reg := NewSkillRegistry()
	err := reg.LoadSkills(dir)
	if err == nil {
		t.Fatal("expected an error for exceeding max skill tree depth, got nil")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("expected the error to mention depth, got: %v", err)
	}
	if reg.GetSkill("deep") != nil {
		t.Error("a skill beyond the depth limit must not be loaded")
	}
}

// TestPathIsInside exercises the containment helper, including the prefix
// lookalike that a naive strings.HasPrefix check would get wrong.
func TestPathIsInside(t *testing.T) {
	const root = "/skills"
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"direct child", "/skills/foo/SKILL.md", true},
		{"nested child", "/skills/a/b/c/SKILL.md", true},
		{"root itself", "/skills", false},
		{"sibling escapes", "/other/foo", false},
		{"parent escape via ..", "/skills/../etc/passwd", false},
		{"prefix lookalike", "/skills-secret/SKILL.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathIsInside(tt.path, root); got != tt.want {
				t.Errorf("pathIsInside(%q, %q) = %v, want %v", tt.path, root, got, tt.want)
			}
		})
	}
}
