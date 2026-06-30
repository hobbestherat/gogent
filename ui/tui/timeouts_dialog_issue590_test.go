package ui

import (
	"strings"
	"testing"

	"gogent/internal/config"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// Issue #590 — the model/tool/sub-agent timeouts relocated from the Sub-agent
// Settings dialog into their own discoverable "Timeouts" dialog, reachable from
// the Config menu ("&Timeouts…") and the command palette ("Timeout settings"),
// both gated on the GetTimeouts/SetTimeouts handlers.
//
// These tests pin the UI half of the fix:
//   (1) GOAL MATCH — the Timeouts dialog opens (it exists and is reachable), and
//       the menu + palette entries are present when wired.
//   (2) USABILITY — the entries are gated exactly like Notifications/Theme, so a
//       daemon-unwired/headless build never shows a dead "Timeouts" affordance.
//   (3) NO REGRESSIONS — the new dialog honours the same #317 pinned-to-content
//       sizing policy as the Sub-agent dialog (never the 160×42 balloon).
//
// Hermetic: no terminal I/O beyond the in-process app.Resize; no network.

// menuItemTexts collects the visible labels of a settings menu slice.
func menuItemTexts(items []*tv.MenuItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it != nil && !it.Separator {
			out = append(out, it.Label)
		}
	}
	return out
}

func containsMenuItem(items []*tv.MenuItem, label string) bool {
	for _, t := range menuItemTexts(items) {
		if t == label {
			return true
		}
	}
	return false
}

// hasTimeoutAction reports whether the palette offers the "Timeout settings" command
// (it is gated out when GetTimeouts/SetTimeouts are unwired, so this respects gating).
func hasTimeoutAction(w *Workbench) bool {
	_, ok := findCommand(filterCommands(w.commands(), ""), "Timeout settings")
	return ok
}

// --- (3) Pinned-to-content sizing (issue #317 policy) -----------------------

func TestTimeoutsDialogPinnedToContent(t *testing.T) {
	open := func(cols, rows int) tv.Rect {
		w := newTestWorkbench(t)
		w.SetHandlers(settingsHandlers())
		w.app.Resize(cols, rows)
		w.showTimeoutsDialog()
		return dialogBounds(w)
	}

	// showTimeoutsDialog must open a layer at all (not bail to the "unavailable"
	// confirm, which would leave no dialog layer).
	t.Run("opens a pinned dialog", func(t *testing.T) {
		b := open(200, 50)
		if b == (tv.Rect{}) {
			t.Fatal("showTimeoutsDialog opened no dialog layer (handler guard fired unexpectedly)")
		}
		// Spec is MinW48/MaxW64/PreferredW60, MinH11/MaxH11. Height is pinned at 11
		// (MaxH==MinH), never the 85% vertical balloon.
		if b.H != 11 {
			t.Errorf("timeouts height = %d, want pinned 11 (not the 85%% balloon)", b.H)
		}
		if b.W < 48 || b.W > 64 {
			t.Errorf("timeouts width = %d, want within [48,64]", b.W)
		}
		if b.W != 60 {
			t.Errorf("timeouts width = %d, want preferred 60 on a roomy terminal", b.W)
		}
	})

	t.Run("never balloons even on an ultrawide terminal", func(t *testing.T) {
		b := open(300, 80)
		if b.W != 60 || b.H != 11 {
			t.Errorf("timeouts on 300x80 = %dx%d, want capped 60x11 (MaxW/MaxH must hold)", b.W, b.H)
		}
	})

	t.Run("floors on a tiny terminal", func(t *testing.T) {
		b := open(40, 16)
		if b.H != 11 {
			t.Errorf("timeouts height on 40x16 = %d, want 11", b.H)
		}
		if b.W < 48 {
			t.Errorf("timeouts width on 40x16 = %d, want >= MinW 48", b.W)
		}
	})
}

// TestTimeoutsDialogGuardShowsUnavailableWhenUnwired pins the guard contract: even
// if the entry were reached with the handlers unwired, the dialog degrades to the
// "unavailable" confirm rather than nil-dereferencing SetTimeout/GetTimeouts.
func TestTimeoutsDialogGuardShowsUnavailableWhenUnwired(t *testing.T) {
	w := newTestWorkbench(t)
	// Deliberately wire Sub-agent settings but NOT the timeout handlers.
	w.SetHandlers(Handlers{
		GetSettings: func() config.SubAgentConfig { return config.DefaultSubAgentConfig() },
		SetSettings: func(config.SubAgentConfig) {},
	})
	w.app.Resize(120, 40)
	w.showTimeoutsDialog()
	// The guard opens a confirm layer (not the timeouts form), so the top layer is a
	// confirm, not the "timeouts-dialog".
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatal("guard opened no layer at all")
	}
	if top.Name == "timeouts-dialog" {
		t.Fatalf("timeouts dialog opened despite unwired handlers (top layer = %q); guard should show the unavailable confirm", top.Name)
	}
}

// --- (1)+(2) Discoverability + gating ---------------------------------------

func TestTimeoutsMenuItemPresentWhenWired(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(settingsHandlers())
	w.rebuildMenu()
	if !containsMenuItem(w.settingsItems(), "&Timeouts…") {
		t.Errorf("Config menu missing '&Timeouts…' entry when timeout handlers are wired: %v", menuItemTexts(w.settingsItems()))
	}
}

func TestTimeoutsMenuItemAbsentWhenTimeoutHandlersUnwired(t *testing.T) {
	w := newTestWorkbench(t)
	// Settings wired (so settingsItems doesn't early-return) but timeouts NOT.
	w.SetHandlers(Handlers{
		GetSettings: func() config.SubAgentConfig { return config.DefaultSubAgentConfig() },
		SetSettings: func(config.SubAgentConfig) {},
	})
	w.rebuildMenu()
	if containsMenuItem(w.settingsItems(), "&Timeouts…") {
		t.Errorf("Config menu shows '&Timeouts…' despite unwired GetTimeouts/SetTimeouts (must be gated): %v", menuItemTexts(w.settingsItems()))
	}
}

func TestTimeoutsPaletteEntryPresentWhenWired(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(settingsHandlers())
	if !hasTimeoutAction(w) {
		t.Errorf("palette missing 'Timeout settings' when timeout handlers are wired: %v", commandNames(filterCommands(w.commands(), "")))
	}
}

func TestTimeoutsPaletteEntryAbsentWhenTimeoutHandlersUnwired(t *testing.T) {
	// A bare workbench (no handlers at all) must not offer the command.
	if hasTimeoutAction(&Workbench{}) {
		t.Errorf("palette offers 'Timeout settings' on an unwired workbench (must be gated)")
	}
}

// TestTimeoutsEntriesArePeersOfModelsNotSubagentOwned documents the discoverability
// win: "Timeout settings" is a top-level Config command (a peer of Models), found by
// a user searching "timeout" — not buried under the Sub-agent entry. It also confirms
// the palette groups it under "Config".
func TestTimeoutsEntriesArePeersOfModelsNotSubagentOwned(t *testing.T) {
	w := newTestWorkbench(t)
	w.SetHandlers(settingsHandlers())
	cmd, ok := findCommand(w.commands(), "Timeout settings")
	if !ok {
		t.Fatal("palette missing 'Timeout settings'")
	}
	if cmd.category != "Config" {
		t.Errorf("'Timeout settings' category = %q, want Config", cmd.category)
	}
	if cmd.run == nil {
		t.Error("'Timeout settings' has no run handler")
	}
	// Help/cheatsheet visibility mirrors palette availability (visible() == available
	// minus the run check), so a user browsing '?' also finds it.
	if !strings.Contains(helpText(w.commands()), "Timeout settings") {
		t.Error("help overlay (cheatsheet) does not list 'Timeout settings'")
	}
}
