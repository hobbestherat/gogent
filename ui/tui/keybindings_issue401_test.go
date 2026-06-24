package ui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"gogent/internal/config"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func issue401MenuBar(t *testing.T, w *Workbench) *tv.MenuBar {
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

func issue401FindMenuItem(items []*tv.MenuItem, id tv.ActionID) *tv.MenuItem {
	for _, it := range items {
		if it.ActionID == id {
			return it
		}
		if found := issue401FindMenuItem(it.Children, id); found != nil {
			return found
		}
	}
	return nil
}

func issue401FindMenuLabel(items []*tv.MenuItem, label string) *tv.MenuItem {
	want := stripMnemonic(label)
	for _, it := range items {
		if !it.Separator && stripMnemonic(it.Label) == want {
			return it
		}
		if found := issue401FindMenuLabel(it.Children, label); found != nil {
			return found
		}
	}
	return nil
}

func issue401Event(c tv.Chord) tui.TypeEvent {
	ev := tui.TypeEvent{Ctrl: c.Ctrl, Shift: c.Shift, Alt: c.Alt}
	if c.Rune != 0 {
		ev.Key = tui.KeyRune
		ev.Rune = c.Rune
		return ev
	}
	ev.Key = c.Key
	return ev
}

func TestIssue401SourceOnlyDefinesUnifiedCatalog(t *testing.T) {
	for _, path := range []string{"ui/tui/command_palette.go", "ui/tui/keybindings.go"} {
		b, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		for _, forbidden := range []string{"func commands(", "func (w *Workbench) commands(", "func keybindActions("} {
			if strings.Contains(src, forbidden) {
				t.Fatalf("%s still defines %q; #401 requires actions() as the single catalog", path, forbidden)
			}
		}
	}
}

func TestIssue401CatalogCoversAllRebindableGlobals(t *testing.T) {
	w := newTestWorkbench(t)
	want := map[tv.ActionID]tv.Chord{
		actionSessionNew:           {Rune: 'n', Ctrl: true},
		actionSessionNext:          {Rune: ']', Ctrl: true},
		actionSessionClose:         {Rune: 'w', Ctrl: true},
		actionAppQuit:              {Rune: 'q', Ctrl: true},
		actionConfigSubagents:      {Rune: ',', Ctrl: true},
		actionWindowTileVertical:   {Rune: 'v', Ctrl: true, Shift: true},
		actionWindowTileHorizontal: {Rune: 'h', Ctrl: true, Shift: true},
		actionWindowTileGrid:       {Rune: 'g', Ctrl: true, Shift: true},
		actionWindowMaximizeAll:    {Rune: 'm', Ctrl: true, Shift: true},
		actionWindowCascade:        {Rune: 'd', Ctrl: true, Shift: true},
	}
	for id, chord := range want {
		a, ok := w.actionByID(id)
		if !ok {
			t.Fatalf("missing global action %q from actions() catalog", id)
		}
		if a.scope != tv.ScopeGlobal {
			t.Fatalf("%q scope = %v, want Global", id, a.scope)
		}
		if !sameChord(a.deflt, chord) {
			t.Fatalf("%q default = %+v, want %+v", id, a.deflt, chord)
		}
		if a.run == nil {
			t.Fatalf("%q has nil run; menu and registry must share a runnable closure", id)
		}
		if ok, reason := validateScopeRule(a.scope, a.deflt); !ok {
			t.Fatalf("%q default rejected by scope rule: %s", id, reason)
		}
		if b, ok := w.desktop.Bindings().BindingFor(id); !ok {
			t.Fatalf("%q not registered in desktop's unified registry", id)
		} else if b.Scope != tv.ScopeGlobal || !sameChord(b.Chord, chord) {
			t.Fatalf("%q live binding = %+v, want Global %+v", id, b, chord)
		}
	}
}

func TestIssue401DesktopBindingsAndScopedBindingsAreSameRegistry(t *testing.T) {
	w := newTestWorkbench(t)
	if w.desktop.Bindings() != w.desktop.ScopedBindings() {
		t.Fatal("Desktop.Bindings and Desktop.ScopedBindings are not the same unified registry")
	}
	w.openWindow("s", "S")
	reg := w.desktop.Bindings()
	if _, ok := reg.Match(tui.TypeEvent{Key: tui.KeyRune, Rune: 'n', Ctrl: true}); !ok {
		t.Fatal("Global Ctrl+N does not match through the unified registry")
	}
	if _, ok := reg.MatchFallthrough(runeEv('?')); !ok {
		t.Fatal("Fallthrough '?' does not match through the unified registry")
	}
	if _, ok := reg.MatchFocus(runeEv('a'), w.sessions["s"].history.Component); !ok {
		t.Fatal("Focus transcript 'a' does not match through the unified registry")
	}
}

func TestIssue401GlobalRebindDispatchPersistsThroughMenuRebuild(t *testing.T) {
	w := newTestWorkbench(t)
	var persisted config.KeybindingsConfig
	w.handlers.SetKeybindings = func(k config.KeybindingsConfig) { persisted = k }
	old := tui.TypeEvent{Key: tui.KeyRune, Rune: 'n', Ctrl: true}
	rebound := tv.Chord{Key: tui.KeyF5}

	w.applyBinding(actionSessionNew, rebound)
	w.persistKeybindings()
	before := len(w.orderIDs())
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF5}) {
		t.Fatal("rebound session.new F5 did not dispatch as a Global binding")
	}
	if got := len(w.orderIDs()); got != before+1 {
		t.Fatalf("session count after F5 = %d, want %d", got, before+1)
	}
	if w.desktop.Bindings().Dispatch(old) {
		t.Fatal("old Ctrl+N binding still dispatches after session.new was rebound")
	}
	if got := persisted.Overrides[string(actionSessionNew)]; got != "F5" {
		t.Fatalf("persisted session.new override = %q, want F5 in %+v", got, persisted)
	}

	w.rebuildMenu()
	before = len(w.orderIDs())
	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF5}) {
		t.Fatal("rebound session.new F5 stopped dispatching after rebuildMenu")
	}
	if got := len(w.orderIDs()); got != before+1 {
		t.Fatalf("session count after rebuildMenu+F5 = %d, want %d", got, before+1)
	}
	if item := issue401FindMenuItem(issue401MenuBar(t, w).Menus, actionSessionNew); item == nil {
		t.Fatal("session.new menu item is not tagged with ActionID")
	} else if item.Shortcut == nil || item.Shortcut.Display != "F5" {
		t.Fatalf("session.new menu shortcut after rebind = %+v, want display F5", item.Shortcut)
	}
}

func TestIssue401MenuTagsRebindableActionsAndDisplaysChordFor(t *testing.T) {
	w := newTestWorkbench(t)
	w.applyBinding(actionWindowTileVertical, tv.Chord{Key: tui.KeyF6})
	w.applyBinding(actionTranscriptFind, tv.Chord{Key: tui.KeyF7})
	w.rebuildMenu()

	for _, id := range []tv.ActionID{
		actionAppQuit,
		actionSessionNew,
		actionSessionNext,
		actionSessionClose,
		actionTranscriptFind,
		actionConfigSubagents,
		actionWindowTileVertical,
		actionWindowTileHorizontal,
		actionWindowTileGrid,
		actionWindowMaximizeAll,
		actionWindowCascade,
		actionCommandPalette,
	} {
		item := issue401FindMenuItem(issue401MenuBar(t, w).Menus, id)
		if item == nil {
			t.Fatalf("menu is missing ActionID tag for %q", id)
		}
		if item.Shortcut == nil {
			t.Fatalf("menu item %q has no shortcut display", id)
		}
		if want := chordLabel(w.chordFor(id)); item.Shortcut.Display != want {
			t.Fatalf("menu item %q shortcut display = %q, want %q", id, item.Shortcut.Display, want)
		}
	}
}

func TestIssue401ConfigMenuKeybindingsVisibilityGate(t *testing.T) {
	w := newTestWorkbench(t)
	w.handlers.GetKeybindings = nil
	w.handlers.SetKeybindings = nil
	w.rebuildMenu()
	if item := issue401FindMenuLabel(issue401MenuBar(t, w).Menus, "Keybindings…"); item != nil {
		t.Fatalf("Keybindings menu item visible without handlers: %+v", item)
	}

	w.handlers.GetKeybindings = func() config.KeybindingsConfig { return config.KeybindingsConfig{} }
	w.handlers.SetKeybindings = func(config.KeybindingsConfig) {}
	w.rebuildMenu()
	item := issue401FindMenuLabel(issue401MenuBar(t, w).Menus, "Keybindings…")
	if item == nil {
		t.Fatal("Config -> Keybindings menu item missing when handlers are wired")
	}
	if item.OnSelect == nil {
		t.Fatal("Config -> Keybindings menu item has no OnSelect")
	}
	item.OnSelect()
	if top := w.desktop.TopLayer(); top == nil || top.Name != "keybinding-customizer" {
		t.Fatalf("top layer after Config -> Keybindings = %v, want keybinding-customizer", top)
	}
}

func TestIssue401LoadKeybindingsAppliesGlobalsAndRejectsBadGlobalPlainRune(t *testing.T) {
	w := newTestWorkbench(t)
	w.LoadKeybindings(config.KeybindingsConfig{Overrides: map[string]string{
		string(actionSessionNew):          "F8",
		string(actionWindowTileGrid):      "x",
		string(actionCommandPalette):      "F9",
		string(actionTranscriptFind):      "F10",
		string(actionTranscriptToggleMsg): "M",
	}})
	sw := w.openWindow("s", "S")

	assertChord(t, chordForAction(t, w, actionSessionNew), tv.Chord{Key: tui.KeyF8})
	assertChord(t, chordForAction(t, w, actionWindowTileGrid), tv.Chord{Rune: 'g', Ctrl: true, Shift: true})
	assertChord(t, chordForAction(t, w, actionCommandPalette), tv.Chord{Key: tui.KeyF9})
	assertChord(t, chordForAction(t, w, actionTranscriptFind), tv.Chord{Key: tui.KeyF10})
	assertChord(t, chordForAction(t, w, actionTranscriptToggleMsg), tv.Chord{Rune: 'm'})

	if !w.desktop.Bindings().Dispatch(tui.TypeEvent{Key: tui.KeyF8}) {
		t.Fatal("loaded global session.new override does not dispatch")
	}
	if w.desktop.Bindings().Dispatch(runeEv('x')) {
		t.Fatal("invalid plain-rune global override dispatched")
	}
	if !w.desktop.ScopedBindings().DispatchFallthrough(tui.TypeEvent{Key: tui.KeyF9}) {
		t.Fatal("loaded fallthrough command-palette override does not dispatch")
	}
	if _, fired := dispatchAtFocus(w, sw.history.Component, tui.TypeEvent{Key: tui.KeyF10}); !fired {
		t.Fatal("loaded focus transcript.find override does not dispatch")
	}
}

func TestIssue401ConflictHolderIsCatalogBasedAcrossScopes(t *testing.T) {
	w := newTestWorkbench(t)
	global, _ := w.actionByID(actionSessionNew)
	if holder, ok := w.conflictHolder(global, tv.Chord{Rune: 'w', Ctrl: true}); !ok || holder != actionSessionClose {
		t.Fatalf("global conflict holder = %q,%v, want %q,true", holder, ok, actionSessionClose)
	}
	if holder, ok := w.conflictHolder(global, tv.Chord{Rune: 'a'}); ok {
		t.Fatalf("plain 'a' in Focus scope leaked as a Global conflict holder %q", holder)
	}

	focus, _ := w.actionByID(actionTranscriptToggleMsg)
	if holder, ok := w.conflictHolder(focus, tv.Chord{Rune: 'r'}); !ok || holder != actionTranscriptToggleThink {
		t.Fatalf("focus conflict holder = %q,%v, want %q,true", holder, ok, actionTranscriptToggleThink)
	}
	w.clearBinding(actionTranscriptToggleThink)
	if holder, ok := w.conflictHolder(focus, tv.Chord{Rune: 'r'}); ok {
		t.Fatalf("cleared focus binding still reported as conflict holder %q", holder)
	}

	fallthroughAction, _ := w.actionByID(actionHelpOverlay)
	if holder, ok := w.conflictHolder(fallthroughAction, tv.Chord{Rune: ':'}); !ok || holder != actionCommandPalette {
		t.Fatalf("fallthrough conflict holder = %q,%v, want %q,true", holder, ok, actionCommandPalette)
	}
}
