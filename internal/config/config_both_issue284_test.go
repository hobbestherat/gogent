package config

import (
	"encoding/json"
	"testing"
)

// Issue #284: interactive (fire-and-forget) delegation is made accessible by
// default via a new "both" execution model that exposes the blocking
// spawn_subagent AND the asynchronous launch_agent family in one session. These
// tests pin the accessor matrix (IsOneShot / ExposesOneShotTools /
// ExposesInteractiveTools) that toolRegistryForMode and coordinatorInstructions
// switch on, including the empty/unset and unknown-value fallbacks.

// TestSubAgentExposureMatrix exhaustively asserts which coordination tool sets
// each execution model exposes. ExposesOneShotTools gates spawn_subagent;
// ExposesInteractiveTools gates the launch_agent family.
func TestSubAgentExposureMatrix(t *testing.T) {
	cases := []struct {
		name        string
		model       SubAgentExecutionModel
		wantOneShot bool // ExposesOneShotTools (spawn_subagent visible)
		wantInter   bool // ExposesInteractiveTools (launch_agent family visible)
		wantIsOne   bool // IsOneShot (prompt-branch / UI mode label)
	}{
		{"both", SubAgentBothModel, true, true, false},
		{"one_shot", SubAgentOneShotModel, true, false, true},
		{"interactive", SubAgentInteractiveModel, false, true, false},
		{"empty_unset", SubAgentExecutionModel(""), true, false, true},
		{"unknown_garbage", SubAgentExecutionModel("nonsense"), true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := SubAgentConfig{ExecutionModel: tc.model}
			if got := c.ExposesOneShotTools(); got != tc.wantOneShot {
				t.Errorf("ExposesOneShotTools() = %v, want %v", got, tc.wantOneShot)
			}
			if got := c.ExposesInteractiveTools(); got != tc.wantInter {
				t.Errorf("ExposesInteractiveTools() = %v, want %v", got, tc.wantInter)
			}
			if got := c.IsOneShot(); got != tc.wantIsOne {
				t.Errorf("IsOneShot() = %v, want %v", got, tc.wantIsOne)
			}
		})
	}
}

// TestEveryModelExposesAtLeastOneStyle is an invariant: no execution model may
// strip BOTH coordination tool sets, which would leave a session unable to
// delegate at all. A regression here (e.g. a typo flipping a condition) would
// silently disable delegation.
func TestEveryModelExposesAtLeastOneStyle(t *testing.T) {
	models := []SubAgentExecutionModel{
		SubAgentBothModel, SubAgentOneShotModel, SubAgentInteractiveModel,
		SubAgentExecutionModel(""), SubAgentExecutionModel("weird"),
	}
	for _, m := range models {
		c := SubAgentConfig{ExecutionModel: m}
		if !c.ExposesOneShotTools() && !c.ExposesInteractiveTools() {
			t.Errorf("model %q exposes neither tool set; delegation would be impossible", m)
		}
	}
}

// TestIsOneShotConsistentWithExposure documents the intended relationship
// between the three accessors: IsOneShot is true exactly when one-shot tools are
// exposed and interactive tools are not. "both" and "interactive" are NOT
// one-shot even though "both" exposes the blocking tool.
func TestIsOneShotConsistentWithExposure(t *testing.T) {
	models := []SubAgentExecutionModel{
		SubAgentBothModel, SubAgentOneShotModel, SubAgentInteractiveModel,
		SubAgentExecutionModel(""), SubAgentExecutionModel("weird"),
	}
	for _, m := range models {
		c := SubAgentConfig{ExecutionModel: m}
		want := c.ExposesOneShotTools() && !c.ExposesInteractiveTools()
		if c.IsOneShot() != want {
			t.Errorf("model %q: IsOneShot()=%v, but (ExposesOneShot && !ExposesInteractive)=%v",
				m, c.IsOneShot(), want)
		}
	}
}

// TestDefaultSubAgentConfigIsBoth pins the headline behavior change of issue
// #284: the shipped default now exposes BOTH delegation styles, so fire-and-forget
// delegation is reachable without a mode switch. The conservative bounds
// (recursion off, default caps) must be preserved.
func TestDefaultSubAgentConfigIsBoth(t *testing.T) {
	d := DefaultSubAgentConfig()
	if d.ExecutionModel != SubAgentBothModel {
		t.Errorf("default ExecutionModel = %q, want %q", d.ExecutionModel, SubAgentBothModel)
	}
	if !d.ExposesOneShotTools() || !d.ExposesInteractiveTools() {
		t.Errorf("default must expose both styles: oneShot=%v interactive=%v",
			d.ExposesOneShotTools(), d.ExposesInteractiveTools())
	}
	if d.IsOneShot() {
		t.Error("default must NOT report IsOneShot (it is the 'both' model)")
	}
	if d.AllowRecursive {
		t.Error("default must keep recursion OFF for safety")
	}
	if d.MaxSubAgents != defaultMaxSubAgents {
		t.Errorf("default MaxSubAgents = %d, want %d", d.MaxSubAgents, defaultMaxSubAgents)
	}
	if d.MaxDepth != defaultMaxDepth {
		t.Errorf("default MaxDepth = %d, want %d", d.MaxDepth, defaultMaxDepth)
	}
	if d.MaxConcurrent != defaultMaxConcurrent {
		t.Errorf("default MaxConcurrent = %d, want %d", d.MaxConcurrent, defaultMaxConcurrent)
	}
}

// TestGetDefaultConfigUsesBoth checks the top-level config (not just the helper)
// ships the both-styles default, so a fresh install gets it.
func TestGetDefaultConfigUsesBoth(t *testing.T) {
	cfg := GetDefaultConfig()
	if cfg.SubAgents.ExecutionModel != SubAgentBothModel {
		t.Errorf("GetDefaultConfig SubAgents.ExecutionModel = %q, want %q",
			cfg.SubAgents.ExecutionModel, SubAgentBothModel)
	}
}

// TestSubAgentConfigJSONRoundTrip verifies the "both" model survives the
// persistence round trip (config is saved to / loaded from disk on settings
// changes) and that the JSON tag value is the literal "both".
func TestSubAgentConfigJSONRoundTrip(t *testing.T) {
	d := DefaultSubAgentConfig()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The persisted value must be the literal string "both".
	var probe struct {
		ExecutionModel string `json:"execution_model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.ExecutionModel != "both" {
		t.Errorf("persisted execution_model = %q, want \"both\"", probe.ExecutionModel)
	}
	var back SubAgentConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ExecutionModel != SubAgentBothModel {
		t.Errorf("round-tripped ExecutionModel = %q, want %q", back.ExecutionModel, SubAgentBothModel)
	}
	if !back.ExposesOneShotTools() || !back.ExposesInteractiveTools() {
		t.Error("round-tripped config lost the both-styles exposure")
	}
}

// TestZeroValueConfigDefaultsToOneShot guards backward compatibility: a config
// struct deserialized from older data with no execution_model field (the zero
// value) must behave as the stable blocking-only mode, never accidentally
// enabling the experimental async tools.
func TestZeroValueConfigDefaultsToOneShot(t *testing.T) {
	var c SubAgentConfig // zero value, ExecutionModel == ""
	if !c.IsOneShot() {
		t.Error("zero-value config must report IsOneShot for backward compatibility")
	}
	if c.ExposesInteractiveTools() {
		t.Error("zero-value config must NOT expose the experimental interactive tools")
	}
	if !c.ExposesOneShotTools() {
		t.Error("zero-value config must still expose blocking spawn_subagent")
	}
}
