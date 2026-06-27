package main

import (
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
	tuipkg "gogent/ui/tui"
)

// Issue #532 — criterion (2) wiring. The load-swept ConfigWarnings must reach the
// user in EMBEDDED mode (the handler is wired to the local g) and be intentionally
// absent in ATTACHED/remote mode (config is daemon-owned there — the daemon sweeps
// its own config at startup; the client shows no popup). This mirrors the
// GetDefaultModel-nil-while-attached precedent (issue #507) and composes the handler
// sets the way runEmbedded / runAttached do (minus the TTY loop, which is not
// unit-callable).

// TestConfigWarningsHandler_WiredEmbedded verifies the embedded handler set wires
// ConfigWarnings to the local g and surfaces the swept notices, so the startup
// dialog can tell the user which entries were ignored.
func TestConfigWarningsHandler_WiredEmbedded(t *testing.T) {
	home := t.TempDir()
	// Seed a config with one unroutable entry so the sweep records a notice.
	cfg := config.GetDefaultConfig()
	cfg.ModelConfigs = append(cfg.ModelConfigs,
		&config.ModelConfig{Name: "bad", Model: "x"}, // unroutable: no api_type, no endpoint
	)
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	g := gogent.NewGogent(home)
	if len(g.ConfigWarnings()) == 0 {
		t.Fatal("precondition: the load sweep should have recorded a warning for the bad entry")
	}

	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	if h.ConfigWarnings == nil {
		t.Fatal("embedded handlers must wire ConfigWarnings so the startup notice reaches the user")
	}
	if got := h.ConfigWarnings(); len(got) != 1 {
		t.Fatalf("ConfigWarnings() = %v, want 1 notice naming the dropped entry", got)
	}
}

// TestConfigWarningsHandler_NilWhileAttached verifies the attached handler set
// leaves ConfigWarnings nil: config is daemon-owned, so the client shows no popup
// (the daemon sweeps + logs its own config at startup). This composes the handler
// set exactly as runAttached does (rc.Handlers() then installPresentationHandlers).
func TestConfigWarningsHandler_NilWhileAttached(t *testing.T) {
	_, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model")
	clientG := gogent.NewGogent(t.TempDir())

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	if handlers.ConfigWarnings != nil {
		t.Fatal("attached handlers must leave ConfigWarnings nil (config is daemon-owned; the client shows no popup)")
	}
}
