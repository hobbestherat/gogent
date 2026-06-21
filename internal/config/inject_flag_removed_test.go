package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLegacyInjectQueuedInputStillLoads is the backward-compatibility guarantee
// for the flag removal in issue #201: a config.json written by an older gogent
// that sets experimental.inject_queued_input still loads without error. The field
// is gone from ExperimentalConfig, so the JSON key is now an unknown key and
// encoding/json silently ignores it.
func TestLegacyInjectQueuedInputStillLoads(t *testing.T) {
	const legacy = `{
		"default_model": "test",
		"models": [],
		"experimental": {
			"inject_queued_input": true,
			"supervisor": true
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(legacy), &cfg); err != nil {
		t.Fatalf("legacy config with experimental.inject_queued_input should still load, got error: %v", err)
	}
	// The supervisor flag — the other experimental knob — must still parse, since
	// ExperimentalConfig was kept for it (#172).
	if !cfg.Experimental.Supervisor {
		t.Error("experimental.supervisor should still parse after the flag removal")
	}
}

// TestExperimentalConfigHasNoInjectField confirms the inject_queued_input field is
// gone from ExperimentalConfig (issue #201): marshaling the struct never emits the
// key, whether the supervisor flag is set or not. This catches an accidental
// re-add of the field.
func TestExperimentalConfigHasNoInjectField(t *testing.T) {
	for _, ec := range []ExperimentalConfig{
		{},                 // zero value
		{Supervisor: true}, // supervisor on
	} {
		data, err := json.Marshal(ec)
		if err != nil {
			t.Fatalf("marshal %+v: %v", ec, err)
		}
		if strings.Contains(string(data), "inject_queued_input") {
			t.Errorf("ExperimentalConfig%+v still serializes inject_queued_input: %s", ec, data)
		}
	}
	// A config that turns the supervisor on round-trips through JSON.
	in := &Config{Experimental: ExperimentalConfig{Supervisor: true}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if !out.Experimental.Supervisor {
		t.Error("supervisor flag lost in round-trip")
	}
}

// TestInjectQueuedInputKeyIgnoredNotStrict confirms the config loader is not in
// strict/DisallowUnknownFields mode: an unknown top-level key and an unknown
// experimental key are both tolerated, so deleting the flag cannot break existing
// config files (issue #201 acceptance: "configs that still set it load without
// error").
func TestInjectQueuedInputKeyIgnoredNotStrict(t *testing.T) {
	const withUnknownKeys = `{
		"default_model": "x",
		"experimental": {"inject_queued_input": true},
		"some_future_key": 42
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(withUnknownKeys), &cfg); err != nil {
		t.Fatalf("unknown keys should be ignored (non-strict unmarshal): %v", err)
	}
	if cfg.DefaultModel != "x" {
		t.Errorf("default_model = %q, want x", cfg.DefaultModel)
	}
}
