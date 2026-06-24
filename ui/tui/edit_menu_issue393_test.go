package ui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func issue393MenuBar(t *testing.T, w *Workbench) *tv.MenuBar {
	t.Helper()
	v := reflect.ValueOf(w.desktop).Elem().FieldByName("menuBar")
	if !v.IsValid() {
		t.Fatal("turbotui Desktop no longer has a menuBar field; update this test helper")
	}
	if v.IsNil() {
		t.Fatal("workbench desktop has no menu bar installed")
	}
	return (*tv.MenuBar)(unsafe.Pointer(v.Pointer()))
}

func issue393ItemByLabel(items []*tv.MenuItem, label string) *tv.MenuItem {
	want := stripMnemonic(label)
	for _, it := range items {
		if it.Separator {
			continue
		}
		if stripMnemonic(it.Label) == want {
			return it
		}
	}
	return nil
}

func issue393AssertShortcut(t *testing.T, item *tv.MenuItem, display string, r rune) {
	t.Helper()
	if item.Shortcut == nil {
		t.Fatalf("%q has no shortcut", item.Label)
	}
	if item.Shortcut.Display != display {
		t.Fatalf("%q shortcut display = %q, want %q", item.Label, item.Shortcut.Display, display)
	}
	if item.Shortcut.Key != tui.KeyRune || item.Shortcut.Rune != r || !item.Shortcut.Ctrl ||
		item.Shortcut.Shift || item.Shortcut.Alt {
		t.Fatalf("%q shortcut = %+v, want Ctrl+%c rune shortcut only", item.Label, *item.Shortcut, r)
	}
}

func TestIssue393RebuildMenuInstallsEditMenuBetweenFileAndSession(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "Session")
	w.rebuildMenu()

	bar := issue393MenuBar(t, w)
	if len(bar.Menus) < 6 {
		t.Fatalf("top-level menus = %d, want at least File/Edit/Session/View/Config/Help", len(bar.Menus))
	}
	wantTop := []string{"&File", "&Edit", "&Session", "&View", "&Config"}
	for i, want := range wantTop {
		if got := bar.Menus[i].Label; got != want {
			t.Fatalf("top-level menu %d = %q, want %q; menus=%v", i, got, want, issue393MenuLabels(bar.Menus))
		}
	}

	edit := bar.Menus[1]
	if got := len(edit.Children); got != 5 {
		t.Fatalf("Edit menu item count = %d, want 5", got)
	}
	wantLabels := []string{"&Copy", "Cu&t", "&Paste", "----------", "&Find…"}
	for i, want := range wantLabels {
		item := edit.Children[i]
		if want == "----------" {
			if !item.Separator && item.Label != want {
				t.Fatalf("Edit item %d = label %q separator=%v, want separator", i, item.Label, item.Separator)
			}
			if item.OnSelect != nil {
				t.Fatalf("Edit separator has OnSelect wired")
			}
			continue
		}
		if item.Label != want {
			t.Fatalf("Edit item %d label = %q, want %q", i, item.Label, want)
		}
		if item.OnSelect == nil {
			t.Fatalf("Edit item %q has no OnSelect", want)
		}
	}
	issue393AssertShortcut(t, edit.Children[0], "Ctrl+C", 'c')
	issue393AssertShortcut(t, edit.Children[1], "Ctrl+X", 'x')
	issue393AssertShortcut(t, edit.Children[2], "Ctrl+V", 'v')
	issue393AssertShortcut(t, edit.Children[4], "Ctrl+F", 'f')
}

func issue393MenuLabels(items []*tv.MenuItem) []string {
	labels := make([]string, 0, len(items))
	for _, item := range items {
		labels = append(labels, item.Label)
	}
	return labels
}

func TestIssue393EditMenuAcceleratorsAreRegisteredByRebuildMenu(t *testing.T) {
	w := newTestWorkbench(t)
	w.rebuildMenu()

	reg := w.desktop.Bindings()
	if reg == nil {
		t.Fatal("menu bindings registry is nil after rebuildMenu")
	}
	for _, tt := range []struct {
		name string
		ev   tui.TypeEvent
	}{
		{"Copy", tui.TypeEvent{Key: tui.KeyRune, Rune: 'c', Ctrl: true}},
		{"Cut", tui.TypeEvent{Key: tui.KeyRune, Rune: 'x', Ctrl: true}},
		{"Paste", tui.TypeEvent{Key: tui.KeyRune, Rune: 'v', Ctrl: true}},
		{"Find", tui.TypeEvent{Key: tui.KeyRune, Rune: 'f', Ctrl: true}},
	} {
		if _, ok := reg.Match(tt.ev); !ok {
			t.Fatalf("%s accelerator %+v is not registered in rebuilt menu", tt.name, tt.ev)
		}
	}
}

func TestIssue393CopyCutAcceleratorsDoNotSwallowUnconsumedCtrlCOrCtrlX(t *testing.T) {
	w := newTestWorkbench(t)
	w.rebuildMenu()
	w.desktop.SetFocus(nil)

	bar := issue393MenuBar(t, w)
	for _, tt := range []struct {
		name string
		ev   tui.TypeEvent
	}{
		{"Copy", tui.TypeEvent{Key: tui.KeyRune, Rune: 'c', Ctrl: true}},
		{"Cut", tui.TypeEvent{Key: tui.KeyRune, Rune: 'x', Ctrl: true}},
	} {
		if bar.HandleAccelerator(tt.ev) {
			t.Fatalf("%s accelerator was consumed with no focus; Ctrl+C/Ctrl+X must fall through when copy/cut do not handle", tt.name)
		}
	}
}

func TestIssue393EditClipboardItemsInvokeFocusedDesktopPaths(t *testing.T) {
	w := newTestWorkbench(t)
	field := tv.NewComponent(tv.Rect{X: 0, Y: 0, W: 10, H: 1})
	field.Focusable = true

	var copies, cuts, pastes int
	var pasted string
	field.CopyFn = func(*tv.VisualComponent) (string, bool) {
		copies++
		return "copied text", true
	}
	field.CutFn = func(*tv.VisualComponent) (string, bool) {
		cuts++
		return "cut text", true
	}
	field.OnPasteFn = func(_ *tv.VisualComponent, text string) bool {
		pastes++
		pasted = text
		return true
	}
	w.desktop.SetFocus(field)
	t.Setenv("PATH", issue393ClipboardPath(t, "paste from fake clipboard"))

	items := w.editItems()
	issue393ItemByLabel(items, "Copy").OnSelect()
	issue393ItemByLabel(items, "Cut").OnSelect()
	issue393ItemByLabel(items, "Paste").OnSelect()

	if copies != 1 {
		t.Fatalf("Edit->Copy invoked CopyFn %d times, want 1", copies)
	}
	if cuts != 1 {
		t.Fatalf("Edit->Cut invoked CutFn %d times, want 1", cuts)
	}
	if pastes != 1 || pasted != "paste from fake clipboard" {
		t.Fatalf("Edit->Paste delivered (%d, %q), want (1, fake clipboard text)", pastes, pasted)
	}
}

func TestIssue393EditClipboardItemsAreGracefulWithNoFocus(t *testing.T) {
	w := newTestWorkbench(t)
	w.desktop.SetFocus(nil)
	t.Setenv("PATH", issue393ClipboardPath(t, "unused"))

	for _, label := range []string{"Copy", "Cut", "Paste"} {
		item := issue393ItemByLabel(w.editItems(), label)
		if item == nil || item.OnSelect == nil {
			t.Fatalf("Edit->%s missing or unwired", label)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Edit->%s panicked with no focus: %v", label, r)
				}
			}()
			item.OnSelect()
		}()
	}
}

func TestIssue393EditFindOpensActiveTranscriptPrompt(t *testing.T) {
	w := newTestWorkbench(t)
	w.openWindow("s", "Session")

	find := issue393ItemByLabel(w.editItems(), "Find…")
	if find == nil || find.OnSelect == nil {
		t.Fatal("Edit->Find missing or unwired")
	}
	find.OnSelect()

	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatal("Edit->Find did not open a dialog")
	}
	if top.Name != "input-dialog" {
		t.Fatalf("top layer after Edit->Find = %q, want input-dialog", top.Name)
	}
	box := inputDialogBox(t, w)
	for _, r := range "Needle" {
		typeDlgRune(box, r)
	}
	submitDlg(box)
	if got := w.sessions["s"].transcript.query; got != "needle" {
		t.Fatalf("Edit->Find submitted query = %q, want %q", got, "needle")
	}
}

func TestIssue393FindMovedOutOfViewMenu(t *testing.T) {
	w := newTestWorkbench(t)
	for _, item := range w.viewItems() {
		if item.Separator {
			continue
		}
		if strings.Contains(stripMnemonic(item.Label), "Find") {
			t.Fatalf("View menu still contains Find item: %q", item.Label)
		}
		if item.Shortcut != nil && item.Shortcut.Display == "Ctrl+F" {
			t.Fatalf("View menu item %q still owns Ctrl+F", item.Label)
		}
	}
}

func issue393ClipboardPath(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "pbpaste")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$ISSUE393_CLIPBOARD\"\n"), 0o755); err != nil {
		t.Fatalf("write fake pbpaste: %v", err)
	}
	t.Setenv("ISSUE393_CLIPBOARD", text)
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}
