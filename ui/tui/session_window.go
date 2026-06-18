package ui

import (
	"encoding/json"
	"fmt"
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
	"gogent/internal/config"
	"sort"
	"strings"
)

// SessionWindow is a single chat session rendered in its own window/layer.
type SessionWindow struct {
	wb          *Workbench
	id          string
	title       string
	window      *tv.Window
	layer       *tv.Layer
	history     *tv.TextView
	input       *tv.MultiLineInput
	sendButton  *tv.Button
	modelLabel  *tv.Label
	modelSelect *tv.Select
	status      *tv.Label
	systemInstr string
	// pendingTool tracks the foldable entry created for an in-flight tool call so
	// its result can be appended as a child when it returns.
	pendingTool *tv.TextEntry
	busy        bool
}

// newSessionWindow builds the window, its widgets and their layout/handlers.
func newSessionWindow(wb *Workbench, id, title string, bounds tv.Rect) *SessionWindow {
	sw := &SessionWindow{wb: wb, id: id, title: title}
	window := tv.NewWindow(title, bounds, tui.LineSingle)
	
	// Enable scalable windows using turbotv options
	window.Resizable = wb.windowConfig.Resizable
	window.Minimizable = wb.windowConfig.Minimizable
	window.MinWidth = wb.windowConfig.MinWidth
	window.MinHeight = wb.windowConfig.MinHeight
	
	window.OnClose = func(_ *tv.Window) { wb.CloseSession(id) }
	history := tv.NewTextView("", tv.Rect{})
	history.Wrap = true
	history.AddColored("[System] "+title+" ready. Type a message and press Enter (Shift+Enter for newline).", colorInfo)
	input := tv.NewMultiLineInput("", tv.Rect{})
	sendButton := tv.NewButton("Send", tv.Rect{}, nil)
	modelLabel := tv.NewLabel("Model", tv.Rect{})
	modelSelect := tv.NewSelect(wb.desktop, wb.modelNames, tv.Rect{})
	modelLabel.SetTarget(modelSelect)
	status := tv.NewLabel("idle", tv.Rect{})
	status.FG = colorNote
	sw.window = window
	sw.history = history
	sw.input = input
	sw.sendButton = sendButton
	sw.modelLabel = modelLabel
	sw.modelSelect = modelSelect
	sw.status = status
	window.AddContent(history)
	window.AddContent(input)
	window.AddContent(sendButton)
	window.AddContent(modelLabel)
	window.AddContent(modelSelect)
	window.AddContent(status)
	window.Content.LayoutFn = func(c *tv.VisualComponent) {
		wd := c.Bounds.W
		ht := c.Bounds.H
		if wd < 4 || ht < 6 {
			return
		}
		inputH := 3
		selW := 24
		if selW > wd-9 {
			selW = wd - 9
		}
		modelLabel.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: 6, H: 1})
		modelSelect.Component.SetBounds(tv.Rect{X: 7, Y: 0, W: selW, H: 1})
		history.Component.SetBounds(tv.Rect{X: 0, Y: 1, W: wd, H: ht - inputH - 2})
		status.Component.SetBounds(tv.Rect{X: 0, Y: ht - inputH - 1, W: wd, H: 1})
		input.Component.SetBounds(tv.Rect{X: 0, Y: ht - inputH, W: wd - 10, H: inputH})
		sendButton.Component.SetBounds(tv.Rect{X: wd - 9, Y: ht - inputH, W: 8, H: 1})
	}
	submit := func() {
		text := strings.TrimSpace(input.GetText())
		if text == "" || sw.busy {
			return
		}
		input.Clear()
		sw.addUser(text)
		sw.setBusy(true)
		modelName := sw.selectedModelName()
		if wb.handlers.OnSend != nil {
			go wb.handlers.OnSend(sw.id, text, modelName)
		}
	}
	sendButton.OnPress = submit
	input.OnSubmit = submit
	sw.layer = tv.NewWindowLayer("layer-"+id, window)
	return sw
}

// selectedModelName returns the backend model identifier for the current select.
// The unique config Name is preferred so distinct endpoints sharing the same
// underlying model id can still be selected individually.
func (sw *SessionWindow) selectedModelName() string {
	cfg := sw.selectedModelConfig()
	if cfg != nil {
		if cfg.Name != "" {
			return cfg.Name
		}
		return cfg.Model
	}
	return ""
}
func (sw *SessionWindow) selectedModelConfig() *config.ModelConfig {
	return sw.wb.modelByDisplayName(sw.modelSelect.Value())
}

// setBusy updates the status line and busy flag.
func (sw *SessionWindow) setBusy(busy bool) {
	sw.busy = busy
	if busy {
		sw.status.SetText("working...")
		sw.status.FG = colorInfo
		sw.sendButton.SetLabel("...")
	} else {
		sw.status.SetText("idle")
		sw.status.FG = colorNote
		sw.sendButton.SetLabel("Send")
	}
}

// apply renders a single backend session event into the transcript.
func (sw *SessionWindow) apply(ev agent.SessionEvent) {
	switch ev.Type {
	case agent.SessionEventThinking:
		sw.status.SetText(fmt.Sprintf("thinking... (step %d)", ev.Step))
		sw.status.FG = colorInfo
	case agent.SessionEventAssistantStep:
		sw.addThought(ev.Text)
	case agent.SessionEventToolCall:
		sw.beginToolCall(ev.Tool, ev.Args)
	case agent.SessionEventToolResult:
		sw.finishToolCall(ev.Tool, ev.Result)
	case agent.SessionEventFinal:
		sw.addAssistant(ev.Text)
		sw.setBusy(false)
	case agent.SessionEventCompaction:
		sw.addCompaction(ev.Step, ev.Text)
	case agent.SessionEventError:
		if ev.Err != nil {
			sw.addError(ev.Err.Error())
		}
		sw.setBusy(false)
	}
}

// addUser appends the user's message.
func (sw *SessionWindow) addUser(text string) {
	header := sw.history.AddColored("You:", colorUser)
	for _, line := range childLines(text) {
		header.AddColored(line, colorUser)
	}
}

// addAssistant appends the assistant's final answer (expanded, not folded).
func (sw *SessionWindow) addAssistant(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	header := sw.history.AddColored("Gogent:", colorAgent)
	for _, line := range childLines(text) {
		header.AddColored(line, colorAgent)
	}
	header.SetCollapsed(false)
}

// addThought appends a collapsed-by-default "thought" entry.
func (sw *SessionWindow) addThought(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	header := sw.history.AddColored("thought", colorNote)
	for _, line := range childLines(text) {
		header.AddColored(line, colorNote)
	}
	header.SetCollapsed(true)
}

// addCompaction appends a collapsed note recording a context-compression pass;
// the structured summary is folded inside.
func (sw *SessionWindow) addCompaction(estTokens int, digest string) {
	header := sw.history.AddColored(fmt.Sprintf("context compacted (~%d tokens)", estTokens), colorNote)
	for _, line := range childLines(digest) {
		header.AddColored(line, colorNote)
	}
	header.SetCollapsed(true)
}

// beginToolCall creates a collapsed entry for a tool call, holding its args.
func (sw *SessionWindow) beginToolCall(name string, args map[string]interface{}) {
	header := sw.history.AddColored(fmt.Sprintf("tool: %s (running...)", name), colorTool)
	header.AddColored("args:", colorTool)
	for _, line := range formatArgs(args) {
		header.AddColored("  "+line, colorTool)
	}
	header.SetCollapsed(true)
	sw.pendingTool = header
}

// finishToolCall appends the result to the pending tool entry (or a fresh one).
func (sw *SessionWindow) finishToolCall(name, result string) {
	header := sw.pendingTool
	if header == nil {
		header = sw.history.AddColored(fmt.Sprintf("tool: %s", name), colorTool)
	} else {
		header.SetText(fmt.Sprintf("tool: %s (done)", name))
	}
	header.AddColored("result:", colorResult)
	for _, line := range childLines(result) {
		header.AddColored("  "+line, colorResult)
	}
	header.SetCollapsed(true)
	sw.pendingTool = nil
}

// addError appends a red error line.
func (sw *SessionWindow) addError(text string) {
	header := sw.history.AddColored("error:", colorError)
	for _, line := range childLines(text) {
		header.AddColored("  "+line, colorError)
	}
}

// formatArgs renders tool arguments as readable key/value lines.
func formatArgs(args map[string]interface{}) []string {
	if len(args) == 0 {
		return []string{"(none)"}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		val := args[k]
		var rendered string
		switch v := val.(type) {
		case string:
			rendered = v
		default:
			if b, err := json.Marshal(v); err == nil {
				rendered = string(b)
			} else {
				rendered = fmt.Sprintf("%v", v)
			}
		}
		for i, line := range childLines(rendered) {
			if i == 0 {
				out = append(out, fmt.Sprintf("%s: %s", k, line))
			} else {
				out = append(out, "  "+line)
			}
		}
	}
	return out
}
