package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gogent/internal/config"
	"gogent/internal/gogent"
	"gogent/internal/server"
	tuipkg "gogent/ui/tui"
)

// Regression coverage for issue #507: in attached mode the default model is daemon-owned,
// so installPresentationHandlers must NO LONGER wire GetDefaultModel/SetDefaultModel to
// the local client core. This file composes the handler set the way runAttached does
// (rc.Handlers() then installPresentationHandlers) — minus the signal/SSH/TUI loop, which
// is not unit-callable — and asserts the split-brain is gone end-to-end.
//
// Design criteria under test:
//   (1) goal — an attached settings-only session leaves the CLIENT's config.json
//       byte-unchanged for the daemon-owned field (default_model); GetDefaultModel reads
//       the daemon; SetDefaultModel writes the daemon.
//   (2) usability — "Set as default" while attached flips the DAEMON core, not the client.
//   (3) no regressions — embedded mode still wires these from the local g; notifications
//       stay client-local (and explicitly do not pretend to control daemon notifications).
//   (4) holistic — the composition is exactly runAttached's handler assembly; no daemon
//       import leaks into ui/tui (this test lives in cmd, the only place allowed to call
//       installPresentationHandlers).

// seedClientConfigIssue507 writes a well-formed ~/.gogent/config.json under home with the
// given default model and returns its bytes for a later byte-identity check.
func seedClientConfigIssue507(t *testing.T, home, defaultModel string) []byte {
	t.Helper()
	cfg := config.GetDefaultConfig()
	cfg.DefaultModel = defaultModel
	if err := config.SaveConfig(home, cfg); err != nil {
		t.Fatalf("seed client config: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".gogent", "config.json"))
	if err != nil {
		t.Fatalf("read seeded client config: %v", err)
	}
	return b
}

// daemonWithModelsIssue507 builds a loopback /api daemon whose core knows the named
// models and starts on the given default. It returns the live daemon core (for direct
// assertion) and a credential-less HTTP client that is human-scoped over loopback.
func daemonWithModelsIssue507(t *testing.T, initialDefault string, models ...string) (*gogent.Gogent, *tuipkg.APIClient) {
	t.Helper()
	g := gogent.NewGogent(t.TempDir())
	for _, name := range models {
		// Routable config (api_type/endpoint set) so save-time validation
		// (issue #532) is satisfied — these tests exercise default-model resolution,
		// not config validation.
		if err := g.AddModel(config.ModelConfig{Name: name, APIType: "openai", Endpoint: "https://api.example.com/v1"}); err != nil {
			t.Fatalf("AddModel %s: %v", name, err)
		}
	}
	if initialDefault != "" {
		if err := g.SetDefaultModel(initialDefault); err != nil {
			t.Fatalf("SetDefaultModel %s: %v", initialDefault, err)
		}
	}
	srv := server.NewServer(g, server.Options{})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	client, err := tuipkg.NewAPIClient(httpSrv.URL, "")
	if err != nil {
		t.Fatalf("NewAPIClient: %v", err)
	}
	return g, client
}

// configDefaultModelIssue507 reads a home's config.json default_model field, returning
// "" if the file/field is absent. Used for the field-level (byte-normalization-robust)
// regression check.
func configDefaultModelIssue507(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".gogent", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw struct {
		DefaultModel string `json:"default_model"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return raw.DefaultModel
}

// TestAttachedDefaultModelIssue507ReadsDaemonNotClient: after composing rc.Handlers() +
// installPresentationHandlers, GetDefaultModel resolves to the DAEMON's default, NOT the
// client core's. (Before the fix it returned the client's, causing the index-0 fallback.)
func TestAttachedDefaultModelIssue507ReadsDaemonNotClient(t *testing.T) {
	daemonG, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model", "daemon-model-2")
	clientHome := t.TempDir()
	seedClientConfigIssue507(t, clientHome, "client-model")
	clientG := gogent.NewGogent(clientHome)

	if clientG.DefaultModelName() != "client-model" {
		t.Fatalf("setup: client default = %q, want client-model", clientG.DefaultModelName())
	}
	if clientG.DefaultModelName() == daemonG.DefaultModelName() {
		t.Fatalf("setup: client and daemon defaults must differ to make the split observable")
	}

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	// Must read the daemon's value; if installPresentationHandlers had re-wired it to
	// clientG this would return "client-model".
	if got := handlers.GetDefaultModel(); got != "daemon-model" {
		t.Fatalf("attached GetDefaultModel = %q, want daemon-model (daemon-owned); client wiring leaked", got)
	}
}

// TestAttachedDefaultModelIssue507DoesNotMutateClientConfig: the PRIMARY regression — an
// attached SetDefaultModel updates the DAEMON core and leaves the CLIENT's config.json
// byte-identical (and its default_model field unchanged). Before the fix it wrote the
// client's config via g.SetDefaultModel+SaveConfig.
func TestAttachedDefaultModelIssue507DoesNotMutateClientConfig(t *testing.T) {
	daemonG, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model", "daemon-model-2")
	clientHome := t.TempDir()
	seedBytes := seedClientConfigIssue507(t, clientHome, "client-model")
	clientG := gogent.NewGogent(clientHome)

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	if err := handlers.SetDefaultModel("daemon-model-2"); err != nil {
		t.Fatalf("SetDefaultModel: %v", err)
	}
	// The DAEMON core must have flipped.
	if got := daemonG.DefaultModelName(); got != "daemon-model-2" {
		t.Fatalf("daemon DefaultModelName = %q, want daemon-model-2 (Set as default must update the daemon)", got)
	}
	// The CLIENT config.json must be byte-identical.
	afterBytes, err := os.ReadFile(filepath.Join(clientHome, ".gogent", "config.json"))
	if err != nil {
		t.Fatalf("read client config after set: %v", err)
	}
	if string(afterBytes) != string(seedBytes) {
		t.Fatalf("client config.json was mutated by an attached SetDefaultModel:\n--- before ---\n%s\n--- after ---\n%s", seedBytes, afterBytes)
	}
	// Field-level guard, robust even if a future load normalizes unrelated bytes.
	if got := configDefaultModelIssue507(t, clientHome); got != "client-model" {
		t.Fatalf("client default_model field = %q, want client-model (daemon-owned field must not be written to the client)", got)
	}
}

// TestInstallPresentationHandlersIssue507DoesNotOverwriteDaemonDefaultModel: guards against a
// future re-introduction of the local-g wiring. installPresentationHandlers must leave the
// daemon-backed GetDefaultModel/SetDefaultModel set by rc.Handlers() untouched.
func TestInstallPresentationHandlersIssue507DoesNotOverwriteDaemonDefaultModel(t *testing.T) {
	daemonG, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model")
	// A client core whose own default differs from the daemon's, so a leaked client
	// closure would be observable.
	clientG := gogent.NewGogent(t.TempDir())
	if clientG.DefaultModelName() == daemonG.DefaultModelName() {
		t.Fatalf("setup: client (%q) and daemon (%q) defaults must differ", clientG.DefaultModelName(), daemonG.DefaultModelName())
	}

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	if handlers.GetDefaultModel == nil || handlers.SetDefaultModel == nil {
		t.Fatalf("rc.Handlers must populate the daemon-backed default-model handlers")
	}
	installPresentationHandlers(&handlers, clientG, wb, false)

	// Still the daemon-backed closure → resolves to the daemon's default, not clientG's.
	if got := handlers.GetDefaultModel(); got != daemonG.DefaultModelName() {
		t.Fatalf("installPresentationHandlers overwrote daemon GetDefaultModel: got %q, want %q", got, daemonG.DefaultModelName())
	}
}

// TestAttachedNotificationsIssue507AreClientLocal: notifications are CLIENT-owned by
// policy (the notifier hardware is local). GetNotifyConfig/SetNotifyConfig (wired by
// installPresentationHandlers) read/write the CLIENT core and deliberately do NOT touch
// the daemon's /api/settings/notifications (daemon-side fallback only).
func TestAttachedNotificationsIssue507AreClientLocal(t *testing.T) {
	daemonG, client := daemonWithModelsIssue507(t, "daemon-model", "daemon-model")
	clientHome := t.TempDir()
	seedClientConfigIssue507(t, clientHome, "client-model")
	clientG := gogent.NewGogent(clientHome)

	// Distinct client vs daemon notify configs so client-local vs synced is observable.
	clientNotify := config.NotifyConfig{Enabled: true, Bell: true, Desktop: false}
	daemonNotify := config.NotifyConfig{Enabled: false, Bell: false, Desktop: false}
	clientG.SetNotifications(clientNotify)
	daemonG.SetNotifications(daemonNotify)

	wb := tuipkg.NewWorkbench(nil)
	handlers := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb).Handlers()
	installPresentationHandlers(&handlers, clientG, wb, false)

	got := handlers.GetNotifyConfig()
	if got.Bell != clientNotify.Bell || got.Enabled != clientNotify.Enabled {
		t.Fatalf("attached GetNotifyConfig = %+v, want the CLIENT-LOCAL config %+v (not the daemon's %+v)", got, clientNotify, daemonNotify)
	}
}

// TestEmbeddedDefaultModelIssue507StillUsesLocalCore: embedded mode is unchanged —
// GetDefaultModel/SetDefaultModel are backed by the LOCAL g (there is no daemon). Only
// the attached path changed.
func TestEmbeddedDefaultModelIssue507StillUsesLocalCore(t *testing.T) {
	g := gogent.NewGogent(t.TempDir()) // built-in default "local-lan"
	wb := tuipkg.NewWorkbench(nil)
	h := embeddedHandlersFor(g, wb, false)

	if h.GetDefaultModel == nil || h.SetDefaultModel == nil {
		t.Fatalf("embedded handlers must wire GetDefaultModel/SetDefaultModel from the local g")
	}
	if got := h.GetDefaultModel(); got != g.DefaultModelName() {
		t.Fatalf("embedded GetDefaultModel = %q, want local core %q", got, g.DefaultModelName())
	}
	// SetDefaultModel must update the LOCAL core ("qemu-host" is a built-in model).
	if err := h.SetDefaultModel("qemu-host"); err != nil {
		t.Fatalf("embedded SetDefaultModel qemu-host: %v", err)
	}
	if got := g.DefaultModelName(); got != "qemu-host" {
		t.Fatalf("embedded SetDefaultModel did not update the local core; got %q", got)
	}
}
