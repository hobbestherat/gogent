package permission

import "testing"

// TestActionWatcherConstant pins the wire value of the watcher action so the
// gogent gate, persisted rules, and audit trail agree on the token.
func TestActionWatcherConstant(t *testing.T) {
	if ActionWatcher != "watcher" {
		t.Errorf("ActionWatcher = %q, want \"watcher\"", ActionWatcher)
	}
}

// TestActionWatcherGate confirms ActionWatcher flows through the standard
// Service gate: default-deny when headless (no prompter), allow under a matching
// rule, and deny under an explicit deny rule — each scoped to the watcher name
// used as the resource.
func TestActionWatcherGate(t *testing.T) {
	// No prompter installed -> "ask" resolves to deny.
	s := New("")
	if err := s.Check(ActionWatcher, "daily"); err == nil {
		t.Error("headless ActionWatcher check should default to deny")
	}

	allow := New("")
	allow.AddRule(Rule{Action: string(ActionWatcher), Resource: "*", Effect: string(EffectAllow)})
	if err := allow.Check(ActionWatcher, "daily"); err != nil {
		t.Errorf("allowed ActionWatcher check should pass, got %v", err)
	}

	deny := New("")
	deny.AddRule(Rule{Action: string(ActionWatcher), Resource: "*", Effect: string(EffectDeny)})
	if err := deny.Check(ActionWatcher, "daily"); err == nil {
		t.Error("denied ActionWatcher check should fail")
	}

	// A name-scoped allow rule covers only that watcher, leaving others to deny —
	// the same per-resource safety property MCP relies on.
	scoped := New("")
	scoped.AddRule(Rule{Action: string(ActionWatcher), Resource: "blessed", Effect: string(EffectAllow)})
	if err := scoped.Check(ActionWatcher, "blessed"); err != nil {
		t.Errorf("scoped allow should permit its own watcher, got %v", err)
	}
	if err := scoped.Check(ActionWatcher, "other"); err == nil {
		t.Error("scoped allow must not leak to a different watcher name")
	}
}
