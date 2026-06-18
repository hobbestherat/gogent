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
	"unicode/utf8"
)

// SessionWindow is a single chat session rendered in its own window/layer.
type SessionWindow struct {
	wb          *Workbench
	id          string
	title       string
	window      *tv.Window
	layer       *tv.Layer
	history     *tv.TextView
	transcript  *transcriptModel
	input       *tv.MultiLineInput
	sendButton  *tv.Button
	modelLabel  *tv.Label
	modelSelect *tv.Select
	status      *tv.Label
	systemInstr string
	// pendingTool tracks the record created for an in-flight tool call so its
	// result can be appended to the same foldable entry when it returns.
	pendingTool *transcriptRecord
	busy        bool
	// statusState is the current left-hand status text (idle/working.../thinking
	// ... (step N)); statusStats holds the latest per-session stats snapshot.
	// refreshStatus composes the two into the single bottom status line.
	statusState string
	statusStats agent.SessionStats
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
	input := tv.NewMultiLineInput("", tv.Rect{})
	sendButton := tv.NewButton("Send", tv.Rect{}, nil)
	modelLabel := tv.NewLabel("Model", tv.Rect{})
	modelSelect := tv.NewSelect(wb.desktop, wb.modelNames, tv.Rect{})
	modelLabel.SetTarget(modelSelect)
	status := tv.NewLabel("idle", tv.Rect{})
	status.FG = colorNote
	sw.window = window
	sw.history = history
	sw.transcript = newTranscriptModel(history)
	sw.transcript.add(&transcriptRecord{
		kind:   kindSystem,
		header: "[System] " + title + " ready. Type a message and press Enter (Shift+Enter for newline).",
		color:  colorInfo,
	})
	// Intercept transcript keys (search/filter/fold) before the TextView's own
	// scroll handling, falling through to it for everything else.
	scroll := history.Component.OnTypeFn
	history.Component.OnTypeFn = func(c *tv.VisualComponent, event tui.TypeEvent) bool {
		if sw.handleTranscriptKey(event) {
			return true
		}
		if scroll != nil {
			return scroll(c, event)
		}
		return false
	}
	sw.input = input
	sw.sendButton = sendButton
	sw.modelLabel = modelLabel
	sw.modelSelect = modelSelect
	sw.status = status
	sw.statusState = "idle"
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
		// Reflow the status line to the new width so its stats truncate/expand
		// with the window on resize.
		sw.refreshStatus()
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
		sw.statusState = "working..."
		sw.status.FG = colorInfo
		sw.sendButton.SetLabel("...")
	} else {
		sw.statusState = "idle"
		sw.status.FG = colorNote
		sw.sendButton.SetLabel("Send")
	}
	sw.refreshStatus()
}

// refreshStatus rebuilds the bottom status line from the current state text and
// per-session stats, sized to the label's current width so it truncates
// gracefully on narrow windows.
func (sw *SessionWindow) refreshStatus() {
	sw.status.SetText(formatStatusLine(sw.statusState, sw.statusStats, sw.status.Component.Bounds.W))
}

// apply renders a single backend session event into the transcript.
func (sw *SessionWindow) apply(ev agent.SessionEvent) {
	switch ev.Type {
	case agent.SessionEventThinking:
		sw.statusState = fmt.Sprintf("thinking... (step %d)", ev.Step)
		sw.status.FG = colorInfo
		sw.refreshStatus()
	case agent.SessionEventUsage:
		sw.statusStats = ev.Stats
		sw.refreshStatus()
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

// styledChildLines splits text into foldable child lines sharing one colour.
func styledChildLines(text string, color tui.Color) []styledLine {
	lines := childLines(text)
	out := make([]styledLine, len(lines))
	for i, line := range lines {
		out[i] = styledLine{text: line, color: color}
	}
	return out
}

// addUser appends the user's message.
func (sw *SessionWindow) addUser(text string) {
	sw.transcript.add(&transcriptRecord{
		kind: kindUser, header: "You:", color: colorUser,
		lines: styledChildLines(text, colorUser),
	})
}

// addAssistant appends the assistant's final answer (expanded, not folded).
func (sw *SessionWindow) addAssistant(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	sw.transcript.add(&transcriptRecord{
		kind: kindAssistant, header: "Gogent:", color: colorAgent,
		lines: styledChildLines(text, colorAgent),
	})
}

// addThought appends a collapsed-by-default "thought" entry.
func (sw *SessionWindow) addThought(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	sw.transcript.add(&transcriptRecord{
		kind: kindThinking, header: "thought", color: colorNote, collapsed: true,
		lines: styledChildLines(text, colorNote),
	})
}

// addCompaction appends a collapsed note recording a context-compression pass;
// the structured summary is folded inside.
func (sw *SessionWindow) addCompaction(estTokens int, digest string) {
	sw.transcript.add(&transcriptRecord{
		kind:      kindCompaction,
		header:    fmt.Sprintf("context compacted (~%d tokens)", estTokens),
		color:     colorNote,
		collapsed: true,
		lines:     styledChildLines(digest, colorNote),
	})
}

// beginToolCall creates a collapsed entry for a tool call, holding its args.
func (sw *SessionWindow) beginToolCall(name string, args map[string]interface{}) {
	lines := []styledLine{{text: "args:", color: colorTool}}
	for _, line := range formatArgs(args) {
		lines = append(lines, styledLine{text: "  " + line, color: colorTool})
	}
	sw.pendingTool = sw.transcript.add(&transcriptRecord{
		kind: kindTool, header: fmt.Sprintf("tool: %s (running...)", name),
		color: colorTool, collapsed: true, lines: lines,
	})
}

// finishToolCall appends the result to the pending tool entry (or a fresh one).
func (sw *SessionWindow) finishToolCall(name, result string) {
	rec := sw.pendingTool
	if rec == nil {
		rec = sw.transcript.add(&transcriptRecord{
			kind: kindTool, header: fmt.Sprintf("tool: %s", name), color: colorTool, collapsed: true,
		})
	} else {
		sw.transcript.setHeader(rec, fmt.Sprintf("tool: %s (done)", name))
	}
	sw.transcript.appendLine(rec, styledLine{text: "result:", color: colorResult})
	for _, line := range childLines(result) {
		sw.transcript.appendLine(rec, styledLine{text: "  " + line, color: colorResult})
	}
	sw.transcript.setCollapsed(rec, true)
	sw.pendingTool = nil
}

// addError appends a red error line.
func (sw *SessionWindow) addError(text string) {
	lines := make([]styledLine, 0)
	for _, line := range childLines(text) {
		lines = append(lines, styledLine{text: "  " + line, color: colorError})
	}
	sw.transcript.add(&transcriptRecord{
		kind: kindError, header: "error:", color: colorError, lines: lines,
	})
}

// handleTranscriptKey implements the less/vim-style transcript controls while the
// history view is focused: '/' to search, single letters to toggle event-type
// filters, 'f'/'u' to fold/unfold all, and Esc to clear an active filter/search.
// It returns false (letting the TextView scroll) when nothing applies.
func (sw *SessionWindow) handleTranscriptKey(event tui.TypeEvent) bool {
	if event.Ctrl || event.Alt {
		return false
	}
	switch event.Key {
	case tui.KeyEscape:
		if sw.transcript.filtering() {
			sw.transcript.showAll()
			return true
		}
		return false
	case tui.KeyRune:
		switch event.Rune {
		case '/':
			sw.promptFind()
		case 'a':
			sw.transcript.toggleKind(kindAssistant)
		case 't':
			sw.transcript.toggleKind(kindTool)
		case 'r':
			sw.transcript.toggleKind(kindThinking)
		case 'e':
			sw.transcript.toggleKind(kindError)
		case 'f':
			sw.transcript.setFold(true)
		case 'u':
			sw.transcript.setFold(false)
		default:
			return false
		}
		return true
	}
	return false
}

// promptFind opens the search prompt and applies the entered query as a
// find-in-transcript filter (an empty query clears the search).
func (sw *SessionWindow) promptFind() {
	sw.wb.showInputDialog("Find in Transcript", "&Search:", sw.transcript.query, func(value string, ok bool) {
		if !ok {
			return
		}
		sw.transcript.setQuery(value)
		sw.wb.desktop.Redraw()
	})
}

// restore replays a saved transcript into the model so a re-opened session is
// searchable and filterable like a live one. It mirrors renderTranscript's
// role-to-entry mapping.
func (sw *SessionWindow) restore(msgs []ChatMessage) {
	for _, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "user":
			if strings.TrimSpace(m.Content) != "" {
				sw.addUser(m.Content)
			}
		case "assistant":
			sw.addAssistant(m.Content)
			if m.Tool != "" {
				lines := make([]styledLine, 0)
				for _, line := range childLines(m.Args) {
					lines = append(lines, styledLine{text: "  " + line, color: colorTool})
				}
				sw.transcript.add(&transcriptRecord{
					kind: kindTool, header: fmt.Sprintf("tool: %s", m.Tool),
					color: colorTool, collapsed: true, lines: lines,
				})
			}
		case "tool":
			lines := make([]styledLine, 0)
			for _, line := range childLines(m.Content) {
				lines = append(lines, styledLine{text: "  " + line, color: colorResult})
			}
			sw.transcript.add(&transcriptRecord{
				kind: kindTool, header: fmt.Sprintf("result: %s", m.Tool),
				color: colorResult, collapsed: true, lines: lines,
			})
		default: // system / other
			if strings.TrimSpace(m.Content) != "" {
				sw.transcript.add(&transcriptRecord{
					kind: kindSystem, header: "[System]", color: colorInfo, collapsed: true,
					lines: styledChildLines(m.Content, colorInfo),
				})
			}
		}
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

// statusSep joins the state and each stat segment in the status line.
const statusSep = " · "

// formatStatusLine composes the bottom status line for a session window:
//
//	‹state› · <in>/<out> tok · <n> turns · ctx <pct>%
//
// The state sits on the left with the compact stats following it. Segments with
// no meaningful value yet (zero tokens/turns, or an unknown context window) are
// omitted, so a fresh, idle session shows just its state. On narrow windows the
// right-most stat segments are dropped first — the state (the most important
// signal) always stays visible — and as a last resort the state itself is
// truncated to width. Width is measured in display cells; the separator is a
// single-cell middle dot.
func formatStatusLine(state string, stats agent.SessionStats, width int) string {
	state = strings.TrimSpace(state)
	if width <= 0 {
		return state
	}
	line := state
	if runeLen(line) > width {
		return truncateRunes(line, width)
	}
	for _, seg := range statusSegments(stats) {
		add := statusSep + seg
		if runeLen(line)+runeLen(add) <= width {
			line += add
		} else {
			break
		}
	}
	return line
}

// statusSegments renders the compact stat segments in display order. Zero-value
// segments are omitted.
func statusSegments(stats agent.SessionStats) []string {
	var segs []string
	if stats.TokensIn > 0 || stats.TokensOut > 0 {
		segs = append(segs, formatTokens(stats.TokensIn)+"/"+formatTokens(stats.TokensOut)+" tok")
	}
	if stats.Turns > 0 {
		segs = append(segs, fmt.Sprintf("%d turns", stats.Turns))
	}
	if stats.ContextWindow > 0 {
		segs = append(segs, fmt.Sprintf("ctx %d%%", contextPercent(stats.ContextTokens, stats.ContextWindow)))
	}
	return segs
}

// formatTokens renders a token count compactly with k/M suffixes (e.g. 12300 ->
// "12.3k"). Values under a thousand are shown verbatim.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// contextPercent returns context usage as a whole percentage of the window,
// clamped to [0, 100]. A non-positive window yields 0.
func contextPercent(tokens, window int) int {
	if window <= 0 {
		return 0
	}
	if p := tokens * 100 / window; p < 100 {
		return p
	}
	return 100
}

// runeLen returns the number of display cells (runes) in s.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// truncateRunes returns the first max runes of s, for the rare very-narrow
// window where even the state does not fit.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
