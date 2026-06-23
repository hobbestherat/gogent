package ui

import (
	"strings"
	"testing"
)

// lastNote returns the text of the most recent transcript record (the system note
// echoCommand/addNote appended), joined into one string.
func lastNote(sw *SessionWindow) string {
	recs := sw.transcript.records
	if len(recs) == 0 {
		return ""
	}
	return strings.Join(toTexts(recs[len(recs)-1].lines), "\n")
}

// TestHandleSlashCommandRoutesWatcher verifies /watcher is recognised as a
// client-side command (handled, not sent to the model) and a non-command is not.
func TestHandleSlashCommandRoutesWatcher(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	w.SetHandlers(Handlers{ListWatchers: func(string) []WatcherInfo { return nil }})

	if !sw.handleSlashCommand("/watcher list") {
		t.Error("/watcher should be handled as a client-side command")
	}
	if sw.handleSlashCommand("hello there") {
		t.Error("a plain message must not be treated as a slash command")
	}
}

// TestWatcherCommandListEmpty covers /watcher list when no watchers are visible:
// it echoes a "no watchers" note rather than an empty block.
func TestWatcherCommandListEmpty(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	w.SetHandlers(Handlers{ListWatchers: func(string) []WatcherInfo { return nil }})

	sw.handleWatcherCommand([]string{"list"})
	if note := lastNote(sw); !strings.Contains(note, "no watchers") {
		t.Errorf("empty list note = %q, want it to mention 'no watchers'", note)
	}
}

// TestWatcherCommandListFormats covers /watcher list rendering: it passes the
// CALLING session id (session-private visibility), and the row shows the name, the
// free/attached target, the schedule, status (with the disabled suffix) and the
// next-fire.
func TestWatcherCommandListFormats(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("sess-1", "S")
	var gotSession string
	w.SetHandlers(Handlers{
		ListWatchers: func(id string) []WatcherInfo {
			gotSession = id
			return []WatcherInfo{
				{ID: "f", Name: "emailer", Free: true, Enabled: true, Status: "idle", Schedule: "daily 07:00 UTC", NextFire: "07:00"},
				{ID: "a", Name: "gh", Free: false, TargetSession: "sess-1", Enabled: false, Status: "idle", Schedule: "every 5m", NextFire: "12:05"},
			}
		},
	})

	sw.handleWatcherCommand([]string{"list"})
	if gotSession != "sess-1" {
		t.Errorf("/watcher list queried session %q, want the calling session sess-1", gotSession)
	}
	note := lastNote(sw)
	for _, want := range []string{"emailer", "free", "daily 07:00 UTC", "gh", "sess-1", "disabled"} {
		if !strings.Contains(note, want) {
			t.Errorf("/watcher list note missing %q in:\n%s", want, note)
		}
	}
}

// TestWatcherCommandDispatch verifies each acting sub-command dispatches to the
// matching handler with the named watcher.
func TestWatcherCommandDispatch(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	calls := map[string]string{}
	w.SetHandlers(Handlers{
		ListWatchers:   func(string) []WatcherInfo { return nil },
		EnableWatcher:  func(n string) error { calls["enable"] = n; return nil },
		DisableWatcher: func(n string) error { calls["disable"] = n; return nil },
		RunWatcher:     func(n string) error { calls["run"] = n; return nil },
		StopWatcher:    func(n string) error { calls["stop"] = n; return nil },
	})

	for _, sub := range []string{"enable", "disable", "run", "stop"} {
		sw.handleWatcherCommand([]string{sub, "my-watcher"})
		if calls[sub] != "my-watcher" {
			t.Errorf("/watcher %s dispatched %q, want my-watcher", sub, calls[sub])
		}
	}
}

// TestWatcherCommandMultiWordName verifies a watcher name with spaces is rejoined
// from the remaining args, not truncated to the first word.
func TestWatcherCommandMultiWordName(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	var got string
	w.SetHandlers(Handlers{
		ListWatchers: func(string) []WatcherInfo { return nil },
		RunWatcher:   func(n string) error { got = n; return nil },
	})
	sw.handleWatcherCommand([]string{"run", "my", "watcher", "name"})
	if got != "my watcher name" {
		t.Errorf("multi-word name = %q, want %q", got, "my watcher name")
	}
}

// TestWatcherCommandUsage covers the usage notes: bare /watcher, and an acting
// sub-command with no name argument.
func TestWatcherCommandUsage(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	w.SetHandlers(Handlers{ListWatchers: func(string) []WatcherInfo { return nil }})

	sw.handleWatcherCommand(nil)
	if note := lastNote(sw); !strings.Contains(note, "usage:") {
		t.Errorf("bare /watcher should print usage, got %q", note)
	}

	sw.handleWatcherCommand([]string{"enable"}) // missing name
	if note := lastNote(sw); !strings.Contains(note, "usage:") {
		t.Errorf("/watcher enable with no name should print usage, got %q", note)
	}
}

// TestWatcherCommandUnknownSub covers an unrecognised sub-command: it prints the
// usage line rather than dispatching anywhere.
func TestWatcherCommandUnknownSub(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	w.SetHandlers(Handlers{ListWatchers: func(string) []WatcherInfo { return nil }})
	sw.handleWatcherCommand([]string{"frobnicate", "x"})
	if note := lastNote(sw); !strings.Contains(note, "usage:") {
		t.Errorf("unknown sub-command should print usage, got %q", note)
	}
}

// TestWatcherCommandUnavailable covers the unwired-handler paths: list and the
// acting sub-commands each report "unavailable" rather than panicking on a nil
// handler.
func TestWatcherCommandUnavailable(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	w.SetHandlers(Handlers{}) // nothing wired

	sw.handleWatcherCommand([]string{"list"})
	if note := lastNote(sw); !strings.Contains(note, "unavailable") {
		t.Errorf("/watcher list with no ListWatchers should report unavailable, got %q", note)
	}

	sw.handleWatcherCommand([]string{"enable", "x"})
	if note := lastNote(sw); !strings.Contains(note, "unavailable") {
		t.Errorf("/watcher enable with no EnableWatcher should report unavailable, got %q", note)
	}
}

// TestWatcherCommandError verifies a handler error is surfaced in the echoed note
// (the user sees why the action failed).
func TestWatcherCommandError(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("s", "S")
	w.SetHandlers(Handlers{
		ListWatchers: func(string) []WatcherInfo { return nil },
		RunWatcher:   func(string) error { return errBoom },
	})
	sw.handleWatcherCommand([]string{"run", "x"})
	note := lastNote(sw)
	if !strings.Contains(note, "failed") || !strings.Contains(note, "boom") {
		t.Errorf("a handler error should be echoed, got %q", note)
	}
}

// errBoom is a sentinel error for the dispatch-error test.
var errBoom = stringError("boom")

type stringError string

func (e stringError) Error() string { return string(e) }
