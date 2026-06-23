package ui

import (
	"strings"
	"testing"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file exercises gogent issue #366: the theme editor's live swatch is now an
// interactive turbotui ColorPicker, but the TextBox remains the source of truth for
// save/reset/cancel. The tests drive the real editor widget tree and popup layer.

func withIssue366ColorLevel(t *testing.T, level tui.ColorLevel) {
	t.Helper()
	old := tui.GetColorLevel()
	tui.SetColorLevel(level)
	t.Cleanup(func() { tui.SetColorLevel(old) })
}

func issue366OpenEditor(t *testing.T, cfg config.ThemeConfig, onSet func(config.ThemeConfig)) *Workbench {
	t.Helper()
	issue204RestoreTheme(t)
	w := newTestWorkbench(t)
	w.SetHandlers(Handlers{
		GetTheme: func() config.ThemeConfig { return cfg },
		SetTheme: func(next config.ThemeConfig) {
			if onSet != nil {
				onSet(next)
			}
		},
	})
	w.showThemeEditor()
	w.desktop.Redraw()
	return w
}

func issue366RoleSwatch(t *testing.T, w *Workbench, key string) (x, y int, target *tv.VisualComponent) {
	t.Helper()
	label := issue243WantLabels[key] + ":"
	if !scrollEditorToReveal(w, label) {
		t.Fatalf("role %q label %q not visible at any editor scroll offset", key, label)
	}
	grid := editorGrid(w)
	row, col, ok := findRunes(grid, label)
	if !ok {
		t.Fatalf("role %q label vanished after scrolling into view", key)
	}
	labelW := themeEditorLeftLabelW
	if issue267labelColumn(col) == "right" {
		labelW = themeEditorLabelW
	}
	x = col + labelW + themeEditorFieldW + 2
	y = row
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatalf("theme editor layer is not open")
	}
	target = top.Root.HitTestDeep(x, y)
	if target == nil {
		t.Fatalf("no component hit at swatch coordinate (%d,%d)", x, y)
	}
	if !target.Focusable {
		t.Fatalf("swatch for role %q is not focusable; hit component bounds=%+v", key, target.Bounds)
	}
	return x, y, target
}

func issue366RenderedField(t *testing.T, w *Workbench, key string) string {
	t.Helper()
	label := issue243WantLabels[key] + ":"
	if !scrollEditorToReveal(w, label) {
		t.Fatalf("role %q label %q not visible at any editor scroll offset", key, label)
	}
	grid := editorGrid(w)
	row, col, ok := findRunes(grid, label)
	if !ok {
		t.Fatalf("role %q label vanished after scrolling into view", key)
	}
	labelW := themeEditorLeftLabelW
	if issue267labelColumn(col) == "right" {
		labelW = themeEditorLabelW
	}
	fieldX := col + labelW + 1
	var b strings.Builder
	for x := fieldX; x < fieldX+themeEditorFieldW && x < len(grid[row]); x++ {
		ch := grid[row][x]
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return strings.TrimSpace(b.String())
}

func issue366ClickComponent(c *tv.VisualComponent, x, y int) {
	c.BubbleClick(tui.ClickEvent{X: x, Y: y, Button: tui.MouseLeft, Down: true})
	c.BubbleClick(tui.ClickEvent{X: x, Y: y, Button: tui.MouseLeft, Down: false})
}

func issue366ActivateSwatchByClick(t *testing.T, w *Workbench, key string) *tv.VisualComponent {
	t.Helper()
	x, y, target := issue366RoleSwatch(t, w, key)
	issue366ClickComponent(target, x, y)
	top := w.desktop.TopLayer()
	if top == nil || top.Name != "color-picker-popup" {
		t.Fatalf("clicking swatch for role %q did not open color picker popup; top layer=%v", key, top)
	}
	return top.Root
}

func issue366ActivateSwatchByKey(t *testing.T, w *Workbench, key string, ev tui.TypeEvent) *tv.VisualComponent {
	t.Helper()
	_, _, target := issue366RoleSwatch(t, w, key)
	w.desktop.SetFocus(target)
	if !target.Focused() {
		t.Fatalf("could not focus swatch for role %q", key)
	}
	if !target.BubbleType(ev) {
		t.Fatalf("activation key was not handled by swatch for role %q", key)
	}
	top := w.desktop.TopLayer()
	if top == nil || top.Name != "color-picker-popup" {
		t.Fatalf("keyboard activation for role %q did not open color picker popup; top layer=%v", key, top)
	}
	return top.Root
}

func issue366ClickSave(t *testing.T, w *Workbench) {
	t.Helper()
	grid := editorGrid(w)
	row, col, ok := findRunes(grid, "Save")
	if !ok {
		t.Fatalf("Save button not visible")
	}
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatalf("no layer open while trying to save")
	}
	target := top.Root.HitTestDeep(col, row)
	if target == nil {
		t.Fatalf("no component hit at Save coordinate (%d,%d)", col, row)
	}
	issue366ClickComponent(target, col, row)
}

func TestIssue366SwatchClickOpensPickerAndEscapeCancels(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevel16)
	w := issue366OpenEditor(t, config.ThemeConfig{
		Overrides: map[string]string{"user": "3"},
	}, nil)

	popup := issue366ActivateSwatchByClick(t, w, "user")
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnd})
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEscape})

	if top := w.desktop.TopLayer(); top == nil || top.Name != "theme-editor" {
		t.Fatalf("Escape should close only the picker and return to theme editor; top layer=%v", top)
	}
	if got := issue366RenderedField(t, w, "user"); got != "3" {
		t.Fatalf("Escape changed the field: got %q, want original %q", got, "3")
	}
}

func TestIssue366SwatchKeyboardCommitANSIUpdatesFieldAndSavePath(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevel16)
	var saved config.ThemeConfig
	var saves int
	w := issue366OpenEditor(t, config.ThemeConfig{
		Overrides: map[string]string{"user": "3"},
	}, func(cfg config.ThemeConfig) {
		saved = cfg
		saves++
	})

	popup := issue366ActivateSwatchByKey(t, w, "user", tui.TypeEvent{Key: tui.KeyEnter})
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnd})
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})

	if got := issue366RenderedField(t, w, "user"); got != "15" {
		t.Fatalf("ANSI picker commit wrote field %q, want %q", got, "15")
	}
	issue366ClickSave(t, w)
	if saves != 1 {
		t.Fatalf("Save callback count = %d, want 1", saves)
	}
	if got := saved.Overrides["user"]; got != "15" {
		t.Fatalf("saved user override = %q, want %q (picker must feed normal buildThemeConfig path)", got, "15")
	}
}

func TestIssue366SwatchSpaceCommitDefaultSpec(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevel16)
	w := issue366OpenEditor(t, config.ThemeConfig{
		Overrides: map[string]string{"user": "3"},
	}, nil)

	popup := issue366ActivateSwatchByKey(t, w, "user", tui.TypeEvent{Key: tui.KeyRune, Rune: ' '})
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyHome})
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})

	if got := issue366RenderedField(t, w, "user"); got != "default" {
		t.Fatalf("default picker commit wrote field %q, want %q", got, "default")
	}
}

func TestIssue366PickerSeedsFromCurrentRGBFieldAndCommitsRGBSpec(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevelTrueColor)
	var saved config.ThemeConfig
	w := issue366OpenEditor(t, config.ThemeConfig{
		Overrides: map[string]string{"user": "#010203"},
	}, func(cfg config.ThemeConfig) { saved = cfg })

	popup := issue366ActivateSwatchByKey(t, w, "user", tui.TypeEvent{Key: tui.KeyEnter})
	w.desktop.Redraw()
	if screen := screenText(w); !containsOnScreen(screen, "#010203") {
		t.Fatalf("picker did not seed from current RGB field; preview lacked #010203\n%s", screen)
	}

	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnd}) // R: 1 -> 255
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})

	if got := issue366RenderedField(t, w, "user"); got != "#FF0203" {
		t.Fatalf("RGB picker commit wrote field %q, want %q", got, "#FF0203")
	}
	issue366ClickSave(t, w)
	if got := saved.Overrides["user"]; got != "#FF0203" {
		t.Fatalf("saved user override = %q, want %q", got, "#FF0203")
	}
}

func TestIssue366NoColorSwatchDoesNotOpenPicker(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevel16)
	w := issue366OpenEditor(t, config.ThemeConfig{
		NoColor:   true,
		Overrides: map[string]string{"user": "3"},
	}, nil)

	x, y, target := issue366RoleSwatch(t, w, "user")
	issue366ClickComponent(target, x, y)
	if top := w.desktop.TopLayer(); top == nil || top.Name != "theme-editor" {
		t.Fatalf("NO_COLOR swatch click should not open picker; top layer=%v", top)
	}
	w.desktop.SetFocus(target)
	target.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})
	if top := w.desktop.TopLayer(); top == nil || top.Name != "theme-editor" {
		t.Fatalf("NO_COLOR swatch Enter should not open picker; top layer=%v", top)
	}
	if got := issue366RenderedField(t, w, "user"); got != "3" {
		t.Fatalf("NO_COLOR activation changed the field: got %q, want %q", got, "3")
	}
}
