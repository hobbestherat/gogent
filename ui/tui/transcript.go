package ui

import (
	"fmt"
	"strings"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// ChatMessage is a UI-facing view of one transcript message, decoupled from the
// backend model types. It is used both to restore a session window and to render
// a sub-agent's internal monologue popup.
type ChatMessage struct {
	Role    string // "user", "assistant", "tool", "system"
	Content string
	Tool    string // tool name (for tool results, or an assistant tool call)
	Args    string // pretty-printed args for an assistant tool call
	// Reasoning is the assistant turn's retained chain-of-thought (reasoning models
	// only); it is rendered as a collapsed "thought" entry ahead of the visible
	// answer, mirroring the live appendThinkingDelta/foldLiveThought path so a
	// reasoning-only turn is not blank on reopen. Empty for ordinary turns (issue
	// #402).
	Reasoning string
}

// renderTranscript renders a list of chat messages into a foldable TextView,
// mirroring the live session rendering (user/assistant expanded; tool calls and
// results folded). It is the shared path reused by restored sessions and the
// sub-agent monologue popup.
func renderTranscript(history *tv.TextView, msgs []ChatMessage) {
	for _, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "user":
			renderRole(history, "You:", m.Content, colorUser, false)
		case "assistant":
			// Retained reasoning renders first as a collapsed "thought", matching the
			// live order (thinking, then answer) and the foldable live entry (#402).
			if strings.TrimSpace(m.Reasoning) != "" {
				renderRole(history, "thought", m.Reasoning, colorNote, true)
			}
			if strings.TrimSpace(m.Content) != "" {
				renderRole(history, "Gogent:", m.Content, colorAgent, false)
			}
			if m.Tool != "" {
				header := history.AddColored(fmt.Sprintf("tool: %s", m.Tool), colorTool)
				for _, line := range childLines(m.Args) {
					header.AddColored("  "+line, colorTool)
				}
				header.SetCollapsed(true)
			}
		case "tool":
			header := history.AddColored(fmt.Sprintf("result: %s", m.Tool), colorResult)
			for _, line := range childLines(m.Content) {
				header.AddColored("  "+line, colorResult)
			}
			header.SetCollapsed(true)
		default: // system / other
			renderRole(history, "[System]", m.Content, colorInfo, true)
		}
	}
	history.ScrollToBottom()
}

// renderRole appends a header line with its text as foldable children.
func renderRole(history *tv.TextView, header, text string, color tui.Color, collapsed bool) {
	if strings.TrimSpace(text) == "" {
		return
	}
	entry := history.AddColored(header, color)
	for _, line := range childLines(text) {
		entry.AddColored(line, color)
	}
	entry.SetCollapsed(collapsed)
}
