package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// Issue #590 "[UX] Timeouts" — the discoverability/structure change that relocates
// the model/tool/sub-agent timeouts out of the Sub-agent dialog into their own
// "Timeouts" page, and adds an OPTIONAL per-model model-timeout override.
//
// These tests pin the config-layer invariants the issue and the 4 design gates rest
// on — none of which the pre-existing suite covered (there was no TimeoutConfig test):
//
//   (1) GOAL MATCH — the per-model ModelTimeoutSeconds override resolves with the
//       correct precedence (override > global; unset => global exactly).
//   (2) USABILITY / BACKWARD COMPAT (hard requirement) — the on-disk schema is
//       unchanged: a config.json that predates the change still loads with identical
//       effective timeouts; defaults are the 300s built-ins, untouched.
//   (3) NO REGRESSIONS — TimeoutConfig round-trips its three fields; the per-model
//       override is omitempty so an unset value never perturbs an old config's bytes.
//
// All hermetic: no network, no sleeps, no clock.

// defaultTimeoutSecs is the single built-in default every timeout falls back to.
// Asserted once here so an accidental change to the constant is caught loudly — the
// issue is explicit that this is a discoverability change, NOT a behaviour change.
const defaultTimeoutSecs = 300

func TestTimeoutConfigDefaultsAreTheShipped300s(t *testing.T) {
	d := DefaultTimeoutConfig()
	if d.ModelSeconds != defaultTimeoutSecs || d.ToolSeconds != defaultTimeoutSecs || d.SubAgentSeconds != defaultTimeoutSecs {
		t.Fatalf("DefaultTimeoutConfig = %+v, want all %ds (issue #590 forbids changing the defaults)", d, defaultTimeoutSecs)
	}
	// A zero TimeoutConfig (e.g. an older config that omits a field) resolves to the
	// same defaults via the *OrDefault accessors — this is the "unset => default"
	// contract every consumer (gogent.go buildConnection, toolRegistry, SetSubAgentTimeout)
	// depends on.
	var zero TimeoutConfig
	if got := zero.ModelSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("zero.ModelSecondsOrDefault = %d, want %d", got, defaultTimeoutSecs)
	}
	if got := zero.ToolSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("zero.ToolSecondsOrDefault = %d, want %d", got, defaultTimeoutSecs)
	}
	if got := zero.SubAgentSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("zero.SubAgentSecondsOrDefault = %d, want %d", got, defaultTimeoutSecs)
	}
}

func TestTimeoutConfigOrDefaultHonoursPositiveValuesAndFallsBackOnNonPositive(t *testing.T) {
	// Positive values pass through verbatim; zero AND negative (a typo) fall back.
	tc := TimeoutConfig{ModelSeconds: 42, ToolSeconds: 0, SubAgentSeconds: -7}
	if got := tc.ModelSecondsOrDefault(); got != 42 {
		t.Errorf("ModelSecondsOrDefault = %d, want 42 (positive passes through)", got)
	}
	if got := tc.ToolSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("ToolSecondsOrDefault = %d, want %d (0 => default)", got, defaultTimeoutSecs)
	}
	if got := tc.SubAgentSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("SubAgentSecondsOrDefault = %d, want %d (negative => default, defensive)", got, defaultTimeoutSecs)
	}
}

// TestTimeoutConfigRoundTripPreservesAllThreeFields pins the explicit/relocated
// form: a TimeoutConfig with every field set survives marshal→unmarshal with its
// values intact and under the unchanged top-level "timeouts" JSON keys.
func TestTimeoutConfigRoundTripPreservesAllThreeFields(t *testing.T) {
	in := TimeoutConfig{ModelSeconds: 111, ToolSeconds: 222, SubAgentSeconds: 333}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"model_seconds":111,"tool_seconds":222,"subagent_seconds":333}`
	if got := string(data); got != want {
		t.Errorf("marshal = %s, want %s (the JSON keys must not have moved — issue #590)", got, want)
	}
	var out TimeoutConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip = %+v, want %+v", out, in)
	}
}

// TestTimeoutConfigRoundTripThroughConfig pins the keys live under the top-level
// Config.timeouts block (NOT under sub_agents) and survive a full Config round-trip.
// This is the structural heart of the #590 fix: the timeouts were only *displayed*
// under Sub-agents; on disk they were always their own block, so the relocation is
// UI-only and no key migrates.
func TestTimeoutConfigRoundTripThroughConfig(t *testing.T) {
	in := &Config{
		DefaultModel: "m",
		SubAgents:    DefaultSubAgentConfig(),
		Timeouts:     TimeoutConfig{ModelSeconds: 10, ToolSeconds: 20, SubAgentSeconds: 30},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The timeouts block is a sibling of sub_agents, never nested inside it.
	s := string(data)
	if !strings.Contains(s, `"timeouts":{"model_seconds":10`) {
		t.Errorf("timeouts block missing/malformed in Config JSON: %s", s)
	}
	if i := strings.Index(s, `"sub_agents"`); i >= 0 {
		j := strings.Index(s, `"timeouts"`)
		// Both present and timeouts is not nested within the sub_agents value: the
		// timeouts key must appear AFTER the sub_agents block closes (a top-level key).
		if j < i {
			t.Errorf("timeouts key appears before sub_agents in Config JSON (unexpected ordering): %s", s)
		}
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Timeouts != in.Timeouts {
		t.Errorf("round-trip Timeouts = %+v, want %+v", out.Timeouts, in.Timeouts)
	}
}

// TestSubAgentConfigHasNoTimeoutFields is the regression guard for the issue's
// premise: timeouts must NOT be part of SubAgentConfig. If a future change re-buried
// them here, the whole discoverability fix would silently undo, so encode the
// absence explicitly.
func TestSubAgentConfigHasNoTimeoutFields(t *testing.T) {
	data, err := json.Marshal(SubAgentConfig{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"model_seconds", "tool_seconds", "subagent_seconds", "timeouts"} {
		if strings.Contains(string(data), key) {
			t.Errorf("SubAgentConfig JSON unexpectedly contains %q — timeouts must live in TimeoutConfig, not here", key)
		}
	}
}

// -----------------------------------------------------------------------------
// Backward compatibility (HARD requirement): old configs load identically.
// -----------------------------------------------------------------------------

// TestOldConfigWithTimeoutsBlockLoadsIdentically simulates a config.json written by
// a prior gogent release: a top-level "timeouts" block alongside "sub_agents". Such
// a config MUST continue to load with exactly the configured effective timeouts —
// the relocation added no migration and renamed no keys.
func TestOldConfigWithTimeoutsBlockLoadsIdentically(t *testing.T) {
	const oldConfigJSON = `{
		"default_model": "local-lan",
		"sub_agents": {"execution_model": "both", "max_subagents": 4, "max_depth": 3},
		"timeouts": {"model_seconds": 90, "tool_seconds": 45, "subagent_seconds": 60}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(oldConfigJSON), &cfg); err != nil {
		t.Fatalf("old config failed to load: %v", err)
	}
	if got := cfg.Timeouts.ModelSecondsOrDefault(); got != 90 {
		t.Errorf("old model timeout = %d, want 90", got)
	}
	if got := cfg.Timeouts.ToolSecondsOrDefault(); got != 45 {
		t.Errorf("old tool timeout = %d, want 45", got)
	}
	if got := cfg.Timeouts.SubAgentSecondsOrDefault(); got != 60 {
		t.Errorf("old sub-agent timeout = %d, want 60", got)
	}
	// sub_agents is untouched too (the dialog that hosted them still reads it).
	if cfg.SubAgents.ExecutionModel != SubAgentBothModel {
		t.Errorf("sub_agents.execution_model = %q, want both", cfg.SubAgents.ExecutionModel)
	}
}

// TestOldConfigWithoutTimeoutsBlockYieldsDefaults covers the other old-config shape:
// a config.json that predates the timeouts block entirely (or omits it). It MUST
// resolve to the built-in 300s defaults — not zero, not "off", not a behaviour change.
func TestOldConfigWithoutTimeoutsBlockYieldsDefaults(t *testing.T) {
	const oldConfigJSON = `{
		"default_model": "local-lan",
		"sub_agents": {"execution_model": "one_shot"}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(oldConfigJSON), &cfg); err != nil {
		t.Fatalf("config without timeouts block failed to load: %v", err)
	}
	if got := cfg.Timeouts.ModelSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("model timeout with no block = %d, want default %d", got, defaultTimeoutSecs)
	}
	if got := cfg.Timeouts.ToolSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("tool timeout with no block = %d, want default %d", got, defaultTimeoutSecs)
	}
	if got := cfg.Timeouts.SubAgentSecondsOrDefault(); got != defaultTimeoutSecs {
		t.Errorf("sub-agent timeout with no block = %d, want default %d", got, defaultTimeoutSecs)
	}
}

// TestDefaultConfigSerializesTimeouts pins GetDefaultConfig — the config a fresh
// install writes — still documents the timeouts block, so a user opening config.json
// sees the explicit, discoverable knobs the issue asks for.
func TestDefaultConfigSerializesTimeouts(t *testing.T) {
	cfg := GetDefaultConfig()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	if !strings.Contains(string(data), `"timeouts":`) {
		t.Errorf("default config JSON has no timeouts block: %s", string(data))
	}
	if cfg.Timeouts != DefaultTimeoutConfig() {
		t.Errorf("default Timeouts = %+v, want %+v", cfg.Timeouts, DefaultTimeoutConfig())
	}
}

// -----------------------------------------------------------------------------
// Part B: per-model ModelTimeoutSeconds override (resolution gate).
// -----------------------------------------------------------------------------

func TestModelTimeoutSecondsOrDefaultResolution(t *testing.T) {
	const global = 120 // stand-in for TimeoutConfig.ModelSecondsOrDefault()
	cases := []struct {
		name string
		m    *ModelConfig
		want int
	}{
		{"nil receiver falls back to global", nil, global},
		{"zero value falls back to global", &ModelConfig{}, global},
		{"negative (typo) falls back to global", &ModelConfig{ModelTimeoutSeconds: -5}, global},
		{"positive override wins over global", &ModelConfig{ModelTimeoutSeconds: 900}, 900},
		{"override of 1 is honoured (minimum valid)", &ModelConfig{ModelTimeoutSeconds: 1}, 1},
	}
	for _, tc := range cases {
		if got := tc.m.ModelTimeoutSecondsOrDefault(global); got != tc.want {
			t.Errorf("%s: ModelTimeoutSecondsOrDefault(%d) = %d, want %d", tc.name, global, got, tc.want)
		}
	}
}

// TestEffectiveModelTimeoutResolution is the end-to-end resolution gate the design
// calls out: feed the accessor the GLOBAL effective value (ModelSecondsOrDefault) and
// assert the four regimes — nothing set => default; global set, no override => global;
// global set + override => override wins; no global + override => override.
func TestEffectiveModelTimeoutResolution(t *testing.T) {
	// Nothing configured anywhere => built-in default.
	var tc TimeoutConfig
	m := &ModelConfig{}
	if got := m.ModelTimeoutSecondsOrDefault(tc.ModelSecondsOrDefault()); got != defaultTimeoutSecs {
		t.Errorf("nothing set: effective = %d, want default %d", got, defaultTimeoutSecs)
	}
	// Global set, override unset => global.
	tc = TimeoutConfig{ModelSeconds: 150}
	m = &ModelConfig{}
	if got := m.ModelTimeoutSecondsOrDefault(tc.ModelSecondsOrDefault()); got != 150 {
		t.Errorf("global only: effective = %d, want 150", got)
	}
	// Global set + override => override wins.
	m = &ModelConfig{ModelTimeoutSeconds: 600}
	if got := m.ModelTimeoutSecondsOrDefault(tc.ModelSecondsOrDefault()); got != 600 {
		t.Errorf("override present: effective = %d, want 600 (override must win)", got)
	}
	// Global unset + override => override (falls back through the default to the model).
	tc = TimeoutConfig{}
	if got := m.ModelTimeoutSecondsOrDefault(tc.ModelSecondsOrDefault()); got != 600 {
		t.Errorf("override with no global: effective = %d, want 600", got)
	}
}

// TestModelTimeoutSecondsOmitemptyKeepsOldConfigsByteIdentical pins the wire/disk
// contract: an unset override (0) MUST be omitted so a config.json round-trip on an
// old install stays byte-stable (no new key appears), and a set override serialises.
func TestModelTimeoutSecondsOmitemptyKeepsOldConfigsByteIdentical(t *testing.T) {
	unset, err := json.Marshal(&ModelConfig{Name: "m"})
	if err != nil {
		t.Fatalf("marshal unset: %v", err)
	}
	if strings.Contains(string(unset), "model_timeout_seconds") {
		t.Errorf("unset override leaked into JSON (%s); omitempty must keep old configs byte-identical", unset)
	}
	set, err := json.Marshal(&ModelConfig{Name: "m", ModelTimeoutSeconds: 700})
	if err != nil {
		t.Fatalf("marshal set: %v", err)
	}
	if !strings.Contains(string(set), `"model_timeout_seconds":700`) {
		t.Errorf("set override missing from JSON: %s", set)
	}
	// Round-trip the set value back.
	var back ModelConfig
	if err := json.Unmarshal(set, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ModelTimeoutSeconds != 700 {
		t.Errorf("round-trip = %d, want 700", back.ModelTimeoutSeconds)
	}
}
