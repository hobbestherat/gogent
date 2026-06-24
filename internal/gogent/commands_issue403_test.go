package gogent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gogent/internal/config"
)

func commandsPathIssue403(home string) string {
	return filepath.Join(home, ".gogent", commandsFileName)
}

func writeRawCommandsIssue403(t *testing.T, home string, data []byte) {
	t.Helper()
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir commands dir: %v", err)
	}
	if err := os.WriteFile(commandsPathIssue403(home), data, 0o600); err != nil {
		t.Fatalf("write commands file: %v", err)
	}
}

func TestIssue403CommandStoreRoundTripsCommandsJSON(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	want := config.CommandStore{Commands: []config.CommandDef{
		{
			Name:        "create-component",
			Description: "Scaffold a component",
			Parameters: []config.CommandParam{
				{Name: "name", Description: "Component name", Required: true},
				{Name: "dir", Description: "Target dir", Default: "src/components"},
			},
			Template: "Create $name in ${dir}",
			Model:    "fast",
			Agent:    "frontend",
			Subtask:  true,
			Version:  2,
			Versions: []config.CommandVersion{
				{Version: 1, Template: "Create $name", Parameters: []config.CommandParam{{Name: "name", Required: true}}, SavedAt: "2026-01-15T10:00:00Z"},
				{Version: 2, Template: "Create $name in ${dir}", Parameters: []config.CommandParam{{Name: "name", Required: true}, {Name: "dir", Default: "src/components"}}, Model: "fast", Agent: "frontend", Subtask: true, SavedAt: "2026-01-15T11:00:00Z"},
			},
		},
	}}

	if err := g.SaveCommands(&want); err != nil {
		t.Fatalf("SaveCommands: %v", err)
	}
	if _, err := os.Stat(commandsPathIssue403(home) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("atomic save left temp file behind: %v", err)
	}
	if info, err := os.Stat(commandsPathIssue403(home)); err != nil {
		t.Fatalf("stat commands.json: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("commands.json mode = %v, want 0600", got)
	}

	got := g.LoadCommands()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestIssue403LoadCommandsMissingAndCorruptFilesAreEmpty(t *testing.T) {
	if got := NewGogent(t.TempDir()).LoadCommands(); len(got.Commands) != 0 {
		t.Fatalf("missing commands file loaded %d commands, want empty", len(got.Commands))
	}

	for _, raw := range []string{"", "{", "not json", `{"commands":"not-array"}`, `[1,2,3]`} {
		home := t.TempDir()
		writeRawCommandsIssue403(t, home, []byte(raw))
		if got := NewGogent(home).LoadCommands(); len(got.Commands) != 0 {
			t.Fatalf("corrupt commands content %q loaded %d commands, want empty", raw, len(got.Commands))
		}
	}
}

func TestIssue403CommandVersioningCreateUpdateRestoreAppendOnly(t *testing.T) {
	g := NewGogent(t.TempDir())

	created, err := g.CreateCommand(config.CommandDef{
		Name:       "review-change",
		Template:   "Review $target",
		Parameters: []config.CommandParam{{Name: "target", Required: true}},
		Model:      "model-a",
		Agent:      "agent-a",
	})
	if err != nil {
		t.Fatalf("CreateCommand: %v", err)
	}
	assertLatestVersionIssue403(t, created, 1)

	created.Template = "Review $target with $depth detail"
	created.Parameters = append(created.Parameters, config.CommandParam{Name: "depth", Default: "medium"})
	created.Model = "model-b"
	created.Agent = "agent-b"
	created.Subtask = true
	updated, err := g.UpdateCommand(created)
	if err != nil {
		t.Fatalf("UpdateCommand: %v", err)
	}
	assertLatestVersionIssue403(t, updated, 2)
	if len(updated.Versions) != 2 {
		t.Fatalf("update versions len = %d, want 2", len(updated.Versions))
	}

	restored, err := g.RestoreCommandVersion("review-change", 1)
	if err != nil {
		t.Fatalf("RestoreCommandVersion: %v", err)
	}
	assertLatestVersionIssue403(t, restored, 3)
	if len(restored.Versions) != 3 {
		t.Fatalf("restore versions len = %d, want 3", len(restored.Versions))
	}
	if restored.Template != "Review $target" || restored.Model != "model-a" || restored.Agent != "agent-a" || restored.Subtask {
		t.Fatalf("restore did not copy version 1 content into top-level fields: %#v", restored)
	}
	if restored.Versions[0].Version != 1 || restored.Versions[1].Version != 2 || restored.Versions[2].Version != 3 {
		t.Fatalf("versions not append-only/in order: %#v", restored.Versions)
	}
	if restored.Versions[2].Template != restored.Template || !reflect.DeepEqual(restored.Versions[2].Parameters, restored.Parameters) {
		t.Fatalf("restore snapshot does not mirror latest top-level fields: latest=%#v top=%#v", restored.Versions[2], restored)
	}
	for _, v := range restored.Versions {
		if _, err := time.Parse(time.RFC3339, v.SavedAt); err != nil {
			t.Fatalf("version %d SavedAt %q is not RFC3339: %v", v.Version, v.SavedAt, err)
		}
	}
}

func assertLatestVersionIssue403(t *testing.T, def config.CommandDef, wantVersion int) {
	t.Helper()
	if def.Version != wantVersion {
		t.Fatalf("Version = %d, want %d", def.Version, wantVersion)
	}
	if len(def.Versions) == 0 {
		t.Fatal("Versions must contain latest snapshot")
	}
	latest := def.Versions[len(def.Versions)-1]
	if latest.Version != def.Version || latest.Template != def.Template ||
		latest.Model != def.Model || latest.Agent != def.Agent || latest.Subtask != def.Subtask ||
		!reflect.DeepEqual(latest.Parameters, def.Parameters) {
		t.Fatalf("latest snapshot %#v does not mirror top-level command %#v", latest, def)
	}
}

func TestIssue403CommandCollisionsAndValidation(t *testing.T) {
	g := NewGogent(t.TempDir())
	if _, err := g.CreateCommand(config.CommandDef{Name: "review", Template: "Review"}); err != nil {
		t.Fatalf("seed CreateCommand: %v", err)
	}
	if _, err := g.CreateCommand(config.CommandDef{Name: "review", Template: "Again"}); !errors.Is(err, ErrCommandExists) {
		t.Fatalf("duplicate custom command error = %v, want ErrCommandExists", err)
	}
	for _, builtin := range []string{"undo", "help", "read"} {
		if _, err := g.CreateCommand(config.CommandDef{Name: builtin, Template: "shadow"}); err == nil || !strings.Contains(err.Error(), "built-in") {
			t.Fatalf("built-in collision %q error = %v, want built-in rejection", builtin, err)
		}
	}
	for _, tc := range []config.CommandDef{
		{Name: "BadName", Template: "x"},
		{Name: "empty-template", Template: " "},
		{Name: "bad-param", Template: "$1bad", Parameters: []config.CommandParam{{Name: "1bad"}}},
		{Name: "dup-param", Template: "$x", Parameters: []config.CommandParam{{Name: "x"}, {Name: "x"}}},
	} {
		if _, err := g.CreateCommand(tc); err == nil {
			t.Fatalf("CreateCommand(%#v) should fail validation", tc)
		}
	}

	reserved := g.ReservedCommandNames()
	for _, name := range []string{"review", "undo", "help", "read"} {
		if !reserved[name] {
			t.Fatalf("ReservedCommandNames missing %q", name)
		}
	}
}

func TestIssue403CommandServiceReturnsDeepCopies(t *testing.T) {
	g := NewGogent(t.TempDir())
	created, err := g.CreateCommand(config.CommandDef{
		Name:       "copy-check",
		Template:   "$thing",
		Parameters: []config.CommandParam{{Name: "thing", Required: true}},
	})
	if err != nil {
		t.Fatalf("CreateCommand: %v", err)
	}

	created.Parameters[0].Name = "mutated"
	created.Versions[0].Parameters[0].Name = "mutated-version"
	got, err := g.GetCommand("copy-check")
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if got.Parameters[0].Name != "thing" || got.Versions[0].Parameters[0].Name != "thing" {
		t.Fatalf("returned command shares mutable slices with store: %#v", got)
	}

	listed := g.ListCommands()
	listed[0].Parameters[0].Name = "listed-mutated"
	again, _ := g.GetCommand("copy-check")
	if again.Parameters[0].Name != "thing" {
		t.Fatalf("ListCommands returned mutable store slice: %#v", again)
	}
}

func TestIssue403SaveCommandsNilWritesValidJSON(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	if err := g.SaveCommands(nil); err != nil {
		t.Fatalf("SaveCommands(nil): %v", err)
	}
	data, err := os.ReadFile(commandsPathIssue403(home))
	if err != nil {
		t.Fatalf("read commands.json: %v", err)
	}
	var store config.CommandStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("nil save produced invalid JSON: %v (%s)", err, data)
	}
	if len(store.Commands) != 0 {
		t.Fatalf("nil save loaded %d commands, want empty", len(store.Commands))
	}
}
