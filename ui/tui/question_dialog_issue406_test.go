package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

func issue406QuestionRequest() agent.QuestionRequest {
	return agent.QuestionRequest{
		Title:   "Project setup",
		Summary: "Gather scope before planning.",
		Topics: []agent.QuestionTopic{
			{
				Title: "Choices",
				Items: []agent.QuestionItem{
					{ID: "frameworks", Label: "Frameworks", Type: agent.QuestionMultiSelect, Options: []string{"react", "svelte"}},
					{ID: "priority", Label: "Priority", Type: agent.QuestionChoice, Options: []string{"low", "high"}},
				},
			},
			{
				Title: "Details",
				Items: []agent.QuestionItem{
					{ID: "name", Label: "Name", Type: agent.QuestionText, Required: true, Placeholder: "Ada Lovelace"},
					{ID: "notes", Label: "Notes", Type: agent.QuestionTextarea, Placeholder: "Optional notes"},
				},
			},
		},
	}
}

func TestIssue406QuestionDialogTabsValidationAndStructuredSubmit(t *testing.T) {
	var output bytes.Buffer
	app := tui.NewWithSize(100, 32, &output)
	desktop := tv.NewDesktop(app)
	result := make(chan agent.QuestionResponse, 1)

	showQuestionDialog(desktop, issue406QuestionRequest(), func(resp agent.QuestionResponse) {
		result <- resp
	})
	desktop.Redraw()

	if top := desktop.TopLayer(); top == nil || top.Name != "question-dialog" {
		t.Fatalf("top layer = %#v, want question-dialog modal", top)
	}
	screen := issue406ScreenText(app)
	for _, want := range []string{
		"Project setup",
		"Gather scope before planning.",
		"Choices",
		"Details",
		"Frameworks",
		"react",
		"svelte",
		"Priority",
		"low",
		"high",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("rendered dialog missing %q:\n%s", want, screen)
		}
	}

	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: ' '}) // react
	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab})
	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: ' '}) // svelte
	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab})
	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab})
	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: ' '}) // high

	issue406SubmitViaDialogRoot(t, desktop)
	select {
	case got := <-result:
		t.Fatalf("submit returned despite missing required text: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	desktop.Redraw()
	screen = issue406ScreenText(app)
	if !strings.Contains(screen, "Name is required") {
		t.Fatalf("required validation error was not rendered:\n%s", screen)
	}
	if !strings.Contains(screen, "Name") || !strings.Contains(screen, "Notes") {
		t.Fatalf("validation did not switch to the tab with the missing required field:\n%s", screen)
	}

	issue406TypeString(t, app, "Ada")
	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyTab})
	issue406TypeString(t, app, "see spec")
	issue406SubmitViaDialogRoot(t, desktop)

	select {
	case got := <-result:
		if got.Cancelled {
			t.Fatal("submit returned Cancelled=true, want answers")
		}
		if got.Answers["name"] != "Ada" || got.Answers["notes"] != "see spec" || got.Answers["priority"] != "high" {
			t.Fatalf("scalar answers = %#v", got.Answers)
		}
		frameworks, ok := got.Answers["frameworks"].([]string)
		if !ok {
			t.Fatalf("frameworks answer = %T %#v, want []string", got.Answers["frameworks"], got.Answers["frameworks"])
		}
		if strings.Join(frameworks, ",") != "react,svelte" {
			t.Fatalf("frameworks = %#v, want react,svelte", frameworks)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dialog submit result")
	}
	if top := desktop.TopLayer(); top != nil {
		t.Fatalf("dialog layer still present after submit: %#v", top)
	}
}

func TestIssue406QuestionDialogEscapeCancels(t *testing.T) {
	app := tui.NewWithSize(80, 24, &bytes.Buffer{})
	desktop := tv.NewDesktop(app)
	result := make(chan agent.QuestionResponse, 1)

	showQuestionDialog(desktop, issue406QuestionRequest(), func(resp agent.QuestionResponse) {
		result <- resp
	})
	if top := desktop.TopLayer(); top == nil {
		t.Fatal("dialog was not shown")
	}

	issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyEscape})
	select {
	case got := <-result:
		if !got.Cancelled {
			t.Fatalf("Escape result = %+v, want Cancelled=true", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Escape cancellation")
	}
	if top := desktop.TopLayer(); top != nil {
		t.Fatalf("dialog layer still present after Escape: %#v", top)
	}
}

func issue406Dispatch(t *testing.T, app *tui.App, ev tui.TypeEvent) {
	t.Helper()
	handlers := append([]func(tui.TypeEvent){}, *exportedField[[]func(tui.TypeEvent)](t, app, "typeHandlers")...)
	if len(handlers) == 0 {
		t.Fatal("app has no type handlers; desktop input dispatch is not wired")
	}
	for _, handler := range handlers {
		handler(ev)
	}
}

func issue406TypeString(t *testing.T, app *tui.App, s string) {
	t.Helper()
	for _, r := range s {
		issue406Dispatch(t, app, tui.TypeEvent{Key: tui.KeyRune, Rune: r})
	}
}

func issue406SubmitViaDialogRoot(t *testing.T, desktop *tv.Desktop) {
	t.Helper()
	top := desktop.TopLayer()
	if top == nil {
		t.Fatal("no dialog layer to submit")
	}
	if top.Root.OnTypeFn == nil {
		t.Fatal("dialog root has no type handler")
	}
	if !top.Root.OnTypeFn(top.Root, tui.TypeEvent{Key: tui.KeyEnter, Ctrl: true}) {
		t.Fatal("dialog root did not consume Ctrl+Enter submit")
	}
}

func issue406ScreenText(app *tui.App) string {
	var b strings.Builder
	for y := 0; y < app.Height(); y++ {
		for x := 0; x < app.Width(); x++ {
			ch := app.ReadCell(x, y).Ch
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
