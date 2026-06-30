package gogent

import (
	"testing"
	"time"

	"gogent/internal/config"
	"gogent/internal/model"
)

// Issue #590 — buildConnection is the single point that resolves the effective
// model-request timeout and applies it to the connection: the per-model
// ModelTimeoutSeconds override when set, otherwise the global timeouts.model_seconds
// (which itself falls back to the 300s built-in). These tests drive buildConnection
// directly (it only reads g.config) and read the connection's exported Timeout back,
// proving the override actually reaches the HTTP client — the Part B acceptance gate.
//
// buildConnection is also the one place a per-model override could be silently
// dropped, so the override-wins / global-fallback / default regimes are pinned here
// at the seam rather than only at the config accessor. Hermetic: no requests are
// made (NewModelConnectionFromConfig is lazy).

const defaultModelTimeout = 300 * time.Second

func gogentWithTimeouts(t config.TimeoutConfig) *Gogent {
	return &Gogent{config: &config.Config{Timeouts: t}}
}

func TestBuildConnectionAppliesGlobalModelTimeout(t *testing.T) {
	g := gogentWithTimeouts(config.TimeoutConfig{ModelSeconds: 120})
	conn := g.buildConnection(&config.ModelConfig{Name: "m", Model: "gpt-x"})
	if conn.Timeout != 120*time.Second {
		t.Errorf("conn.Timeout = %v, want 120s (global model timeout)", conn.Timeout)
	}
}

func TestBuildConnectionGlobalTimeoutFallsBackToDefaultWhenUnset(t *testing.T) {
	// ModelSeconds == 0 => the built-in 300s default via ModelSecondsOrDefault.
	g := gogentWithTimeouts(config.TimeoutConfig{})
	conn := g.buildConnection(&config.ModelConfig{Name: "m"})
	if conn.Timeout != defaultModelTimeout {
		t.Errorf("conn.Timeout = %v, want default %v (unset global => 300s)", conn.Timeout, defaultModelTimeout)
	}
}

func TestBuildConnectionPerModelOverrideWinsOverGlobal(t *testing.T) {
	// The headline Part B case: a slow local model gets a longer leash than the
	// global, without raising the timeout for every other backend.
	g := gogentWithTimeouts(config.TimeoutConfig{ModelSeconds: 120})
	conn := g.buildConnection(&config.ModelConfig{
		Name:                "slow-local",
		ModelTimeoutSeconds: 900,
	})
	if conn.Timeout != 900*time.Second {
		t.Errorf("conn.Timeout = %v, want 900s (per-model override must win over the 120s global)", conn.Timeout)
	}
}

func TestBuildConnectionPerModelOverrideAppliesEvenWhenGlobalUnset(t *testing.T) {
	g := gogentWithTimeouts(config.TimeoutConfig{})
	conn := g.buildConnection(&config.ModelConfig{
		Name:                "slow-local",
		ModelTimeoutSeconds: 600,
	})
	if conn.Timeout != 600*time.Second {
		t.Errorf("conn.Timeout = %v, want 600s (override applies even with no global)", conn.Timeout)
	}
}

func TestBuildConnectionUnsetOverrideFallsBackToGlobalExactly(t *testing.T) {
	// override == 0 must be indistinguishable from "no override field": today's
	// behaviour, byte- and behaviour-identical (the Part B non-regression promise).
	g := gogentWithTimeouts(config.TimeoutConfig{ModelSeconds: 150})
	conn := g.buildConnection(&config.ModelConfig{Name: "m"})
	if conn.Timeout != 150*time.Second {
		t.Errorf("conn.Timeout = %v, want 150s (unset override => global, unchanged)", conn.Timeout)
	}
}

func TestBuildConnectionNegativeOverrideFallsBackToGlobal(t *testing.T) {
	// A negative override (a hand-edit typo) must NOT tighten the timeout to a
	// negative duration; it falls back to the global like an unset value.
	g := gogentWithTimeouts(config.TimeoutConfig{ModelSeconds: 150})
	conn := g.buildConnection(&config.ModelConfig{
		Name:                "m",
		ModelTimeoutSeconds: -10,
	})
	if conn.Timeout != 150*time.Second {
		t.Errorf("conn.Timeout = %v, want 150s (negative override => global, defensive)", conn.Timeout)
	}
}

func TestBuildConnectionNilConfigKeepsConnectionDefaultAndDoesNotPanic(t *testing.T) {
	// g.config == nil guards the SetTimeout call (e.g. a not-yet-configured Gogent),
	// so the connection keeps NewModelConnectionFromConfig's own 5m default and the
	// build does not panic.
	g := &Gogent{config: nil}
	conn := g.buildConnection(&config.ModelConfig{Name: "m"})
	if conn == nil {
		t.Fatal("buildConnection returned nil")
	}
	if conn.Timeout != 5*time.Minute {
		t.Errorf("conn.Timeout = %v, want NewModelConnectionFromConfig's 5m default when g.config is nil", conn.Timeout)
	}
}

// TestBuildConnectionOverrideResolutionMatchesAccessor is a cross-check: the value
// buildConnection writes equals ModelTimeoutSecondsOrDefault(global) for the same
// inputs, so the resolution logic has exactly one source of truth. Regression guard
// against the two diverging (e.g. buildConnection re-implementing the fallback).
func TestBuildConnectionOverrideResolutionMatchesAccessor(t *testing.T) {
	cfg := &config.ModelConfig{Name: "m", ModelTimeoutSeconds: 42}
	g := gogentWithTimeouts(config.TimeoutConfig{ModelSeconds: 200})
	conn := g.buildConnection(cfg)
	want := time.Duration(cfg.ModelTimeoutSecondsOrDefault(g.config.Timeouts.ModelSecondsOrDefault())) * time.Second
	if conn.Timeout != want {
		t.Errorf("conn.Timeout = %v, want accessor-derived %v", conn.Timeout, want)
	}
}

// keep model import used even if future edits drop a direct reference.
var _ = model.NewModelConnection
