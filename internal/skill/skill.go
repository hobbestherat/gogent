package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Limits applied when loading skills. The skills directories may be shared or
// writable, so the loader treats them as a trust boundary: it never follows
// links (CWE-59), keeps every file it reads inside the skills root, and bounds
// both tree depth and file size to keep untrusted content out of the model
// context (issue #15).
const (
	maxSkillTreeDepth = 16      // maximum directory nesting under a skills root
	maxSkillFileSize  = 1 << 20 // 1 MiB; a SKILL.md is never this large
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

// LoadSkills loads all skills from a directory tree (single read at startup).
// The directory is a trust boundary: symlinks are not followed and no file
// outside it is ever read (issue #15). A missing directory is a no-op rather
// than an error, since the skills directories are optional.
func (r *SkillRegistry) LoadSkills(dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve skills dir %s: %w", dir, err)
	}
	// Canonicalize the root by resolving the symlinks on its own path; the
	// per-file containment check is compared against this resolved root.
	root, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // optional skills directory not present
		}
		return fmt.Errorf("eval skills dir %s: %w", dir, err)
	}

	return r.loadSkillsRecursive(root, root, 0)
}

// loadSkillsRecursive loads skills from a directory recursively. A skill that
// fails to read or parse no longer vanishes silently: its error is aggregated
// (alongside any sibling failures) and returned, while the rest still load
// (issue #17). Symlinked entries are never traversed (issue #15). A missing
// directory is a no-op rather than an error, since the skills directories are
// optional.
func (r *SkillRegistry) loadSkillsRecursive(dir, root string, depth int) error {
	if depth > maxSkillTreeDepth {
		return fmt.Errorf("max skill tree depth exceeded at %s", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // optional skills directory not present
		}
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	var errs error
	for _, entry := range entries {
		// Never follow symlinks anywhere in the skills tree (issue #15): a
		// symlinked directory could otherwise pull arbitrary, out-of-tree
		// content into the model context.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if entry.IsDir() {
			subDir := filepath.Join(dir, entry.Name())
			skillPath := filepath.Join(subDir, "SKILL.md")
			if err := r.loadSkillFile(skillPath, root, entry.Name()); err != nil {
				errs = errors.Join(errs, fmt.Errorf("skill %s: %w", entry.Name(), err))
			}
			// Recurse into subdirectories.
			if err := r.loadSkillsRecursive(subDir, root, depth+1); err != nil {
				errs = errors.Join(errs, err)
			}
		}
	}

	return errs
}

// loadSkillFile loads a single SKILL.md, returning any read or parse error so
// the caller can surface it instead of dropping the skill without a trace. It
// returns nil when the file is absent, since not every directory holds a skill.
// As a trust boundary (issue #15) it refuses symlinks and non-regular files,
// verifies the file lies inside the skills root, and bounds its size — so a
// shared or writable skills dir cannot inject arbitrary content into context.
func (r *SkillRegistry) loadSkillFile(path, root, name string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no SKILL.md in this directory; not an error
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked skill file %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular skill file %s", path)
	}
	if !pathIsInside(path, root) {
		return fmt.Errorf("skill file %s is outside the skills root", path)
	}
	if info.Size() > maxSkillFileSize {
		return fmt.Errorf("skill file %s exceeds %d byte limit", path, maxSkillFileSize)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	description, err := parseFrontmatter(string(content))
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
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
	return nil
}

// pathIsInside reports whether path is strictly nested below root. Both paths
// must already be absolute and symlink-free (the loader canonicalizes the root
// and Lstats every file), so this is a lexical containment check that also
// rejects prefix lookalikes (e.g. "/skills-secret" against root "/skills").
func pathIsInside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != "." && !strings.HasPrefix(rel, "..")
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
