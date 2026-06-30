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

// TestValidateConnectionAndModelConfig pins the split routability rule: connection
// routability is checked against the ProviderConnection (ValidateConnection), while
// model-level rules (hosted-gateway empty model, Vertex model-id shape) are checked
// by ValidateModelConfig(pc, m). There is still one rule with no drift, now keyed on
// the connection + model rather than a flat config.
func TestValidateConnectionAndModelConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pc      *config.ProviderConnection
		m       *config.ModelConfig
		wantErr bool
	}{
		{"nil/nil", nil, nil, false},
		{"empty api_type + endpoint (unroutable)", &config.ProviderConnection{Name: "bad"}, &config.ModelConfig{Name: "x", Model: "x"}, true},
		{"unrecognized api_type", &config.ProviderConnection{Name: "typo", APIType: "opnai"}, &config.ModelConfig{Name: "x", Model: "x"}, true},
		{"openrouter empty model", &config.ProviderConnection{Name: "gw", APIType: "openrouter"}, &config.ModelConfig{Name: "x"}, true},
		{"openai empty endpoint (local default)", &config.ProviderConnection{Name: "o", APIType: "openai"}, &config.ModelConfig{Name: "x", Model: "m"}, false},
		{"explicit endpoint empty api_type", &config.ProviderConnection{Name: "e", Endpoint: "https://api.example.com/v1"}, &config.ModelConfig{Name: "x", Model: "m"}, false},
		{"anthropic empty model (accepted)", &config.ProviderConnection{Name: "a", APIType: "anthropic"}, &config.ModelConfig{Name: "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateModelConfig(tc.pc, tc.m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateModelConfig = %v, wantErr=%v", err, tc.wantErr)
			}
			// Connection routability must agree on the connection-only verdict.
			if tc.pc != nil {
				connErr := ValidateConnection(tc.pc)
				routability := tc.name == "empty api_type + endpoint (unroutable)" || tc.name == "unrecognized api_type"
				if (connErr != nil) != routability {
					t.Errorf("ValidateConnection = %v, want routability=%v", connErr, routability)
				}
			}
		})
	}
}

// TestValidateConnection_RejectsAsModelError verifies an unroutable connection is
// rejected as a *model.ModelError (connection-named, field-naming). gogent wraps it
// with ErrModelInvalid; the actionable *ModelError must still be recoverable via
// errors.As through that double-wrap (exercised in the gogent package tests).
func TestValidateConnection_RejectsAsModelError(t *testing.T) {
	err := ValidateConnection(&config.ProviderConnection{Name: "ghost"})
	if err == nil {
		t.Fatal("an unroutable connection must be rejected")
	}
	var me *ModelError
	if !errors.As(err, &me) {
		t.Fatalf("rejection must be a *ModelError, got %T: %v", err, err)
	}
	if !strings.Contains(me.Message, `connection "ghost"`) {
		t.Errorf("error must name the connection; got %q", me.Message)
	}
	if !strings.Contains(me.Message, "api_type and endpoint are both empty") {
		t.Errorf("error must explain the routability failure; got %q", me.Message)
	}
}

// TestNewUnroutableConnection_FailsWithClearMessage_NoDial is the Defect-1.1 fix:
// when no routable model is configured, gogent builds this connection so the first
// completion fails fast with the actionable message instead of silently targeting
// DefaultModelURL (localhost) and 404ing. A bare newPlaceholderConnection() has
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
	bare := newPlaceholderConnection()
	if bare.configErr != nil {
		t.Fatalf("test premise: bare NewModelConnection must have nil configErr, got %v", bare.configErr)
	}
	safe := NewUnroutableConnection("x")
	if safe.configErr == nil {
		t.Fatal("NewUnroutableConnection must set configErr (unlike the bare placeholder)")
	}
}
