package gogent

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/tool"
)

type issue406FakeAsker struct {
	called chan agent.QuestionRequest
	reply  chan issue406AskReply
}

type issue406AskReply struct {
	resp agent.QuestionResponse
	err  error
}

func newIssue406FakeAsker() *issue406FakeAsker {
	return &issue406FakeAsker{
		called: make(chan agent.QuestionRequest, 1),
		reply:  make(chan issue406AskReply, 1),
	}
}

func (f *issue406FakeAsker) AskQuestions(req agent.QuestionRequest) (agent.QuestionResponse, error) {
	f.called <- req
	r := <-f.reply
	return r.resp, r.err
}

func issue406Args() map[string]interface{} {
	return map[string]interface{}{
		"title":   "Project setup",
		"summary": "Gather scope before planning.",
		"topics": []interface{}{
			map[string]interface{}{
				"title": "Scope",
				"items": []interface{}{
					map[string]interface{}{"id": "name", "label": "Name", "type": "text", "required": true, "placeholder": "Ada"},
					map[string]interface{}{"id": "notes", "label": "Notes", "type": "textarea", "help": "Optional context"},
				},
			},
			map[string]interface{}{
				"title": "Choices",
				"items": []interface{}{
					map[string]interface{}{"id": "priority", "label": "Priority", "type": "choice", "options": []interface{}{"low", "high"}},
					map[string]interface{}{"id": "frameworks", "label": "Frameworks", "type": "multiselect", "options": []interface{}{"react", "svelte"}},
				},
			},
		},
	}
}

func TestIssue406AskUserToolRegisteredNonReadOnlyAndSchemaNormalized(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	ask := g.GetToolRegistry().Get("ask_user")
	if ask == nil {
		t.Fatal("ask_user tool is not registered")
	}
	if ask.ReadOnly {
		t.Fatal("ask_user ReadOnly = true, want false so it runs on the serial/blocking path")
	}
	if ask.Strict {
		t.Fatal("ask_user Strict = true, want false for the nested rich schema")
	}

	schema, ok := ask.InputSchema.(map[string]interface{})
	if !ok {
		t.Fatalf("InputSchema = %T, want map[string]interface{}", ask.InputSchema)
	}
	if got := schema["type"]; got != "object" {
		t.Fatalf("schema root type = %v, want object", got)
	}
	if !issue406StringListContains(schema["required"], "topics") {
		t.Fatalf("schema required = %#v, want topics required", schema["required"])
	}
	topics, ok := propertySchema(schema, "topics")
	if !ok {
		t.Fatal("schema missing topics property")
	}
	if topics["type"] != "array" {
		t.Fatalf("topics.type = %v, want array", topics["type"])
	}
	topicItems, ok := topics["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("topics.items = %T, want map[string]interface{}", topics["items"])
	}
	if !issue406StringListContains(topicItems["required"], "title") || !issue406StringListContains(topicItems["required"], "items") {
		t.Fatalf("topic required = %#v, want title and items", topicItems["required"])
	}
	itemProps := topicItems["properties"].(map[string]interface{})["items"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})
	typ := itemProps["type"].(map[string]interface{})
	if !reflect.DeepEqual(typ["enum"], []string{"text", "textarea", "choice", "multiselect"}) {
		t.Fatalf("item type enum = %#v, want text/textarea/choice/multiselect", typ["enum"])
	}
}

func TestIssue406AskUserExecuteBlocksAndReturnsStructuredAnswers(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	fake := newIssue406FakeAsker()
	g.SetQuestionAsker(fake)
	ask := g.GetToolRegistry().Get("ask_user")

	done := make(chan struct {
		result interface{}
		err    error
	}, 1)
	go func() {
		result, err := ask.Execute(issue406Args(), tool.ToolContext{SessionID: "session-1", AgentID: "agent-1"})
		done <- struct {
			result interface{}
			err    error
		}{result: result, err: err}
	}()

	var req agent.QuestionRequest
	select {
	case req = <-fake.called:
	case <-time.After(time.Second):
		t.Fatal("ask_user did not call the QuestionAsker")
	}
	if req.SessionID != "session-1" || req.AgentID != "agent-1" {
		t.Fatalf("request context = session %q agent %q, want session-1/agent-1", req.SessionID, req.AgentID)
	}
	if req.Title != "Project setup" || len(req.Topics) != 2 || len(req.Topics[1].Items) != 2 {
		t.Fatalf("request was not parsed as expected: %+v", req)
	}

	select {
	case got := <-done:
		t.Fatalf("ask_user returned before the fake asker replied: result=%#v err=%v", got.result, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	fake.reply <- issue406AskReply{resp: agent.QuestionResponse{Answers: map[string]interface{}{
		"name":       "Ada",
		"notes":      "see spec",
		"priority":   "high",
		"frameworks": []string{"react", "svelte"},
	}}}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ask_user returned error: %v", got.err)
		}
		answers, ok := got.result.(map[string]interface{})
		if !ok {
			t.Fatalf("result = %T, want map[string]interface{}", got.result)
		}
		if answers["name"] != "Ada" || answers["notes"] != "see spec" || answers["priority"] != "high" {
			t.Fatalf("scalar answers = %#v", answers)
		}
		if !reflect.DeepEqual(answers["frameworks"], []string{"react", "svelte"}) {
			t.Fatalf("frameworks = %#v, want []string{\"react\", \"svelte\"}", answers["frameworks"])
		}
	case <-time.After(time.Second):
		t.Fatal("ask_user did not unblock after the fake asker replied")
	}
}

func TestIssue406AskUserHeadlessFallbackDoesNotHang(t *testing.T) {
	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	ask := g.GetToolRegistry().Get("ask_user")
	done := make(chan error, 1)
	go func() {
		_, err := ask.Execute(issue406Args(), tool.ToolContext{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ask_user unavailable: no interactive UI") {
			t.Fatalf("headless error = %v, want unavailable UI tool error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("headless ask_user hung instead of returning a safe unavailable error")
	}
}

func TestIssue406AskUserCancelErrorAndNilAnswers(t *testing.T) {
	tests := []struct {
		name      string
		reply     issue406AskReply
		wantErr   string
		wantEmpty bool
	}{
		{
			name:    "cancelled",
			reply:   issue406AskReply{resp: agent.QuestionResponse{Cancelled: true}},
			wantErr: "ask_user cancelled",
		},
		{
			name:      "nil answers become empty object",
			reply:     issue406AskReply{resp: agent.QuestionResponse{}},
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
			fake := newIssue406FakeAsker()
			g.SetQuestionAsker(fake)
			ask := g.GetToolRegistry().Get("ask_user")

			done := make(chan struct {
				result interface{}
				err    error
			}, 1)
			go func() {
				result, err := ask.Execute(issue406Args(), tool.ToolContext{})
				done <- struct {
					result interface{}
					err    error
				}{result: result, err: err}
			}()
			select {
			case <-fake.called:
			case <-time.After(time.Second):
				t.Fatal("ask_user did not call fake asker")
			}
			fake.reply <- tc.reply

			select {
			case got := <-done:
				if tc.wantErr != "" {
					if got.err == nil || !strings.Contains(got.err.Error(), tc.wantErr) {
						t.Fatalf("error = %v, want containing %q", got.err, tc.wantErr)
					}
					return
				}
				if got.err != nil {
					t.Fatalf("unexpected error: %v", got.err)
				}
				if tc.wantEmpty {
					answers, ok := got.result.(map[string]interface{})
					if !ok || len(answers) != 0 {
						t.Fatalf("result = %#v, want empty answer object", got.result)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("ask_user did not unblock after fake reply")
			}
		})
	}
}

func TestIssue406AskUserValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{
			name: "missing topics",
			args: map[string]interface{}{},
			want: "topics",
		},
		{
			name: "duplicate ids across topics",
			args: map[string]interface{}{"topics": []interface{}{
				map[string]interface{}{"title": "A", "items": []interface{}{map[string]interface{}{"id": "same", "label": "One", "type": "text"}}},
				map[string]interface{}{"title": "B", "items": []interface{}{map[string]interface{}{"id": "same", "label": "Two", "type": "textarea"}}},
			}},
			want: "duplicate item id",
		},
		{
			name: "choice requires options",
			args: map[string]interface{}{"topics": []interface{}{
				map[string]interface{}{"title": "A", "items": []interface{}{map[string]interface{}{"id": "priority", "label": "Priority", "type": "choice"}}},
			}},
			want: "requires a non-empty \"options\" array",
		},
		{
			name: "invalid item type",
			args: map[string]interface{}{"topics": []interface{}{
				map[string]interface{}{"title": "A", "items": []interface{}{map[string]interface{}{"id": "x", "label": "X", "type": "date"}}},
			}},
			want: "invalid type",
		},
	}

	g := NewGogentWithWorkspace(t.TempDir(), t.TempDir())
	ask := g.GetToolRegistry().Get("ask_user")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ask.Execute(tc.args, tool.ToolContext{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func issue406StringListContains(v interface{}, want string) bool {
	switch xs := v.(type) {
	case []string:
		for _, x := range xs {
			if x == want {
				return true
			}
		}
	case []interface{}:
		for _, x := range xs {
			if x == want {
				return true
			}
		}
	}
	return false
}
