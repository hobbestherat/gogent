package ui

import (
	"reflect"
	"strings"
	"testing"
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
	t.Run("unchecked close persists show true", func(t *testing.T) {
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
		if len(calls) != 1 || !calls[0] {
			t.Fatalf("SetShowWelcome calls = %v, want [true]", calls)
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
