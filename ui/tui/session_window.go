package ui

import (
	"encoding/json"
	"fmt"
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
	"gogent/internal/agent"
	"gogent/internal/config"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// readOnly marks a static analysis window opened from the Sessions browser
	// (issue #58): it renders a saved transcript with the full search/filter/
	// fold/yank toolkit but has no input, model selector or live backend session,
	// so several can sit open side-by-side for comparison without cost. Its id is
	// an "analysis-N" synthetic (never a backend session id), it is excluded from
	// the persisted layout, and closing it tears down no backend session.
	readOnly bool
	// pendingTool tracks the record created for an in-flight tool call so its
	// result can be appended to the same foldable entry when it returns.
	pendingTool *transcriptRecord
	busy        bool
	// statusState is the current left-hand status text (idle/working.../thinking
	// ... (step N)); statusStats holds the latest per-session stats snapshot.
	// refreshStatus composes the two into the single bottom status line.
	statusState string
	statusStats agent.SessionStats
	// turnStart / turnStartOut anchor the live elapsed timer and output
	// throughput shown while a turn is generating. turnStart is the zero time
	// when the session is idle. budgetAlerted latches the one-time "budget
	// exceeded" transcript note so it fires once per breach (cumulative token
	// usage is monotonic, so a breach never self-clears).
	turnStart     time.Time
	turnStartOut  int
	budgetAlerted bool
}

// newSessionWindow builds the window, its widgets and their layout/handlers. A
// readOnly window (opened from the Sessions browser, issue #58) omits the input,
// model selector and status line and gives the transcript the full height; a
// live window wires the full send/model/status chrome.
func newSessionWindow(wb *Workbench, id, title string, bounds tv.Rect, readOnly bool) *SessionWindow {
	sw := &SessionWindow{wb: wb, id: id, title: title, readOnly: readOnly}
	displayTitle := title
	if readOnly {
		displayTitle = title + " (analysis)"
	}
	window := tv.NewWindow(displayTitle, bounds, tui.LineSingle)

	// Enable scalable windows using turbotv options
	window.Resizable = wb.windowConfig.Resizable
	window.Minimizable = wb.windowConfig.Minimizable
	window.MinWidth = wb.windowConfig.MinWidth
	window.MinHeight = wb.windowConfig.MinHeight

	window.OnClose = func(_ *tv.Window) { wb.CloseSession(id) }
	history := tv.NewTextView("", tv.Rect{})
	history.Wrap = true
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

	if readOnly {
		// Analysis window: transcript only, no input/model/status chrome.
		window.AddContent(history)
		window.Content.LayoutFn = func(c *tv.VisualComponent) {
			wd := c.Bounds.W
			ht := c.Bounds.H
			if wd < 4 || ht < 4 {
				return
			}
			history.Component.SetBounds(tv.Rect{X: 0, Y: 0, W: wd, H: ht})
		}
		sw.layer = tv.NewWindowLayer("layer-"+id, window)
		return sw
	}

	input := tv.NewMultiLineInput("", tv.Rect{})
	sendButton := tv.NewButton("Send", tv.Rect{}, nil)
	modelLabel := tv.NewLabel("Model", tv.Rect{})
	modelSelect := tv.NewSelect(wb.desktop, wb.modelNames, tv.Rect{})
	modelLabel.SetTarget(modelSelect)
	status := tv.NewLabel("idle", tv.Rect{})
	status.FG = colorNote
	sw.input = input
	sw.sendButton = sendButton
	sw.modelLabel = modelLabel
	sw.modelSelect = modelSelect
	// A model change in the focused session moves the Overall panel's "model"/"api"
	// rows (issue #107); coalesce the refresh rather than paying for one per pick.
	modelSelect.OnChange = func(int) { wb.scheduleOverallRefresh() }
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
		selW := headerSelectWidth(sw.wb.longestModelNameWidth(), wd)
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
		// A leading slash is a client-side command (/undo, /rewind) handled
		// locally rather than sent to the model (issue #41).
		if sw.handleSlashCommand(text) {
			input.Clear()
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

// setBusy updates the status line and busy flag, anchoring the live elapsed
// timer to the turn's start (and clearing it when the turn ends). The status
// colour is left to refreshStatus, which folds in the context/budget severity.
func (sw *SessionWindow) setBusy(busy bool) {
	sw.busy = busy
	if busy {
		sw.statusState = "working..."
		sw.sendButton.SetLabel("...")
		sw.turnStart = time.Now()
		sw.turnStartOut = sw.statusStats.TokensOut
	} else {
		sw.statusState = "idle"
		sw.sendButton.SetLabel("Send")
		sw.turnStart = time.Time{}
		sw.turnStartOut = 0
	}
	sw.refreshStatus()
}

// refreshStatus rebuilds the bottom status line from the current state text,
// per-session stats and the live elapsed/throughput figures, sized to the
// label's current width so it truncates gracefully on narrow windows. It also
// sets the status colour (severity over idle/working) and raises the one-time
// budget-exceeded transcript note on the threshold crossing.
func (sw *SessionWindow) refreshStatus() {
	budget := sw.wb.budgetConfig()
	live := sw.liveStats()
	sw.status.FG = statusColor(!sw.busy, sw.statusStats, budget)
	sw.status.SetText(formatStatusLine(sw.statusState, sw.statusStats, live, budget, sw.status.Component.Bounds.W))
	sw.alertBudgetIfNewlyExceeded(budget)
}

// liveStats computes the transient generation-time figures (elapsed since the
// turn started and the output-token throughput) shown only while a turn is in
// flight. It returns a zero value when idle or before the turn has started.
func (sw *SessionWindow) liveStats() liveStats {
	if !sw.busy || sw.turnStart.IsZero() {
		return liveStats{}
	}
	elapsed := time.Since(sw.turnStart)
	out := liveStats{elapsed: elapsed}
	if produced := sw.statusStats.TokensOut - sw.turnStartOut; elapsed > 0 && produced > 0 {
		out.tokensPerSec = float64(produced) / elapsed.Seconds()
	}
	return out
}

// alertBudgetIfNewlyExceeded records a one-line transcript note the first time a
// session's cumulative token usage crosses its configured budget, and clears the
// latch if usage ever drops back below it (it cannot for cumulative tokens, but
// the guard keeps the logic honest if the budget is later lowered).
func (sw *SessionWindow) alertBudgetIfNewlyExceeded(budget config.BudgetConfig) {
	exceeded := budgetStatus(sw.statusStats, budget) == budgetExceeded
	switch {
	case exceeded && !sw.budgetAlerted:
		sw.budgetAlerted = true
		used := sw.statusStats.TokensIn + sw.statusStats.TokensOut
		sw.transcript.add(&transcriptRecord{
			kind:   kindSystem,
			header: "[Budget] token budget exceeded",
			color:  colorError,
			lines: styledChildLines(
				fmt.Sprintf("Cumulative usage %d tok reached the configured budget of %d tok.", used, budget.TokenBudget),
				colorError),
		})
	case !exceeded:
		sw.budgetAlerted = false
	}
}

// apply renders a single backend session event into the transcript.
func (sw *SessionWindow) apply(ev agent.SessionEvent) {
	switch ev.Type {
	case agent.SessionEventThinking:
		sw.statusState = fmt.Sprintf("thinking... (step %d)", ev.Step)
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

// addNote appends a one-line system note to the transcript, used to echo
// client-side command feedback.
func (sw *SessionWindow) addNote(text string) {
	sw.transcript.add(&transcriptRecord{
		kind:   kindSystem,
		header: "[System]",
		color:  colorInfo,
		lines:  styledChildLines(text, colorInfo),
	})
}

// handleSlashCommand interprets a leading "/..." input as a client-side command.
// It returns true when the command was recognized and handled (the caller clears
// the input without sending anything to the model), and false when the input is
// not a recognized command and should be sent as a normal message.
func (sw *SessionWindow) handleSlashCommand(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	fields := strings.Fields(text)
	switch fields[0] {
	case "/undo":
		summary, err := sw.callUndo(false, 0)
		sw.echoCommand("/undo", summary, err)
		return true
	case "/rewind":
		turns := 0 // 0 => revert every recorded turn
		if len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				turns = n
			} else {
				sw.echoCommand("/rewind", "", fmt.Errorf("usage: /rewind [turns]"))
				return true
			}
		}
		summary, err := sw.callUndo(true, turns)
		sw.echoCommand("/rewind", summary, err)
		return true
	}
	return false
}

// callUndo invokes the backend undo/rewind handler. rewind selects /rewind (the
// last turns turns, 0 = all) over /undo (the single last turn).
func (sw *SessionWindow) callUndo(rewind bool, turns int) (string, error) {
	if rewind {
		if sw.wb.handlers.OnRewind != nil {
			return sw.wb.handlers.OnRewind(sw.id, turns)
		}
	} else if sw.wb.handlers.OnUndo != nil {
		return sw.wb.handlers.OnUndo(sw.id)
	}
	return "", fmt.Errorf("undo/rewind not available")
}

// echoCommand surfaces a command's outcome as a transcript note.
func (sw *SessionWindow) echoCommand(cmd, summary string, err error) {
	if err != nil {
		sw.addNote(fmt.Sprintf("%s failed: %v", cmd, err))
		return
	}
	if summary == "" {
		summary = "done"
	}
	sw.addNote(fmt.Sprintf("%s — %s", cmd, summary))
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
// filters, 'f'/'u' to fold/unfold all, 'y' to yank the last answer, and Esc to
// clear an active filter/search. It returns false (letting the TextView scroll)
// when nothing applies.
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
		case 'y':
			sw.copyLastAnswer()
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

// copyLastAnswer yanks the most recent assistant answer to the system clipboard
// (issue #62). It is also bound to the 'y' key while the transcript is focused
// (vim-style yank). A transcript note confirms the copy, or reports when there is
// nothing yet to copy.
func (sw *SessionWindow) copyLastAnswer() {
	text := sw.transcript.lastAssistantRecord().body()
	if strings.TrimSpace(text) == "" {
		sw.addNote("no answer to copy yet")
		return
	}
	sw.wb.copyToClipboard(text)
	sw.addNote(fmt.Sprintf("copied last answer (%d chars) to clipboard", utf8.RuneCountInString(text)))
}

// copyLastCode yanks the fenced code blocks from the most recent assistant answer
// to the system clipboard (issue #62). When the answer has no fenced code it
// reports that rather than copying the prose.
func (sw *SessionWindow) copyLastCode() {
	code := extractFencedCode(sw.transcript.lastAssistantRecord().body())
	if strings.TrimSpace(code) == "" {
		sw.addNote("no code block in last answer")
		return
	}
	sw.wb.copyToClipboard(code)
	sw.addNote(fmt.Sprintf("copied code (%d chars) to clipboard", utf8.RuneCountInString(code)))
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

// gaugeCells is the width of the context-usage bar in display cells (e.g.
// "▰▰▰▱▱▱").
const gaugeCells = 6

// Context-usage thresholds for the status gauge's colour, expressed as
// percentages of the configured context window. contextCriticalPct intentionally
// matches the model session's compaction high-water mark (80%): the gauge turns
// red exactly when a compaction pass is about to fire, giving a visual warning
// just before context is compressed (issues #4 / #63).
const (
	contextWarnPct     = 60 // amber: approaching the compaction threshold
	contextCriticalPct = 80 // red: at/over the compaction threshold
)

// liveStats carries the transient, generation-time figures shown only while a
// turn is in flight: the elapsed time since the turn started and the output-token
// throughput. A zero liveStats renders no live segments.
type liveStats struct {
	elapsed      time.Duration
	tokensPerSec float64
}

// budgetLevel summarises how close a session's cumulative token usage is to its
// configured budget, for colouring and alerting the status line.
type budgetLevel int

const (
	budgetOK          budgetLevel = iota
	budgetApproaching             // >= the warn fraction of the budget
	budgetExceeded                // >= the full budget
)

// formatStatusLine composes the bottom status line for a session window:
//
//	‹state› · [budget!] · ‹elapsed› · ‹N t/s› · <in>/<out> tok · <n> turns · ctx ▰▰▱▱▱▱ <pct>%
//
// The state sits on the left with the compact stats following it. Segments with
// no meaningful value yet (no turn in flight, zero tokens/turns, or an unknown
// context window) are omitted, so a fresh, idle session shows just its state. On
// narrow windows the right-most stat segments are dropped first — the state (the
// most important signal) always stays visible — and as a last resort the state
// itself is truncated to width. Width is measured in display cells; the
// separator is a single-cell middle dot.
func formatStatusLine(state string, stats agent.SessionStats, live liveStats, budget config.BudgetConfig, width int) string {
	state = strings.TrimSpace(state)
	if width <= 0 {
		return state
	}
	line := state
	if runeLen(line) > width {
		return truncateRunes(line, width)
	}
	for _, seg := range statusSegments(stats, live, budget) {
		add := statusSep + seg
		if runeLen(line)+runeLen(add) <= width {
			line += add
		} else {
			break
		}
	}
	return line
}

// statusSegments renders the compact stat segments in display order: a budget
// breach marker and the live elapsed/throughput first (so they survive longest
// when the line is narrowed), then the cumulative token/turn/context figures.
// Zero-value segments are omitted.
func statusSegments(stats agent.SessionStats, live liveStats, budget config.BudgetConfig) []string {
	var segs []string
	if budgetStatus(stats, budget) == budgetExceeded {
		segs = append(segs, "budget!")
	}
	if d := formatDuration(live.elapsed); d != "" {
		segs = append(segs, d)
		if tps := formatTokensPerSec(live.tokensPerSec); tps != "" {
			segs = append(segs, tps)
		}
	}
	if stats.TokensIn > 0 || stats.TokensOut > 0 {
		segs = append(segs, formatTokens(stats.TokensIn)+"/"+formatTokens(stats.TokensOut)+" tok")
	}
	if stats.Turns > 0 {
		segs = append(segs, fmt.Sprintf("%d turns", stats.Turns))
	}
	if stats.ContextWindow > 0 {
		segs = append(segs, contextSegment(stats))
	}
	return segs
}

// contextSegment renders the context-usage segment as a bar plus percentage,
// e.g. "ctx ▰▰▱▱▱▱ 38%".
func contextSegment(stats agent.SessionStats) string {
	return fmt.Sprintf("ctx %s %d%%",
		contextGauge(stats.ContextTokens, stats.ContextWindow),
		contextPercent(stats.ContextTokens, stats.ContextWindow))
}

// contextGauge renders the context-usage bar (filled "▰" then empty "▱") scaled
// to gaugeCells. Usage at or over the window fills every cell; any nonzero usage
// fills at least one cell (so a little usage is visible rather than reading as
// empty); an unknown window (<=0) or zero usage yields an all-empty bar.
func contextGauge(tokens, window int) string {
	filled := 0
	if window > 0 && tokens > 0 {
		filled = (tokens*gaugeCells + window/2) / window // rounded to nearest cell
		if filled > gaugeCells {
			filled = gaugeCells
		}
		if filled < 0 {
			filled = 0
		}
		if filled == 0 { // nonzero usage should show at least one cell
			filled = 1
		}
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", gaugeCells-filled)
}

// statusColor picks the status line's foreground colour. Severity wins over the
// idle/working state: a budget breach or context at the compaction threshold
// turns the whole line red, an approaching budget or context turns it amber, and
// otherwise it follows the state (dim grey when idle, blue when working). The
// colour is the at-a-glance warning the issue asks for — it stays even when a
// narrow line drops the text that explains it.
func statusColor(idle bool, stats agent.SessionStats, budget config.BudgetConfig) tui.Color {
	switch budgetStatus(stats, budget) {
	case budgetExceeded:
		return colorError
	case budgetApproaching:
		return colorTool
	}
	pct := contextPercent(stats.ContextTokens, stats.ContextWindow)
	if pct >= contextCriticalPct {
		return colorError
	}
	if pct >= contextWarnPct {
		return colorTool
	}
	if idle {
		return colorNote
	}
	return colorInfo
}

// budgetStatus classifies a session's cumulative token usage against the
// configured budget. A disabled budget (TokenBudget <= 0) always reports OK.
func budgetStatus(stats agent.SessionStats, budget config.BudgetConfig) budgetLevel {
	if budget.TokenBudget <= 0 {
		return budgetOK
	}
	used := stats.TokensIn + stats.TokensOut
	if used >= budget.TokenBudget {
		return budgetExceeded
	}
	if float64(used) >= budget.WarnFractionOrDefault()*float64(budget.TokenBudget) {
		return budgetApproaching
	}
	return budgetOK
}

// formatDuration renders an elapsed duration compactly for the status line:
// under a minute as "Ns", a minute or more as "MmSSs" (e.g. "1m03s"). A
// non-positive duration yields "" so callers can omit it.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// formatTokensPerSec renders an output-throughput figure as "N t/s" (rounded to
// a whole token). Throughput under one token per second renders "<1 t/s"; a
// non-positive figure yields "" so callers can omit it.
func formatTokensPerSec(tps float64) string {
	if tps <= 0 {
		return ""
	}
	n := int(tps + 0.5)
	if n < 1 {
		return "<1 t/s"
	}
	return fmt.Sprintf("%d t/s", n)
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

// headerSelectWidth sizes the window-header model dropdown (issue #108). It grows
// to fit the longest model name plus two cells for the Select's value padding and
// ▼ glyph, never shrinking below a sensible minimum, and is clamped to the room
// available in the window (leaving space for the "Model" label and a small right
// margin). The caller's window-width guard guarantees windowWidth >= 4, so the
// clamp keeps the control at least one cell wide.
func headerSelectWidth(longestName, windowWidth int) int {
	const minW = 24
	w := minW
	if want := longestName + 2; want > w {
		w = want
	}
	if max := windowWidth - 9; w > max {
		w = max
	}
	if w < 1 {
		w = 1
	}
	return w
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
