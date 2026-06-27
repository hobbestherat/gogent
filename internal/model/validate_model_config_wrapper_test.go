package model

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gogent/internal/config"
)

// Issue #532: model.ValidateModelConfig is the exported wrapper over the private
// validateRoutableConfig so gogent's save/load/use paths apply the single
// routability rule at the source instead of only lazily at connection-build time.
// NewUnroutableConnection is the no-routable-model fail-safe (Defect 1.1): it
// carries a deferred configErr so the first completion/scan fails with a clear,
// actionable message instead of silently dialing the DefaultModelURL placeholder.
//
// These tests lock (a) the wrapper is a true passthrough (same verdicts as the
// private function, so there is one rule with no drift), (b) its rejection is a
// model-named *ModelError the server can classify and the TUI can surface, and
// (c) the fail-safe connection errors clearly without dialing.

// TestValidateModelConfig_MatchesValidateRoutableConfig pins the wrapper as a
// passthrough: for every representative config, the exported and private functions
// agree (both nil, or both erroring with the same message). A drift here would mean
// two routability rules, which is exactly what the issue forbids.
func TestValidateModelConfig_MatchesValidateRoutableConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.ModelConfig
	}{
		{"nil", nil},
		{"empty api_type + endpoint (unroutable)", &config.ModelConfig{Name: "bad", Model: "x"}},
		{"unrecognized api_type", &config.ModelConfig{Name: "typo", APIType: "opnai", Model: "x"}},
		{"openrouter empty model", &config.ModelConfig{Name: "gw", APIType: "openrouter"}},
		{"openai empty endpoint (local default)", &config.ModelConfig{Name: "o", APIType: "openai", Model: "m"}},
		{"explicit endpoint empty api_type", &config.ModelConfig{Name: "e", Endpoint: "https://api.example.com/v1", Model: "m"}},
		{"anthropic empty model (accepted)", &config.ModelConfig{Name: "a", APIType: "anthropic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expErr := validateRoutableConfig(tc.cfg)
			gotErr := ValidateModelConfig(tc.cfg)
			if (expErr == nil) != (gotErr == nil) {
				t.Fatalf("wrapper disagrees with private function: private=%v wrapper=%v", expErr, gotErr)
			}
			if expErr != nil && gotErr != nil && expErr.Error() != gotErr.Error() {
				t.Errorf("verdict agreed but message drifted: private=%q wrapper=%q", expErr.Error(), gotErr.Error())
			}
		})
	}
}

// TestValidateModelConfig_RejectsAsModelError verifies the wrapper's rejection is a
// *model.ModelError (model-named, field-naming). gogent wraps it with its
// ErrModelInvalid sentinel; the actionable *ModelError must still be recoverable via
// errors.As through that double-wrap (exercised in the gogent package tests).
func TestValidateModelConfig_RejectsAsModelError(t *testing.T) {
	err := ValidateModelConfig(&config.ModelConfig{Name: "ghost", Model: "x"})
	if err == nil {
		t.Fatal("an unroutable config must be rejected")
	}
	var me *ModelError
	if !errors.As(err, &me) {
		t.Fatalf("rejection must be a *ModelError, got %T: %v", err, err)
	}
	if !strings.Contains(me.Message, `model "ghost"`) {
		t.Errorf("error must name the model; got %q", me.Message)
	}
	if !strings.Contains(me.Message, "api_type and endpoint are both empty") {
		t.Errorf("error must explain the routability failure; got %q", me.Message)
	}
}

// TestNewUnroutableConnection_FailsWithClearMessage_NoDial is the Defect-1.1 fix:
// when no routable model is configured, gogent builds this connection so the first
// completion fails fast with the actionable message instead of silently targeting
// DefaultModelURL (localhost) and 404ing. A bare NewModelConnection() has
// configErr == nil and would dial; this must not.
func TestNewUnroutableConnection_FailsWithClearMessage_NoDial(t *testing.T) {
	const want = "no routable model is configured — add a model"
	conn := NewUnroutableConnection(want)

	start := time.Now()
	resp, err := conn.Complete([]Message{{Role: RoleUser, Content: "hi"}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Complete on the fail-safe connection must return the clear error, got nil")
	}
	if resp != nil {
		t.Errorf("resp must be nil on the fail-safe error, got %+v", resp)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to carry the actionable message %q", err.Error(), want)
	}
	// It must short-circuit without dialing localhost (sub-second, no network wait).
	if elapsed > 2*time.Second {
		t.Errorf("fail-safe should short-circuit without dialing; took %v", elapsed)
	}
}

// TestNewUnroutableConnection_AlsoFailsStreamAndList guards the other network paths
// (the agent loop's streaming entrypoint and the model-listing scan) so the fail-safe
// is uniformly safe, not just on the blocking completion.
func TestNewUnroutableConnection_AlsoFailsStreamAndList(t *testing.T) {
	conn := NewUnroutableConnection("no routable model")

	streamCh, errCh := conn.CompleteStream([]Message{{Role: RoleUser, Content: "hi"}})
	var got error
	select {
	case err, ok := <-errCh:
		if ok {
			got = err
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CompleteStream did not surface the fail-safe error within 2s (likely dialing localhost)")
	}
	for range streamCh { // drain so the goroutine can finish
	}
	if got == nil {
		t.Fatal("CompleteStream must deliver the fail-safe error on the error channel")
	}

	if _, err := conn.ListModels(); err == nil {
		t.Error("ListModels must surface the fail-safe error, got nil")
	}
}

// TestNewUnroutableConnection_DistinctFromBarePlaceholder guards the whole point of
// the fix: the bare library default has configErr == nil and would silently target
// localhost, so the no-model fallback must carry a configErr and never be the bare
// placeholder.
func TestNewUnroutableConnection_DistinctFromBarePlaceholder(t *testing.T) {
	bare := NewModelConnection()
	if bare.configErr != nil {
		t.Fatalf("test premise: bare NewModelConnection must have nil configErr, got %v", bare.configErr)
	}
	safe := NewUnroutableConnection("x")
	if safe.configErr == nil {
		t.Fatal("NewUnroutableConnection must set configErr (unlike the bare placeholder)")
	}
}
