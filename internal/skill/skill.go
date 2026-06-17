package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Skill represents an agent skill
type Skill struct {
	Name        string
	Description string
	Content     string
	Path        string
	LastLoaded  time.Time
}

// SkillUsage tracks usage of a skill
type SkillUsage struct {
	SkillName  string
	Success    int
	Failure    int
	TotalCalls int
}

// SkillRegistry manages available skills with single-read startup
type SkillRegistry struct {
	skills       map[string]*Skill
	activeSkills map[string]bool
	usage        map[string]*SkillUsage
	mu           sync.RWMutex
}

// NewSkillRegistry creates a new skill registry
func NewSkillRegistry() *SkillRegistry {
	return &SkillRegistry{
		skills:       make(map[string]*Skill),
		activeSkills: make(map[string]bool),
		usage:        make(map[string]*SkillUsage),
	}
}

// LoadSkills loads all skills from a directory tree (single read at startup)
func (r *SkillRegistry) LoadSkills(dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.loadSkillsRecursive(dir)
}

// loadSkillsRecursive loads skills from a directory recursively
func (r *SkillRegistry) loadSkillsRecursive(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			// Check for SKILL.md in directory
			skillPath := filepath.Join(dir, entry.Name(), "SKILL.md")
			if info, err := os.Stat(skillPath); err == nil && !info.IsDir() {
				r.loadSkillFile(skillPath, entry.Name())
			}
			// Recurse into subdirectories
			r.loadSkillsRecursive(filepath.Join(dir, entry.Name()))
		}
	}

	return nil
}

// loadSkillFile loads a single skill file
func (r *SkillRegistry) loadSkillFile(path string, name string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}

	description, err := parseFrontmatter(string(content))
	if err != nil {
		return
	}

	r.skills[name] = &Skill{
		Name:        name,
		Description: description,
		Content:     string(content),
		Path:        path,
		LastLoaded:  time.Now(),
	}

	// Initialize usage tracking
	r.usage[name] = &SkillUsage{
		SkillName: name,
	}

	// Activate by default
	r.activeSkills[name] = true
}

// GetSkill gets a skill by name
func (r *SkillRegistry) GetSkill(name string) *Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.skills[name]
}

// ListSkills lists all available skills
func (r *SkillRegistry) ListSkills() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	skills := make([]*Skill, 0, len(r.skills))
	for _, skill := range r.skills {
		skills = append(skills, skill)
	}
	return skills
}

// ListActiveSkills lists active skills
func (r *SkillRegistry) ListActiveSkills() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	skills := make([]*Skill, 0, len(r.activeSkills))
	for name, active := range r.activeSkills {
		if active {
			if skill, ok := r.skills[name]; ok {
				skills = append(skills, skill)
			}
		}
	}
	return skills
}

// ActivateSkill activates a skill
func (r *SkillRegistry) ActivateSkill(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activeSkills[name] = true
}

// DeactivateSkill deactivates a skill
func (r *SkillRegistry) DeactivateSkill(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activeSkills[name] = false
}

// IsSkillActive checks if a skill is active
func (r *SkillRegistry) IsSkillActive(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activeSkills[name]
}

// RecordSkillUsage records usage of a skill
func (r *SkillRegistry) RecordSkillUsage(name string, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.usage[name]; !ok {
		r.usage[name] = &SkillUsage{SkillName: name}
	}

	r.usage[name].TotalCalls++
	if success {
		r.usage[name].Success++
	} else {
		r.usage[name].Failure++
	}
}

// GetSkillStats gets usage statistics for a skill
func (r *SkillRegistry) GetSkillStats(name string) *SkillUsage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.usage[name]
}

// GetAllSkillStats gets all usage statistics
func (r *SkillRegistry) GetAllSkillStats() []*SkillUsage {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := make([]*SkillUsage, 0, len(r.usage))
	for _, usage := range r.usage {
		stats = append(stats, usage)
	}
	return stats
}
