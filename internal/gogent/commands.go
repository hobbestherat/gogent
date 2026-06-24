package gogent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gogent/internal/command"
	"gogent/internal/config"
)

// Custom slash commands service (issue #403): CRUD plus append-only versioning
// over ~/.gogent/commands.json. Every mutation is a load-modify-save under
// commandMu, mirroring the watcher store's load-on-demand persistence model. The
// top-level CommandDef fields always equal the latest version snapshot.

// ErrCommandNotFound is returned when a lookup names a command that does not
// exist; ErrCommandExists when a create would duplicate an existing name. They
// are sentinels so callers (and a future HTTP surface) can map them to statuses.
var (
	ErrCommandNotFound = errors.New("command not found")
	ErrCommandExists   = errors.New("command already exists")
)

// ListCommands returns all custom commands, sorted by name for a stable display
// order. Each is a copy, so callers cannot mutate the persisted store.
func (g *Gogent) ListCommands() []config.CommandDef {
	store := g.LoadCommands()
	out := make([]config.CommandDef, len(store.Commands))
	for i := range store.Commands {
		out[i] = cloneCommand(store.Commands[i])
	}
	sortCommandsByName(out)
	return out
}

// GetCommand returns the custom command with the given name, or ErrCommandNotFound.
func (g *Gogent) GetCommand(name string) (config.CommandDef, error) {
	store := g.LoadCommands()
	if i := indexOfCommand(store.Commands, name); i >= 0 {
		return cloneCommand(store.Commands[i]), nil
	}
	return config.CommandDef{}, fmt.Errorf("%q: %w", name, ErrCommandNotFound)
}

// CreateCommand validates def, stamps it as version 1 with a single history
// snapshot, and persists it. The name must be syntactically valid, must not
// collide with a built-in (ReservedCommandNames) and must not duplicate an
// existing custom command.
func (g *Gogent) CreateCommand(def config.CommandDef) (config.CommandDef, error) {
	g.commandMu.Lock()
	defer g.commandMu.Unlock()

	store := g.LoadCommands()
	def = normalizeCommand(def)
	if err := validateCommandShape(def); err != nil {
		return config.CommandDef{}, err
	}
	// Built-in collision and custom duplicate are distinct conditions with
	// distinct messages, so check the built-in set (command.ReservedNames, which
	// excludes existing custom names) before the duplicate check rather than the
	// combined ReservedCommandNames set.
	if command.ReservedNames()[def.Name] {
		return config.CommandDef{}, fmt.Errorf("name %q is a built-in command", def.Name)
	}
	if indexOfCommand(store.Commands, def.Name) >= 0 {
		return config.CommandDef{}, fmt.Errorf("%q: %w", def.Name, ErrCommandExists)
	}

	def.Version = 1
	def.Versions = []config.CommandVersion{snapshotOf(def, 1)}
	store.Commands = append(store.Commands, def)
	if err := g.SaveCommands(&store); err != nil {
		return config.CommandDef{}, err
	}
	return cloneCommand(def), nil
}

// UpdateCommand records a new version of an existing command: it validates the
// new content, increments the version and appends a snapshot of the new state.
// Name is the immutable key (rename = delete + create); the command must exist.
func (g *Gogent) UpdateCommand(def config.CommandDef) (config.CommandDef, error) {
	g.commandMu.Lock()
	defer g.commandMu.Unlock()

	store := g.LoadCommands()
	def = normalizeCommand(def)
	if err := validateCommandShape(def); err != nil {
		return config.CommandDef{}, err
	}
	i := indexOfCommand(store.Commands, def.Name)
	if i < 0 {
		return config.CommandDef{}, fmt.Errorf("%q: %w", def.Name, ErrCommandNotFound)
	}

	next := store.Commands[i].Version + 1
	def.Version = next
	def.Versions = append(cloneVersions(store.Commands[i].Versions), snapshotOf(def, next))
	store.Commands[i] = def
	if err := g.SaveCommands(&store); err != nil {
		return config.CommandDef{}, err
	}
	return cloneCommand(def), nil
}

// DeleteCommand removes a command (and its entire history) permanently.
func (g *Gogent) DeleteCommand(name string) error {
	g.commandMu.Lock()
	defer g.commandMu.Unlock()

	store := g.LoadCommands()
	i := indexOfCommand(store.Commands, strings.TrimSpace(name))
	if i < 0 {
		return fmt.Errorf("%q: %w", name, ErrCommandNotFound)
	}
	store.Commands = append(store.Commands[:i], store.Commands[i+1:]...)
	return g.SaveCommands(&store)
}

// GetCommandHistory returns the full version history of a command, oldest first.
func (g *Gogent) GetCommandHistory(name string) ([]config.CommandVersion, error) {
	def, err := g.GetCommand(name)
	if err != nil {
		return nil, err
	}
	return def.Versions, nil
}

// RestoreCommandVersion copies version v's content into the current fields,
// increments the version and appends a fresh snapshot — so the restore is itself
// recorded and the history stays append-only (the timeline never rewinds).
func (g *Gogent) RestoreCommandVersion(name string, v int) (config.CommandDef, error) {
	g.commandMu.Lock()
	defer g.commandMu.Unlock()

	store := g.LoadCommands()
	i := indexOfCommand(store.Commands, strings.TrimSpace(name))
	if i < 0 {
		return config.CommandDef{}, fmt.Errorf("%q: %w", name, ErrCommandNotFound)
	}
	cur := store.Commands[i]
	var src *config.CommandVersion
	for j := range cur.Versions {
		if cur.Versions[j].Version == v {
			src = &cur.Versions[j]
			break
		}
	}
	if src == nil {
		return config.CommandDef{}, fmt.Errorf("command %q has no version %d", name, v)
	}

	next := cur.Version + 1
	restored := cur
	restored.Template = src.Template
	restored.Parameters = cloneParams(src.Parameters)
	restored.Model = src.Model
	restored.Agent = src.Agent
	restored.Subtask = src.Subtask
	restored.Version = next
	restored.Versions = append(cloneVersions(cur.Versions), snapshotOf(restored, next))
	store.Commands[i] = restored
	if err := g.SaveCommands(&store); err != nil {
		return config.CommandDef{}, err
	}
	return cloneCommand(restored), nil
}

// ReservedCommandNames returns the names a new custom command may not take: the
// built-in commands (command.ReservedNames) plus every existing custom command
// name. The editor consults it for inline collision detection; CreateCommand
// enforces the built-in half (the existing-name half is enforced by the duplicate
// check, which also blocks self-collision on the in-flight name).
func (g *Gogent) ReservedCommandNames() map[string]bool {
	reserved := command.ReservedNames()
	for _, c := range g.LoadCommands().Commands {
		reserved[c.Name] = true
	}
	return reserved
}

// --- helpers ----------------------------------------------------------------

// normalizeCommand trims the user-facing string fields so a name typed with
// stray whitespace still matches and persists cleanly.
func normalizeCommand(def config.CommandDef) config.CommandDef {
	def.Name = strings.TrimSpace(def.Name)
	def.Description = strings.TrimSpace(def.Description)
	def.Model = strings.TrimSpace(def.Model)
	def.Agent = strings.TrimSpace(def.Agent)
	for i := range def.Parameters {
		def.Parameters[i].Name = strings.TrimSpace(def.Parameters[i].Name)
	}
	return def
}

// validateCommandShape enforces the content rules independent of collisions: a
// valid name, a non-empty template and unique, well-formed parameter names.
func validateCommandShape(def config.CommandDef) error {
	if !command.ValidCommandName(def.Name) {
		return fmt.Errorf("invalid command name %q: use lowercase letters, digits and hyphens", def.Name)
	}
	if strings.TrimSpace(def.Template) == "" {
		return fmt.Errorf("command %q: template must not be empty", def.Name)
	}
	seen := make(map[string]bool, len(def.Parameters))
	for _, p := range def.Parameters {
		if !command.ValidParamName(p.Name) {
			return fmt.Errorf("invalid parameter name %q: use a letter/underscore then letters, digits or underscores", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate parameter name %q", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// snapshotOf captures def's current content as an immutable version stamped at the
// current time (RFC3339). The parameters are cloned so later edits to the live
// command never mutate a stored snapshot.
func snapshotOf(def config.CommandDef, version int) config.CommandVersion {
	return config.CommandVersion{
		Version:    version,
		Template:   def.Template,
		Parameters: cloneParams(def.Parameters),
		Model:      def.Model,
		Agent:      def.Agent,
		Subtask:    def.Subtask,
		SavedAt:    time.Now().UTC().Format(time.RFC3339),
	}
}

// indexOfCommand returns the index of the command named name, or -1.
func indexOfCommand(cmds []config.CommandDef, name string) int {
	for i := range cmds {
		if cmds[i].Name == name {
			return i
		}
	}
	return -1
}

// sortCommandsByName orders commands alphabetically in place.
func sortCommandsByName(cmds []config.CommandDef) {
	for i := 1; i < len(cmds); i++ {
		for j := i; j > 0 && cmds[j].Name < cmds[j-1].Name; j-- {
			cmds[j], cmds[j-1] = cmds[j-1], cmds[j]
		}
	}
}

// cloneCommand deep-copies a command so the persisted store and any returned copy
// never share mutable slices.
func cloneCommand(def config.CommandDef) config.CommandDef {
	def.Parameters = cloneParams(def.Parameters)
	def.Versions = cloneVersions(def.Versions)
	return def
}

func cloneParams(params []config.CommandParam) []config.CommandParam {
	if params == nil {
		return nil
	}
	out := make([]config.CommandParam, len(params))
	copy(out, params)
	return out
}

func cloneVersions(versions []config.CommandVersion) []config.CommandVersion {
	if versions == nil {
		return nil
	}
	out := make([]config.CommandVersion, len(versions))
	for i := range versions {
		out[i] = versions[i]
		out[i].Parameters = cloneParams(versions[i].Parameters)
	}
	return out
}
