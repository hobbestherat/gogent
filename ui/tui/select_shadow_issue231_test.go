package ui

import (
	"testing"

	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file tests issue #231: the "Disable shadows" setting (#215) did not reach
// Select dropdown popups — turbotui's Select always drew a popup shadow. turbotui
// now exposes Select.Shadow; gogent seeds it from the active NoShadow preference
// at construction (the newSelect wrapper) and on the live theme-apply path
// (reseedSelect), exactly as buttons/windows/menus already do. The assertions
// pair every "NoShadow clears it" case with a "default keeps it" counterpart, and
// reuse issue215RestoreTheme to keep shadowsEnabled hermetic across tests.

// newTestSelect builds a Select through a workbench desktop so newSelect's seeding
// runs against a real turbotui desktop, the way gogent constructs selectors.
func newTestSelect(t *testing.T) *tv.Select {
	t.Helper()
	w := newTestWorkbench(t)
	return newSelect(w.desktop, []string{"a", "b"}, tv.Rect{X: 0, Y: 0, W: 10, H: 1})
}

// TestIssue231NewSelectHonoursNoShadow verifies the newSelect wrapper seeds the
// popup shadow from the active NoShadow preference at construction time.
func TestIssue231NewSelectHonoursNoShadow(t *testing.T) {
	t.Run("NoShadow clears the Select popup shadow", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(noShadowTheme())
		if newTestSelect(t).Shadow {
			t.Errorf("Select.Shadow = true, want false under NoShadow")
		}
	})

	t.Run("default keeps the Select popup shadow", func(t *testing.T) {
		issue215RestoreTheme(t)
		ApplyTheme(defaultShadowTheme())
		if !newTestSelect(t).Shadow {
			t.Errorf("Select.Shadow = false, want true by default")
		}
	})
}

// TestIssue231ReseedSelectReappliesShadowToggle verifies the live theme-apply path
// (reseedSelect, called from the #204 refresh chain) re-applies the NoShadow
// toggle to an already-built selector, so flipping the setting takes effect without
// a restart — both directions.
func TestIssue231ReseedSelectReappliesShadowToggle(t *testing.T) {
	issue215RestoreTheme(t)
	ApplyTheme(defaultShadowTheme())
	s := newTestSelect(t)
	if !s.Shadow {
		t.Fatalf("setup: Select.Shadow = false under default, want true")
	}

	// Toggling shadows off and re-skinning must clear the popup shadow.
	ApplyTheme(noShadowTheme())
	reseedSelect(s, tv.DefaultTheme)
	if s.Shadow {
		t.Errorf("after reseed under NoShadow, Select.Shadow = true, want false")
	}

	// Toggling back on and re-skinning must restore it.
	ApplyTheme(defaultShadowTheme())
	reseedSelect(s, tv.DefaultTheme)
	if !s.Shadow {
		t.Errorf("after reseed under default, Select.Shadow = false, want true")
	}
}

// TestIssue231ApplySelectShadowNilSafe guards the helper against a nil selector,
// matching applyButtonShadow/applyMenuBarShadow.
func TestIssue231ApplySelectShadowNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("applySelectShadow(nil) panicked: %v", r)
		}
	}()
	applySelectShadow(nil)
	reseedSelect(nil, tv.DefaultTheme)
}
