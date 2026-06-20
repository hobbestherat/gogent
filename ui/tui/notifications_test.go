package ui

import (
	"errors"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/notify"
)

// TestFirstLine covers the notification-body extraction: trim, take the first
// line, and cap at notifyBodyMaxRunes so a long answer does not bloat a popup.
func TestFirstLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Task complete", "Task complete"},
		{"leading/trailing whitespace trimmed", "  hi  ", "hi"},
		{"first line only", "first\nsecond\nthird", "first"},
		{"crlf first line", "first\r\nsecond", "first"},
		{"empty", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstLine(tc.in); got != tc.want {
				t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Long input is capped to notifyBodyMaxRunes runes.
	long := strings.Repeat("x", notifyBodyMaxRunes*4)
	if got := firstLine(long); len([]rune(got)) != notifyBodyMaxRunes {
		t.Errorf("firstLine capped length = %d, want %d", len([]rune(got)), notifyBodyMaxRunes)
	}
}

// TestEventNotification covers the mapping from a session event to a
// notification reason/body, including the cases that should NOT notify
// (non-terminal events, an error with no underlying error, a non-waiting
// sub-agent).
func TestEventNotification(t *testing.T) {
	err := errors.New("boom\nnext")
	for _, tc := range []struct {
		name       string
		ev         agent.SessionEvent
		wantReason notify.Reason
		wantBody   string
		wantOK     bool
	}{
		{
			name:       "final -> complete",
			ev:         agent.SessionEvent{Type: agent.SessionEventFinal, Text: "Done\ndetails"},
			wantReason: notify.ReasonComplete, wantBody: "Done", wantOK: true,
		},
		{
			name:       "error -> error",
			ev:         agent.SessionEvent{Type: agent.SessionEventError, Err: err},
			wantReason: notify.ReasonError, wantBody: "boom", wantOK: true,
		},
		{
			name:   "error with nil err does not notify",
			ev:     agent.SessionEvent{Type: agent.SessionEventError},
			wantOK: false,
		},
		{
			name:       "subagent waiting -> clarify",
			ev:         agent.SessionEvent{Type: agent.SessionEventSubAgent, Status: agent.StatusWaiting, Result: "CLARIFY: which file?"},
			wantReason: notify.ReasonClarify, wantBody: "CLARIFY: which file?", wantOK: true,
		},
		{
			name:   "subagent completed does not notify",
			ev:     agent.SessionEvent{Type: agent.SessionEventSubAgent, Status: agent.StatusCompleted},
			wantOK: false,
		},
		{
			name:   "thinking does not notify",
			ev:     agent.SessionEvent{Type: agent.SessionEventThinking, Step: 3},
			wantOK: false,
		},
		{
			name:   "tool call does not notify",
			ev:     agent.SessionEvent{Type: agent.SessionEventToolCall, Tool: "read"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, _, body, ok := eventNotification(tc.ev)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}
