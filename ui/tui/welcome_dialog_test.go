package ui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func findWelcomeCheckboxComponent(t *testing.T, w *Workbench) *tv.VisualComponent {
	t.Helper()
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil {
		t.Fatal("no top layer")
	}
	var focusables []*tv.VisualComponent
	var walk func(*tv.VisualComponent)
	walk = func(c *tv.VisualComponent) {
		if c.Focusable {
			focusables = append(focusables, c)
		}
		for _, child := range c.Children() {
			walk(child)
		}
	}
	walk(top.Root)
	for _, c := range focusables {
		if c.Mnemonic == 'd' {
			return c
		}
	}
	t.Fatalf("welcome dialog checkbox not found; focusable count=%d", len(focusables))
	return nil
}

func closeWelcomeWithEscape(t *testing.T, w *Workbench) {
	t.Helper()
	top := w.desktop.TopLayer()
	if top == nil || top.Root == nil || top.Root.OnTypeFn == nil {
		t.Fatal("welcome dialog root has no key handler")
	}
	if !top.Root.OnTypeFn(top.Root, tui.TypeEvent{Key: tui.KeyEscape}) {
		t.Fatal("welcome dialog did not consume Escape")
	}
}

func TestWelcomeDialogCheckboxPersistsOnEveryClosePath(t *testing.T) {
	t.Run("unchanged unchecked close persists nothing", func(t *testing.T) {
		w := newTestWorkbench(t)
		var calls []bool
		w.SetHandlers(Handlers{
			GetShowWelcome: func() bool { return true },
			SetShowWelcome: func(show bool) { calls = append(calls, show) },
		})
		w.showWelcomeDialog()
		if top := w.desktop.TopLayer(); top == nil || top.Name != "welcome-dialog" {
			t.Fatalf("top layer = %v, want welcome-dialog", top)
		}
		closeWelcomeWithEscape(t, w)
		if len(calls) != 0 {
			t.Fatalf("SetShowWelcome calls = %v, want none for unchanged unchecked close", calls)
		}
		if top := w.desktop.TopLayer(); top != nil && top.Name == "welcome-dialog" {
			t.Fatal("welcome dialog still open after Escape")
		}
	})

	t.Run("checked title close persists show false", func(t *testing.T) {
		w := newTestWorkbench(t)
		var calls []bool
		w.SetHandlers(Handlers{
			GetShowWelcome: func() bool { return true },
			SetShowWelcome: func(show bool) { calls = append(calls, show) },
		})
		w.showWelcomeDialog()
		cb := findWelcomeCheckboxComponent(t, w)
		if cb.OnTypeFn == nil || !cb.OnTypeFn(cb, tui.TypeEvent{Key: tui.KeyRune, Rune: ' '}) {
			t.Fatal("checkbox did not toggle on Space")
		}
		top := w.desktop.TopLayer()
		if top == nil || top.Root == nil {
			t.Fatal("welcome dialog missing before title close")
		}
		win := layerWindow(t, top)
		if win.OnClose == nil {
			t.Fatal("dialog window missing title-bar close handler")
		}
		win.OnClose(win)
		if len(calls) != 1 || calls[0] {
			t.Fatalf("SetShowWelcome calls = %v, want [false]", calls)
		}
	})

	t.Run("unchanged checked close persists nothing", func(t *testing.T) {
		w := newTestWorkbench(t)
		var calls []bool
		w.SetHandlers(Handlers{
			GetShowWelcome: func() bool { return false },
			SetShowWelcome: func(show bool) { calls = append(calls, show) },
		})
		w.showWelcomeDialog()
		closeWelcomeWithEscape(t, w)
		if len(calls) != 0 {
			t.Fatalf("SetShowWelcome calls = %v, want none for unchanged checked close", calls)
		}
	})
}

func TestWelcomeDialogReflectsCurrentPreferenceAndHandlesMissingHandlers(t *testing.T) {
	t.Run("disabled startup starts checked and can re-enable", func(t *testing.T) {
		w := newTestWorkbench(t)
		var calls []bool
		w.SetHandlers(Handlers{
			GetShowWelcome: func() bool { return false },
			SetShowWelcome: func(show bool) { calls = append(calls, show) },
		})
		w.showWelcomeDialog()
		cb := findWelcomeCheckboxComponent(t, w)
		// Current state is checked because startup display is disabled. Toggling once
		// unchecks it, so closing should persist ShowWelcome=true.
		if cb.OnTypeFn == nil || !cb.OnTypeFn(cb, tui.TypeEvent{Key: tui.KeyRune, Rune: ' '}) {
			t.Fatal("checkbox did not toggle on Space")
		}
		closeWelcomeWithEscape(t, w)
		if len(calls) != 1 || !calls[0] {
			t.Fatalf("SetShowWelcome calls = %v, want [true]", calls)
		}
	})

	t.Run("nil handlers do not panic or persist", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.showWelcomeDialog()
		closeWelcomeWithEscape(t, w)
		if top := w.desktop.TopLayer(); top != nil && top.Name == "welcome-dialog" {
			t.Fatal("welcome dialog still open with nil handlers")
		}
	})
}

func TestShowWelcomeCommandAndHelpText(t *testing.T) {
	w := newTestWorkbench(t)
	cmd, ok := findCommand(w.commands(), "Show welcome")
	if !ok {
		t.Fatal(`commands() missing "Show welcome"`)
	}
	if cmd.category != "App" {
		t.Fatalf("Show welcome category = %q, want App", cmd.category)
	}
	if !cmd.available() {
		t.Fatal("Show welcome should be available without backend handlers")
	}
	cmd.run()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "welcome-dialog" {
		t.Fatalf("top layer after Show welcome command = %v, want welcome-dialog", top)
	}

	text := helpText((&Workbench{}).commands())
	if !strings.Contains(text, "Show welcome") {
		t.Fatalf("helpText missing Show welcome\n%s", text)
	}
}

func TestHelpMenuWelcomeItemOpensDialog(t *testing.T) {
	w := newTestWorkbench(t)
	w.rebuildMenu()

	bar := desktopMenuBar(t, w)
	var welcome *tv.MenuItem
	for _, menu := range bar.Menus {
		if menu.Label != "&Help" {
			continue
		}
		for _, item := range menu.Children {
			if item.Label == "&Welcome…" {
				welcome = item
				break
			}
		}
	}
	if welcome == nil {
		t.Fatal(`Help menu missing "&Welcome…" item`)
	}
	if welcome.OnSelect == nil {
		t.Fatal("Help Welcome item has no action")
	}
	welcome.OnSelect()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "welcome-dialog" {
		t.Fatalf("top layer after Help Welcome = %v, want welcome-dialog", top)
	}
}

func TestWelcomeBodyListsExpectedOrientationItems(t *testing.T) {
	for _, want := range []string{
		"Ctrl+K",
		"Ctrl+N",
		"Ctrl+F",
		"?",
		"Ctrl+Q",
		"/plan",
		"/act",
		"/undo",
		"/rewind",
		"/stop",
		"/goal",
		"/thinking",
	} {
		if !strings.Contains(welcomeBody, want) {
			t.Errorf("welcomeBody missing %q\n%s", want, welcomeBody)
		}
	}
}

func TestWelcomeStartupGate(t *testing.T) {
	t.Run("shows after initial session when preference true", func(t *testing.T) {
		w := newTestWorkbench(t)
		w.SetHandlers(Handlers{GetShowWelcome: func() bool { return true }})
		done := runWorkbenchForTest(t, w)
		defer stopWorkbenchForTest(t, w, done)

		eventually(t, func() bool {
			return len(w.orderIDs()) == 1 && topLayerName(w) == "welcome-dialog"
		}, "initial session plus welcome dialog")
	})

	t.Run("skips when preference false", func(t *testing.T) {
		w := newTestWorkbench(t)
		checked := make(chan struct{}, 1)
		w.SetHandlers(Handlers{GetShowWelcome: func() bool {
			select {
			case checked <- struct{}{}:
			default:
			}
			return false
		}})
		done := runWorkbenchForTest(t, w)
		defer stopWorkbenchForTest(t, w, done)

		select {
		case <-checked:
		case <-time.After(time.Second):
			t.Fatal("startup did not query GetShowWelcome")
		}
		if got := len(w.orderIDs()); got != 1 {
			t.Fatalf("session count after startup preference check = %d, want 1", got)
		}
		if got := topLayerName(w); got == "welcome-dialog" {
			t.Fatal("welcome dialog opened despite false startup preference")
		}
	})

	t.Run("missing handler starts without panic and skips", func(t *testing.T) {
		w := newTestWorkbench(t)
		done := runWorkbenchForTest(t, w)
		defer stopWorkbenchForTest(t, w, done)

		eventually(t, func() bool { return len(w.orderIDs()) == 1 }, "initial session without welcome handler")
		if got := topLayerName(w); got == "welcome-dialog" {
			t.Fatal("welcome dialog opened with no GetShowWelcome handler")
		}
	})
}

func TestSettingsDialogExposesShowWelcomePreference(t *testing.T) {
	// Issue #339 requires the TUI settings surface to include ShowWelcome so the
	// startup dialog preference is not only reachable from the dialog itself.
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "settings_dialog.go"))
	if err != nil {
		t.Fatalf("read settings_dialog.go: %v", err)
	}
	text := string(src)
	for _, want := range []string{"GetShowWelcome", "SetShowWelcome"} {
		if !strings.Contains(text, want) {
			t.Fatalf("settings dialog does not reference %s; ShowWelcome is not exposed through settings", want)
		}
	}
}

func runWorkbenchForTest(t *testing.T, w *Workbench) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- w.Run()
	}()
	return done
}

func stopWorkbenchForTest(t *testing.T, w *Workbench, done chan error) {
	t.Helper()
	w.quit()
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "inappropriate ioctl for device") {
			t.Fatalf("Workbench.Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Workbench.Run did not stop after quit")
	}
}

func eventually(t *testing.T, ok func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func topLayerName(w *Workbench) string {
	if top := w.desktop.TopLayer(); top != nil {
		return top.Name
	}
	return ""
}

func desktopMenuBar(t *testing.T, w *Workbench) *tv.MenuBar {
	t.Helper()
	field := reflect.ValueOf(w.desktop).Elem().FieldByName("menuBar")
	if field.IsNil() {
		t.Fatal("desktop menuBar is nil")
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*tv.MenuBar)
}

func layerWindow(t *testing.T, layer *tv.Layer) *tv.Window {
	t.Helper()
	field := reflect.ValueOf(layer).Elem().FieldByName("window")
	if field.IsNil() {
		t.Fatal("layer has no window")
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*tv.Window)
}
