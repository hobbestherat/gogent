package agent

// This file defines the neutral bridge between the model-facing `ask_user` tool
// and whatever front-end can render a structured question form (issue #406). It
// lives in internal/agent — a package both internal/gogent (which registers the
// tool) and ui/tui (which implements the asker) already import — so neither side
// has to reach across into the other. It mirrors the EditReviewer/EditReviewRequest
// bridge (internal/gogent/review.go) used for the diff-review modal.

// QuestionItemType is the renderable kind of a single question item. It maps 1:1
// onto a form widget: text → single-line input, textarea → multi-line input,
// choice → single-select (radio) group, multiselect → checkbox group.
type QuestionItemType string

const (
	// QuestionText is a single-line free-text field.
	QuestionText QuestionItemType = "text"
	// QuestionTextarea is a multi-line free-text field.
	QuestionTextarea QuestionItemType = "textarea"
	// QuestionChoice is a single-select among Options (mutually exclusive).
	QuestionChoice QuestionItemType = "choice"
	// QuestionMultiSelect is a multi-select among Options (zero or more).
	QuestionMultiSelect QuestionItemType = "multiselect"
)

// QuestionItem is one point the model wants answered. ID is the stable key the
// answer is reported under in QuestionResponse.Answers, so it must be unique
// across the whole request. Options is required for QuestionChoice and
// QuestionMultiSelect and ignored otherwise. Placeholder and Help are optional
// presentation hints.
type QuestionItem struct {
	ID          string
	Label       string
	Type        QuestionItemType
	Options     []string
	Required    bool
	Placeholder string
	Help        string
}

// QuestionTopic is a named group of items. Each topic becomes one tab in the
// dialog.
type QuestionTopic struct {
	Title string
	Items []QuestionItem
}

// QuestionRequest is a structured, multi-topic question the model asks via the
// ask_user tool. SessionID and AgentID identify the requesting session/sub-agent
// (set by the tool from its ToolContext, not by the model) so the UI can badge the
// right sidebar node and name the requester — exactly like EditReviewRequest. They
// carry no meaning for a headless asker.
type QuestionRequest struct {
	SessionID string
	AgentID   string
	Title     string
	Summary   string
	Topics    []QuestionTopic
}

// QuestionResponse carries the user's answers, keyed by QuestionItem.ID. Per item
// type the value is: multiselect → []string, choice/text/textarea → string. Items
// the user left unanswered are omitted from the map rather than stored as a zero
// value. Cancelled is true when the user dismissed the dialog (Escape/Cancel) or
// the UI shut down before answering; Answers is then empty.
type QuestionResponse struct {
	Answers   map[string]interface{}
	Cancelled bool
}

// QuestionAsker renders a QuestionRequest and blocks until the user answers or
// cancels. It is implemented by the interactive UI and invoked from the agent
// goroutine (via the ask_user tool), so AskQuestions must block until resolved —
// the same contract as permission.Prompter and EditReviewer. A nil asker means
// "no interactive UI"; the tool reports it cannot ask rather than hanging.
type QuestionAsker interface {
	AskQuestions(req QuestionRequest) (QuestionResponse, error)
}
