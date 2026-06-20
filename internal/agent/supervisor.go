package agent

import (
	"fmt"
	"strings"

	"gogent/internal/model"
)

// This file holds the harness-level supervisor's completion-check decision logic
// (issue #172). The supervisor watches a session and, when it goes idle despite a
// persisted /goal not being met, nudges it to continue. The decision of "is the
// goal met?" lives here — independent of the TUI — so it can be unit-tested
// directly: the rule-based todo short-circuit is a pure function, and the model
// judge is injected as a GoalJudge so tests drive it deterministically.

// SupervisorNudgeTemplate is the wording of a supervisor nudge turn (issue #172).
// It is a named constant (not inline prose) so the phrasing is tunable in one
// place; %s is the persisted goal. The leading "[Supervisor: …]" tag marks the
// turn as supervisor-originated so the UI can distinguish it and avoid resetting
// the nudge budget on it.
const SupervisorNudgeTemplate = "[Supervisor: the goal is not yet complete — continue. Goal: %s]"

// SupervisorNudge renders the nudge text for a goal.
func SupervisorNudge(goal string) string {
	return fmt.Sprintf(SupervisorNudgeTemplate, strings.TrimSpace(goal))
}

// IsSupervisorNudge reports whether a message body is a supervisor-originated
// nudge (issue #172). The UI uses this to avoid resetting the nudge budget when
// the supervisor's own turn re-enters the send path.
func IsSupervisorNudge(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "[Supervisor:")
}

// GoalJudge decides whether goal is satisfied given the conversation transcript.
// It is the injectable model-call seam for the completion check: production wires
// a single lightweight model completion (see UserSession.judgeGoal); tests supply
// a deterministic stub. done is the verdict; err is a check failure (the caller
// treats an errored check as "not done" so a transient model error never declares
// premature success).
type GoalJudge func(goal string, transcript []model.Message) (done bool, err error)

// TodosComplete classifies a session's checklist for the supervisor's rule-based
// short-circuit (issue #172). hasTodos reports whether the session has any todo
// items at all; allDone reports whether every item is completed. A session with
// no todos has hasTodos=false (and allDone=false), so the caller falls through to
// the model judge rather than treating an empty list as "done".
func TodosComplete(todos []TodoItem) (allDone, hasTodos bool) {
	if len(todos) == 0 {
		return false, false
	}
	for _, t := range todos {
		if t.Status != TodoCompleted {
			return false, true
		}
	}
	return true, true
}

// CheckGoalComplete is the supervisor's completion check (issue #172): is goal
// satisfied given the session's todos and conversation so far? It is cheap and
// deterministic where it can be:
//
//   - If the session has todos, their state decides it with no model call: all
//     complete → done; any incomplete → not done.
//   - Otherwise it falls back to a single lightweight model judge.
//
// An empty/blank goal is treated as "done" (nothing to supervise). A nil judge
// with no decisive todos also yields done=false so the caller does not nudge
// without a way to ever decide completion.
func CheckGoalComplete(goal string, todos []TodoItem, judge GoalJudge, transcript []model.Message) (bool, error) {
	if strings.TrimSpace(goal) == "" {
		return true, nil
	}
	if allDone, hasTodos := TodosComplete(todos); hasTodos {
		return allDone, nil
	}
	if judge == nil {
		return false, nil
	}
	return judge(goal, transcript)
}

// supervisorJudgePrompt frames the transcript-vs-goal judgement for the model
// (issue #172). The model is asked for a bare yes/no so the parse is trivial and
// the call stays cheap; the transcript is summarised by role so the judge sees
// what happened without re-streaming tool payloads.
const supervisorJudgePrompt = "You are a task supervisor. Given the goal and the conversation transcript so " +
	"far, decide whether the goal has been fully accomplished. Answer with a single " +
	"word: \"yes\" if the goal is fully satisfied, otherwise \"no\". Do not explain.\n\n" +
	"Goal: %s\n\nTranscript:\n%s"

// GoalSatisfied runs the supervisor's completion check for goal against this
// session (issue #172): the todo short-circuit first, then a single lightweight
// model judge over the session's transcript when todos are not decisive. It is
// the production wiring of CheckGoalComplete; the judge call uses the session's
// own primary completer so the verdict reflects the same model the work ran on.
func (s *UserSession) GoalSatisfied(goal string) (bool, error) {
	return CheckGoalComplete(goal, s.Todos(), s.judgeGoal, s.transcriptForJudge())
}

// transcriptForJudge returns the session's primary-model conversation transcript
// for the completion judge, or nil when no model session is wired.
func (s *UserSession) transcriptForJudge() []model.Message {
	if s.RootAgent == nil || s.RootAgent.ThoughtTrain == nil {
		return nil
	}
	return s.RootAgent.ThoughtTrain.GetTranscript()
}

// judgeGoal is the production GoalJudge: a single blocking completion on the
// session's primary model asking whether goal is satisfied by the transcript
// (issue #172). It returns done=false on any error (a missing model, a request
// failure) so a flaky check never declares premature success.
func (s *UserSession) judgeGoal(goal string, transcript []model.Message) (bool, error) {
	if s.RootAgent == nil || s.RootAgent.ThoughtTrain == nil || s.RootAgent.ThoughtTrain.Model == nil {
		return false, fmt.Errorf("supervisor: no model available for completion check")
	}
	prompt := fmt.Sprintf(supervisorJudgePrompt, strings.TrimSpace(goal), summariseTranscript(transcript))
	resp, err := s.RootAgent.ThoughtTrain.Model.Complete([]model.Message{
		{Role: model.RoleUser, Content: prompt},
	})
	if err != nil {
		return false, fmt.Errorf("supervisor: completion check: %w", err)
	}
	if resp == nil {
		return false, fmt.Errorf("supervisor: empty completion-check response")
	}
	return parseYesNo(resp.Content), nil
}

// parseYesNo reads a yes/no verdict from a model reply, tolerant of surrounding
// punctuation and prose: it is "yes" only when the first word is an affirmative.
func parseYesNo(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimLeft(t, "\"'*`>-• \t")
	switch {
	case strings.HasPrefix(t, "yes"), strings.HasPrefix(t, "true"), strings.HasPrefix(t, "done"):
		return true
	default:
		return false
	}
}

// summariseTranscript renders the conversation as plain "role: text" lines for
// the completion judge, dropping empty/tool-plumbing turns so the prompt stays
// cheap. The whole point of the supervisor is to keep model cost low, so the
// summary is intentionally terse.
func summariseTranscript(transcript []model.Message) string {
	var b strings.Builder
	for _, m := range transcript {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		role := string(m.Role)
		if role == "" {
			role = "message"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}
