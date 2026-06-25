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
	// After issue #462 the swatch LEADS the row (swatch → label → field), so it sits
	// swatchW+1 columns LEFT of the label text. Return its LEFT edge: it is inside the
	// swatch for activation, and issue366ClickPopupPaletteCell uses it as the picker
	// popup's origin (the popup opens at the picker's left edge). (Before #462 the swatch
	// was at label+gap+field+2gaps, i.e. the far right of the row.)
	x = col - themeEditorSwatchW - 1
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
	row, fieldX := issue366FieldOrigin(t, w, key)
	grid := editorGrid(w)
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

func issue366FieldOrigin(t *testing.T, w *Workbench, key string) (row, fieldX int) {
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
	return row, col + labelW + 1
}

func issue366SetRenderedField(t *testing.T, w *Workbench, key, value string) {
	t.Helper()
	row, fieldX := issue366FieldOrigin(t, w, key)
	top := w.desktop.TopLayer()
	if top == nil {
		t.Fatalf("theme editor layer is not open")
	}
	target := top.Root.HitTestDeep(fieldX, row)
	if target == nil {
		t.Fatalf("no component hit at field coordinate (%d,%d)", fieldX, row)
	}
	w.desktop.SetFocus(target)
	if !target.Focused() {
		t.Fatalf("could not focus field for role %q", key)
	}
	target.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: 'a', Ctrl: true})
	for _, r := range value {
		target.BubbleType(tui.TypeEvent{Key: tui.KeyRune, Rune: r})
	}
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

func issue366ClickPopupPaletteCell(popup *tv.VisualComponent, swatchX, swatchY, index, cols int) {
	const colorSwatchW = 2
	popupX := swatchX
	popupY := swatchY + 1
	gridX := popupX + 1
	gridY := popupY + 1
	x := gridX + (index%cols)*colorSwatchW
	y := gridY + index/cols
	issue366ClickComponent(popup, x, y)
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

func TestIssue366PopupOutsideClickCancelsWithoutChangingField(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevel16)
	w := issue366OpenEditor(t, config.ThemeConfig{
		Overrides: map[string]string{"user": "3"},
	}, nil)

	popup := issue366ActivateSwatchByClick(t, w, "user")
	popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnd})
	issue366ClickComponent(popup, 0, 0)

	if top := w.desktop.TopLayer(); top == nil || top.Name != "theme-editor" {
		t.Fatalf("outside click should close only the picker and return to theme editor; top layer=%v", top)
	}
	if got := issue366RenderedField(t, w, "user"); got != "3" {
		t.Fatalf("outside click changed the field: got %q, want original %q", got, "3")
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

func TestIssue366PopupMouseCommitANSIUpdatesFieldAndSavePath(t *testing.T) {
	withIssue366ColorLevel(t, tui.ColorLevel16)
	var saved config.ThemeConfig
	var saves int
	w := issue366OpenEditor(t, config.ThemeConfig{
		Overrides: map[string]string{"user": "3"},
	}, func(cfg config.ThemeConfig) {
		saved = cfg
		saves++
	})

	swatchX, swatchY, target := issue366RoleSwatch(t, w, "user")
	issue366ClickComponent(target, swatchX, swatchY)
	popup := w.desktop.TopLayer()
	if popup == nil || popup.Name != "color-picker-popup" {
		t.Fatalf("clicking swatch did not open color picker popup; top layer=%v", popup)
	}
	// Palette index 0 is terminal default; index 10 is ANSI 9 in the 8-column 16-colour grid.
	issue366ClickPopupPaletteCell(popup.Root, swatchX, swatchY, 10, 8)

	if got := issue366RenderedField(t, w, "user"); got != "9" {
		t.Fatalf("mouse picker commit wrote field %q, want %q", got, "9")
	}
	issue366ClickSave(t, w)
	if saves != 1 {
		t.Fatalf("Save callback count = %d, want 1", saves)
	}
	if got := saved.Overrides["user"]; got != "9" {
		t.Fatalf("saved user override = %q, want %q (mouse picker commit must feed normal buildThemeConfig path)", got, "9")
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

func TestIssue366CommittingCurrentSelectionCanonicalizesField(t *testing.T) {
	tests := []struct {
		name       string
		level      tui.ColorLevel
		field      string
		want       string
		wantSeeded string
	}{
		{
			name:  "ansi with leading zeros",
			level: tui.ColorLevel16,
			field: "003",
			want:  "3",
		},
		{
			name:  "default alias",
			level: tui.ColorLevel16,
			field: "none",
			want:  "default",
		},
		{
			name:  "invalid spec falls back to terminal default",
			level: tui.ColorLevel16,
			field: "bad",
			want:  "default",
		},
		{
			name:       "lowercase rgb hex",
			level:      tui.ColorLevelTrueColor,
			field:      "#abcdef",
			want:       "#ABCDEF",
			wantSeeded: "#abcdef",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withIssue366ColorLevel(t, tc.level)
			var saved config.ThemeConfig
			var saves int
			w := issue366OpenEditor(t, config.ThemeConfig{}, func(cfg config.ThemeConfig) {
				saved = cfg
				saves++
			})
			issue366SetRenderedField(t, w, "user", tc.field)
			if !strings.HasPrefix(tc.field, "#") && issue366RenderedField(t, w, "user") != tc.field {
				got := issue366RenderedField(t, w, "user")
				t.Fatalf("setup field = %q, want %q", got, tc.field)
			}

			popup := issue366ActivateSwatchByKey(t, w, "user", tui.TypeEvent{Key: tui.KeyEnter})
			if tc.wantSeeded != "" {
				w.desktop.Redraw()
				if screen := screenText(w); !containsOnScreen(screen, tc.wantSeeded) {
					t.Fatalf("picker did not seed from current field; preview lacked %q\n%s", tc.wantSeeded, screen)
				}
			}
			popup.BubbleType(tui.TypeEvent{Key: tui.KeyEnter})
			issue366ClickSave(t, w)

			if saves != 1 {
				t.Fatalf("Save callback count = %d, want 1", saves)
			}
			if got := saved.Overrides["user"]; got != tc.want {
				t.Fatalf("committing current picker selection saved user override %q, want canonical %q", got, tc.want)
			}
		})
	}
}
