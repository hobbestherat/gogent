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
