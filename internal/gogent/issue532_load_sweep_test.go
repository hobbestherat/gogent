package gogent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogent/internal/config"
	"gogent/internal/model"
)

// Issue #532 — GOAL 2 (load time) + GOAL 4 (resolution). At load, every
// ModelConfigs entry is run through model.ValidateModelConfig; unroutable ones are
// dropped from the in-memory config (policy: WARN-AND-SKIP — never a silent file
// rewrite) and recorded in ConfigWarnings for a startup notice. routableDefaultConfig
// is the shared resolution rule (default if routable, else first routable, else nil)
// used by defaultConnection, the SendMessage fallback, and the listing path; when no
// routable model exists, defaultConnection returns NewUnroutableConnection so a new
// session fails with a clear error instead of silently dialing localhost.

// seedConfigOnDisk writes cfg to <home>/.gogent/config.json (via the real SaveConfig)
// and returns the bytes written, so a test can assert the sweep does not rewrite it.
func seedConfigOnDisk(t *testing.T, home string, cfg *config.Config) []byte {
	t.Helper()
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig seed: %v", err)
	}
	return readConfigBytes(t, home)
}

// goodConnName is the name of the shared routable connection goodEntry references.
const goodConnName = "good-conn"

// goodConns returns the connection list a config needs so goodEntry models resolve
// to a routable provider connection (explicit endpoint).
func goodConns() []*config.ProviderConnection {
	return []*config.ProviderConnection{
		{Name: goodConnName, APIType: "openai", Endpoint: "https://api.example.com/v1"},
	}
}

// badEntry is an unroutable config — the stale pre-validation shape from the issue
// (it references no provider connection, so it cannot be routed).
func badEntry(name string) *config.ModelConfig {
	return &config.ModelConfig{Name: name, Model: "x"}
}

// goodEntry is a routable config (references the goodConns routable connection).
func goodEntry(name string) *config.ModelConfig {
	return &config.ModelConfig{Name: name, Connection: goodConnName, Model: name + "-model"}
}

// modelNames returns the configured model names in config order.
func modelNames(g *Gogent) []string {
	var out []string
	for _, m := range g.Models() {
		out = append(out, m.Name)
	}
	return out
}

// TestLoadSweep_DropsBadKeepsGood_Notifies_DefaultsToGood is the load-time half of
// the fix. A bad on-disk entry is dropped from memory, a notice is recorded naming
// it, and a default that pointed at the bad entry resolves to a survivor. The file
// on disk is left untouched (warn-and-skip, never a silent rewrite).
func TestLoadSweep_DropsBadKeepsGood_Notifies_DefaultsToGood(t *testing.T) {
	home := t.TempDir()
	cfg := &config.Config{
		DefaultModel: "bad",
		Connections:  goodConns(),
		ModelConfigs: []*config.ModelConfig{badEntry("bad"), goodEntry("good")},
	}
	before := seedConfigOnDisk(t, home, cfg)

	g := NewGogent(home)

	// Only the good entry survived in memory.
	names := modelNames(g)
	if len(names) != 1 || names[0] != "good" {
		t.Fatalf("sweep should keep only the good entry; got %v", names)
	}
	// The dropped entry is no longer resolvable by name (so no session can route to it).
	if g.config.GetModelConfig("bad") != nil {
		t.Error("the unroutable entry must be dropped from memory and not resolvable by name")
	}
	// A user-visible notice names the dropped entry and the cause.
	ws := g.ConfigWarnings()
	if len(ws) != 1 {
		t.Fatalf("ConfigWarnings = %v, want exactly 1 notice", ws)
	}
	if !strings.Contains(ws[0], "bad") || !strings.Contains(ws[0], "misconfigured") {
		t.Errorf("notice should name the dropped entry and the cause; got %q", ws[0])
	}
	// The default that pointed at the bad entry now resolves to the survivor (not localhost).
	conn := g.defaultConnection()
	if conn.ModelName != "good-model" {
		t.Errorf("defaultConnection resolved to %q, want the good entry's model (good-model)", conn.ModelName)
	}
	// The file on disk is unchanged (never silently rewritten).
	if after := readConfigBytes(t, home); string(after) != string(before) {
		t.Error("config.json was rewritten by the load sweep; it must not be")
	}
}

// TestLoadSweep_ConfigWarningsReturnsCopy guards the defensive-copy contract: a
// caller mutating the returned slice must not corrupt the stored warnings.
func TestLoadSweep_ConfigWarningsReturnsCopy(t *testing.T) {
	home := t.TempDir()
	seedConfigOnDisk(t, home, &config.Config{
		DefaultModel: "bad",
		Connections:  goodConns(),
		ModelConfigs: []*config.ModelConfig{badEntry("bad"), goodEntry("good")},
	})
	g := NewGogent(home)

	first := g.ConfigWarnings()
	if len(first) == 0 {
		t.Fatal("precondition: expected at least one warning")
	}
	// Mutate the returned slice every way a caller might.
	first[0] = "MUTATED"
	first = append(first, "extra")
	if len(first) != 2 { // use the append result; the local copy should have grown.
		t.Fatalf("local copy should have grown to 2 entries after append, got %d", len(first))
	}

	again := g.ConfigWarnings()
	if len(again) != 1 {
		t.Fatalf("mutating the returned slice changed the stored warnings: got %d, want 1", len(again))
	}
	if again[0] == "MUTATED" {
		t.Error("the returned slice aliased the stored warnings")
	}
}

// TestLoadSweep_AllUnroutable_EmptyInMemory_ClearError is the Defect-1.1 fix: when
// every configured entry is unroutable, the sweep empties the in-memory list and the
// default connection is the fail-safe NewUnroutableConnection — the first send fails
// with a clear, actionable error instead of silently dialing localhost.
func TestLoadSweep_AllUnroutable_EmptyInMemory_ClearError(t *testing.T) {
	home := t.TempDir()
	cfg := &config.Config{
		DefaultModel: "bad1",
		ModelConfigs: []*config.ModelConfig{badEntry("bad1"), badEntry("bad2")},
	}
	seedConfigOnDisk(t, home, cfg)

	g := NewGogent(home)

	if names := modelNames(g); len(names) != 0 {
		t.Fatalf("an all-unroutable config should be emptied by the sweep; got %v", names)
	}
	if ws := g.ConfigWarnings(); len(ws) != 2 {
		t.Fatalf("ConfigWarnings = %v, want 2 notices (one per dropped entry)", ws)
	}
	// The default connection is the fail-safe: it carries a clear deferred error and
	// short-circuits without dialing.
	conn := g.defaultConnection()
	start := time.Now()
	resp, err := conn.Complete([]model.Message{{Role: model.RoleUser, Content: "hi"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Complete on the no-routable-model fail-safe must error, got nil")
	}
	if resp != nil {
		t.Errorf("resp must be nil; got %+v", resp)
	}
	if !strings.Contains(err.Error(), "no routable model is configured") {
		t.Errorf("error = %q, want the actionable fail-safe message", err.Error())
	}
	if elapsed > 2*time.Second {
		t.Errorf("the fail-safe must short-circuit without dialing localhost; took %v", elapsed)
	}
}

// TestLoadSweep_NoBadEntries_NoWarnings: a clean config triggers no drop and no notice.
func TestLoadSweep_NoBadEntries_NoWarnings(t *testing.T) {
	home := t.TempDir()
	seedConfigOnDisk(t, home, &config.Config{
		DefaultModel: "g1",
		Connections:  goodConns(),
		ModelConfigs: []*config.ModelConfig{goodEntry("g1"), goodEntry("g2")},
	})
	g := NewGogent(home)
	if len(modelNames(g)) != 2 {
		t.Fatalf("a clean config should keep both entries; got %v", modelNames(g))
	}
	if ws := g.ConfigWarnings(); len(ws) != 0 {
		t.Errorf("a clean config should yield no warnings; got %v", ws)
	}
}

// TestLoadSweep_NullArrayEntrySkipped: a JSON null element in the models array
// unmarshals to a nil pointer; the sweep must skip it (not panic) and keep the real
// entries, without producing a spurious validation warning.
func TestLoadSweep_NullArrayEntrySkipped(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gogent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := []byte(`{"default_model":"g",` +
		`"connections":[{"name":"c","api_type":"openai","endpoint":"https://api.example.com/v1"}],` +
		`"models":[` +
		`{"name":"g","connection":"c","model":"gm"},` +
		`null]}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), doc, 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	g := NewGogent(home)
	if names := modelNames(g); len(names) != 1 || names[0] != "g" {
		t.Fatalf("a nil array element should be skipped, keeping the real entry; got %v", names)
	}
	if ws := g.ConfigWarnings(); len(ws) != 0 {
		t.Errorf("a nil element is not a validation failure and should not warn; got %v", ws)
	}
}

// TestRoutableDefaultConfig_Table pins the resolution rule shared by defaultConnection
// (new sessions), the SendMessage default fallback, and the listing path (issue #532,
// goal 4): the configured default when it exists AND is routable, else the first
// routable entry, else nil.
func TestRoutableDefaultConfig_Table(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      *config.Config
		wantName string // "" means expect nil
	}{
		{
			name:     "nil config",
			cfg:      nil,
			wantName: "",
		},
		{
			name: "default is routable returns default",
			cfg: &config.Config{
				DefaultModel: "def",
				Connections:  goodConns(),
				ModelConfigs: []*config.ModelConfig{goodEntry("def"), goodEntry("other")},
			},
			wantName: "def",
		},
		{
			name: "default unroutable returns first routable",
			cfg: &config.Config{
				DefaultModel: "bad",
				Connections:  goodConns(),
				ModelConfigs: []*config.ModelConfig{badEntry("bad"), goodEntry("good")},
			},
			wantName: "good",
		},
		{
			name: "default dropped (name absent) returns first routable",
			cfg: &config.Config{
				DefaultModel: "gone",
				Connections:  goodConns(),
				ModelConfigs: []*config.ModelConfig{goodEntry("a"), goodEntry("b")},
			},
			wantName: "a",
		},
		{
			name: "leading unroutable entries skipped first routable wins",
			cfg: &config.Config{
				DefaultModel: "bad0",
				Connections:  goodConns(),
				ModelConfigs: []*config.ModelConfig{badEntry("bad0"), badEntry("bad1"), goodEntry("good")},
			},
			wantName: "good",
		},
		{
			name: "all unroutable returns nil",
			cfg: &config.Config{
				DefaultModel: "bad",
				ModelConfigs: []*config.ModelConfig{badEntry("bad"), badEntry("bad2")},
			},
			wantName: "",
		},
		{
			name: "empty list returns nil",
			cfg: &config.Config{
				DefaultModel: "x",
				ModelConfigs: []*config.ModelConfig{},
			},
			wantName: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routableDefaultConfig(tc.cfg)
			if tc.wantName == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %q", tc.wantName)
			}
			if got.Name != tc.wantName {
				t.Errorf("resolved %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}
