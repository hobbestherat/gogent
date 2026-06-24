package gogent

import (
	"fmt"

	"gogent/internal/agent"
	"gogent/internal/tool"
)

// SetQuestionAsker installs the interactive question asker behind the `ask_user`
// tool (issue #406). With no asker the tool is inert in the safe direction: it
// reports it cannot ask (a headless run has nobody to interview), mirroring the
// diff-review gate's deny-when-no-reviewer and permission's deny-when-no-prompter.
func (g *Gogent) SetQuestionAsker(a agent.QuestionAsker) {
	g.mu.Lock()
	g.questionAsker = a
	g.mu.Unlock()
}

// askUser is the `ask_user` tool's Execute body. It parses the model's request,
// blocks on the installed QuestionAsker until the user answers, and returns the
// answers keyed by item id as the tool result. It is non-ReadOnly so it always
// runs on the serial path and blocks exactly like a permission prompt.
//
// Failure modes are reported as tool errors (not Go errors that abort the turn) so
// the model can recover by falling back to a plain-text question, as the tool
// description instructs: no interactive UI installed, or the user cancelled.
func (g *Gogent) askUser(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
	req, err := parseQuestionRequest(args)
	if err != nil {
		return nil, err
	}
	req.SessionID = ctx.SessionID
	req.AgentID = ctx.AgentID

	g.mu.RLock()
	asker := g.questionAsker
	g.mu.RUnlock()
	if asker == nil {
		return nil, fmt.Errorf("ask_user unavailable: no interactive UI")
	}

	resp, err := asker.AskQuestions(req)
	if err != nil {
		return nil, fmt.Errorf("ask_user failed: %w", err)
	}
	if resp.Cancelled {
		return nil, fmt.Errorf("ask_user cancelled: the user dismissed the question dialog without answering")
	}

	// The result body is a flat JSON object keyed by item id (never null), so an
	// all-optional-unanswered form still returns a well-formed {}.
	if resp.Answers == nil {
		return map[string]interface{}{}, nil
	}
	return resp.Answers, nil
}

// parseQuestionRequest builds an agent.QuestionRequest from the model's decoded
// tool arguments, validating the shape the JSON Schema cannot fully express
// (topics non-empty, every item has id/label/type, choice/multiselect carry
// options). A clear error here surfaces to the model as a tool error so it can fix
// the call rather than present a broken form.
func parseQuestionRequest(args map[string]interface{}) (agent.QuestionRequest, error) {
	req := agent.QuestionRequest{
		Title:   stringArg(args, "title"),
		Summary: stringArg(args, "summary"),
	}

	rawTopics, ok := args["topics"].([]interface{})
	if !ok || len(rawTopics) == 0 {
		return req, fmt.Errorf("ask_user requires a non-empty \"topics\" array")
	}

	seen := make(map[string]bool)
	for ti, rt := range rawTopics {
		tm, ok := rt.(map[string]interface{})
		if !ok {
			return req, fmt.Errorf("topics[%d] must be an object", ti)
		}
		topic := agent.QuestionTopic{Title: asString(tm["title"])}
		rawItems, ok := tm["items"].([]interface{})
		if !ok || len(rawItems) == 0 {
			return req, fmt.Errorf("topics[%d] requires a non-empty \"items\" array", ti)
		}
		for ii, ri := range rawItems {
			im, ok := ri.(map[string]interface{})
			if !ok {
				return req, fmt.Errorf("topics[%d].items[%d] must be an object", ti, ii)
			}
			item, err := parseQuestionItem(im, ti, ii)
			if err != nil {
				return req, err
			}
			if seen[item.ID] {
				return req, fmt.Errorf("duplicate item id %q: ids must be unique across the whole request", item.ID)
			}
			seen[item.ID] = true
			topic.Items = append(topic.Items, item)
		}
		req.Topics = append(req.Topics, topic)
	}
	return req, nil
}

// parseQuestionItem validates and converts a single item object. ti/ii index the
// owning topic/item for legible error messages.
func parseQuestionItem(im map[string]interface{}, ti, ii int) (agent.QuestionItem, error) {
	item := agent.QuestionItem{
		ID:          asString(im["id"]),
		Label:       asString(im["label"]),
		Type:        agent.QuestionItemType(asString(im["type"])),
		Required:    asBool(im["required"]),
		Placeholder: asString(im["placeholder"]),
		Help:        asString(im["help"]),
		Options:     asStringSlice(im["options"]),
	}
	if item.ID == "" {
		return item, fmt.Errorf("topics[%d].items[%d] is missing \"id\"", ti, ii)
	}
	if item.Label == "" {
		return item, fmt.Errorf("item %q is missing \"label\"", item.ID)
	}
	switch item.Type {
	case agent.QuestionText, agent.QuestionTextarea:
	case agent.QuestionChoice, agent.QuestionMultiSelect:
		if len(item.Options) == 0 {
			return item, fmt.Errorf("item %q of type %q requires a non-empty \"options\" array", item.ID, item.Type)
		}
	default:
		return item, fmt.Errorf("item %q has invalid type %q (want text, textarea, choice, or multiselect)", item.ID, item.Type)
	}
	return item, nil
}

// asString coerces a decoded JSON value to a string, returning "" for nil or any
// non-string (the schema already constrains these to strings; this is defensive).
func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// asBool coerces a decoded JSON value to a bool, defaulting to false.
func asBool(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

// asStringSlice coerces a decoded JSON array to []string, dropping non-string
// entries. Returns nil for a missing/empty/non-array value.
func asStringSlice(v interface{}) []string {
	raw, ok := v.([]interface{})
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
