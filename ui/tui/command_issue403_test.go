package ui

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type commandSendIssue403 struct {
	sessionID string
	message   string
	model     string
	effort    string
}

func recordCommandSendsIssue403(w *Workbench) <-chan commandSendIssue403 {
	sent := make(chan commandSendIssue403, 8)
	w.handlers.OnSend = func(sessionID, message, model, effort string) {
		sent <- commandSendIssue403{sessionID: sessionID, message: message, model: model, effort: effort}
	}
	return sent
}

func waitCommandSendIssue403(t *testing.T, sent <-chan commandSendIssue403) commandSendIssue403 {
	t.Helper()
	select {
	case got := <-sent:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for custom-command send")
		return commandSendIssue403{}
	}
}

func assertNoCommandSendIssue403(t *testing.T, sent <-chan commandSendIssue403) {
	t.Helper()
	select {
	case got := <-sent:
		t.Fatalf("unexpected custom-command send: %#v", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestIssue403HandleSlashCommandDispatchesCustomExpandedPrompt(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("sess-403", "S")
	sent := recordCommandSendsIssue403(w)
	w.handlers.GetCustomCommand = func(name string) (CommandDef, error) {
		if name != "create-component" {
			return CommandDef{}, errors.New("not found")
		}
		return CommandDef{
			Name:     name,
			Template: "Create ${name}Type in $dir as $kind. Leave $unknown alone.",
			Parameters: []CommandParam{
				{Name: "name", Required: true},
				{Name: "dir", Default: "src/components"},
				{Name: "kind", Default: "view"},
			},
			Model: "model-override",
		}, nil
	}

	if !sw.handleSlashCommand("/create-component Button dir=src/widgets") {
		t.Fatal("custom slash command should be handled")
	}
	got := waitCommandSendIssue403(t, sent)
	if got.sessionID != "sess-403" {
		t.Fatalf("sent session = %q, want sess-403", got.sessionID)
	}
	want := "Create ButtonType in src/widgets as view. Leave $unknown alone."
	if got.message != want {
		t.Fatalf("sent message = %q, want %q", got.message, want)
	}
	if got.model != "model-override" {
		t.Fatalf("model override = %q, want model-override", got.model)
	}
}

func TestIssue403HandleSlashCommandMissingRequiredShowsErrorAndDoesNotSend(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("sess-403", "S")
	sent := recordCommandSendsIssue403(w)
	w.handlers.GetCustomCommand = func(name string) (CommandDef, error) {
		return CommandDef{
			Name:       name,
			Template:   "Review $target",
			Parameters: []CommandParam{{Name: "target", Required: true}},
		}, nil
	}

	if !sw.handleSlashCommand("/review") {
		t.Fatal("known custom command with bad args should still be handled")
	}
	assertNoCommandSendIssue403(t, sent)
	if note := lastNote(sw); !strings.Contains(note, "missing required parameter") || !strings.Contains(note, "target") {
		t.Fatalf("missing-required note = %q, want parameter error", note)
	}
}

func TestIssue403HandleSlashCommandUnknownCustomFallsThrough(t *testing.T) {
	w := newTestWorkbench(t)
	sw := w.openWindow("sess-403", "S")
	sent := recordCommandSendsIssue403(w)
	var lookedUp string
	w.handlers.GetCustomCommand = func(name string) (CommandDef, error) {
		lookedUp = name
		return CommandDef{}, errors.New("not found")
	}

	if sw.handleSlashCommand("/does-not-exist arg") {
		t.Fatal("unknown custom command should fall through to normal send path")
	}
	if lookedUp != "does-not-exist" {
		t.Fatalf("looked up command %q, want does-not-exist", lookedUp)
	}
	assertNoCommandSendIssue403(t, sent)
}

func TestIssue403HandleSlashCommandCustomDoesNotShadowReservedBuiltins(t *testing.T) {
	for _, slash := range []string{"/undo", "/help", "/read"} {
		t.Run(slash, func(t *testing.T) {
			w := newTestWorkbench(t)
			sw := w.openWindow("sess-403", "S")
			sent := recordCommandSendsIssue403(w)
			var lookedUp bool
			w.handlers.GetCustomCommand = func(name string) (CommandDef, error) {
				lookedUp = true
				return CommandDef{Name: name, Template: "shadowed"}, nil
			}

			handled := sw.handleSlashCommand(slash)
			assertNoCommandSendIssue403(t, sent)
			if slash == "/undo" {
				if !handled {
					t.Fatalf("%s is a client-side built-in and should be handled by its built-in path", slash)
				}
				if lookedUp {
					t.Fatalf("%s should not query custom command registry after built-in handling", slash)
				}
				return
			}
			if handled {
				t.Fatalf("%s is a reserved built-in/backend/file command and must not dispatch as custom", slash)
			}
			if lookedUp {
				t.Fatalf("%s should be rejected as reserved before querying custom commands", slash)
			}
		})
	}
}
