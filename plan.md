# Issue #406 (gogent side): structured `ask_user` tool + tabbed question dialog

## Dep
- Bump turbotui → `52604ee` (done), `go mod tidy`, no replace.

## A. Neutral bridge types + interface — `internal/agent/question.go`
- `QuestionItemType` enum: `text`, `textarea`, `choice`, `multiselect`.
- `QuestionItem{ID, Label, Type, Options, Required, Placeholder, Help}`.
- `QuestionTopic{Title, Items}`.
- `QuestionRequest{SessionID, AgentID, Title, Summary, Topics}` — SessionID/AgentID set
  by the tool from ToolContext (for sidebar badge/notify, mirrors EditReviewRequest).
- `QuestionResponse{Answers map[string]interface{}, Cancelled bool}`.
- `QuestionAsker interface { AskQuestions(QuestionRequest) (QuestionResponse, error) }`.
Chosen `internal/agent` because both `internal/gogent` and `ui/tui` already import it
(no cycle: agent does not import gogent).

## A. Tool — `internal/gogent`
- `gogent.go`: add field `questionAsker agent.QuestionAsker`.
- New `internal/gogent/question.go`: `SetQuestionAsker`, `askUser` (the blocking
  Execute body), `parseQuestionRequest(args, ctx)`.
- Register `ask_user` in `initializeToolRegistry()` (before `enableWatcherTools()`):
  - NOT ReadOnly (serial/blocking path), NOT Strict (nested arrays/objects).
  - InputSchema per issue §A (normalized by Register → NormalizeSchema).
  - Execute: parse → if `questionAsker==nil` error `"ask_user unavailable: no interactive UI"`;
    else `AskQuestions`; Cancelled → error; else return `resp.Answers` (keyed by id,
    `{}` when empty). multiselect→[]string, choice/text/textarea→string.
- `user_session.go`: add `"ask_user"` to `planKeptTools` so CloneForPlanMode retains it.

## B. Dialog — `ui/tui/question_dialog.go`
- `(*Workbench) AskQuestions` implements `agent.QuestionAsker`: notify + markApproval
  (badge), then `serializePrompt(w, QuestionResponse{Cancelled:true}, …)` →
  `desktop.Post` → `presentBackgroundModal` → `showQuestionDialog`. Returns (resp,nil).
- `notifyQuestions` mirrors `notifyReview`.
- `showQuestionDialog(desktop, req, resolve)`:
  - `tv.NewDialog`; optional summary label; one `tv.Tabs` (tab per topic).
  - Per tab a fixed-Rect panel of items: multiselect→`tv.MultiSelect` of Checkbox,
    choice→`tv.RadioGroup` of Checkbox, text→`tv.TextBox`, textarea→`tv.MultiLineInput`.
    Optional help dim-label line per item.
  - Each item → a `questionField{item, answer func() (interface{}, bool)}`.
  - Buttons: Cancel / Prev / Next / Submit. Inline error label.
  - Submit validates required (first miss → switch to its tab + inline error), then
    `resolve(Answers)`. Escape / Cancel / Ctrl+Enter handled at dialog root.

## Wiring — `cmd/main.go`
- After `g.SetReviewer(wb)`: `g.SetQuestionAsker(wb)`.

## Tester targets
`agent.QuestionAsker`/`QuestionRequest`/`QuestionResponse`; tool name `ask_user`
(ReadOnly false, schema normalizes); Execute keyed result + headless error; UI
`showQuestionDialog` tabs/widgets + required-validation.
