package compression

import (
	"fmt"
	"strings"

	"gogent/internal/model"
)

// DefaultKeepRecentTurns is how many trailing user→assistant turns are kept
// verbatim when compressing; older context is summarized.
const DefaultKeepRecentTurns = 3

// Config holds compression tuning. Zero values fall back to defaults.
type Config struct {
	// MaxCompressedTokens is the target size of the generated summary (advisory;
	// passed to the model as guidance).
	MaxCompressedTokens int
	// KeepRecentTurns is how many recent user→assistant turns stay uncompressed.
	KeepRecentTurns int
}

// KeepRecentTurnsOrDefault returns the configured recent-turn count or the
// built-in default when unset.
func (c *Config) KeepRecentTurnsOrDefault() int {
	if c == nil || c.KeepRecentTurns <= 0 {
		return DefaultKeepRecentTurns
	}
	return c.KeepRecentTurns
}

// CompressionAgent summarizes an older slice of a conversation into a structured
// digest. It talks to the model through a stateless model.Completer, so it never
// mutates any live session transcript (no feedback loop).
type CompressionAgent struct {
	config    *Config
	completer model.Completer
}

// NewCompressionAgent creates an agent that summarizes via the given stateless
// completer (typically the session's own model backend).
func NewCompressionAgent(config *Config, completer model.Completer) *CompressionAgent {
	return &CompressionAgent{config: config, completer: completer}
}

// Summarize turns an older slice of conversation messages into the structured
// markdown digest (Goal / Constraints / Progress / …). It issues a single
// stateless completion; the caller splices the returned text back into the
// transcript.
func (ca *CompressionAgent) Summarize(older []model.Message) (string, error) {
	if len(older) == 0 {
		return "", nil
	}
	prompt := ca.buildCompressionPrompt(older)
	resp, err := ca.completer.Complete([]model.Message{{Role: model.RoleUser, Content: prompt}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// SafeSplit partitions a transcript into an older slice to summarize and a recent
// slice to keep verbatim. It keeps the last keepRecentTurns user→assistant turns
// and never splits between an assistant tool-call message and its tool results:
// the split point is moved earlier to a clean turn boundary so the recent slice
// is always self-contained for an OpenAI-style API.
func SafeSplit(messages []model.Message, keepRecentTurns int) (older, recent []model.Message) {
	if keepRecentTurns <= 0 {
		keepRecentTurns = DefaultKeepRecentTurns
	}
	if len(messages) == 0 {
		return nil, nil
	}

	// Walk back, counting user messages as turn starts, to find where the recent
	// slice begins.
	split := len(messages)
	turns := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == model.RoleUser {
			turns++
			split = i
			if turns >= keepRecentTurns {
				break
			}
		}
	}

	// Ensure the boundary is safe: messages[split] must be a clean turn start (a
	// user message) so we never strand an assistant tool_calls message away from
	// its role:"tool" results.
	split = clampToUserBoundary(messages, split)

	if split <= 0 {
		return nil, messages
	}
	return messages[:split], messages[split:]
}

// clampToUserBoundary moves idx back to the nearest user message at or before it,
// so a split there never separates tool_calls from their tool results.
func clampToUserBoundary(messages []model.Message, idx int) int {
	if idx >= len(messages) {
		idx = len(messages) - 1
	}
	for idx > 0 && messages[idx].Role != model.RoleUser {
		idx--
	}
	return idx
}

// buildCompressionPrompt builds the structured-summary prompt for an older slice.
func (ca *CompressionAgent) buildCompressionPrompt(messages []model.Message) string {
	limit := ""
	if ca.config != nil && ca.config.MaxCompressedTokens > 0 {
		limit = fmt.Sprintf("\n- Keep the whole summary under roughly %d tokens.", ca.config.MaxCompressedTokens)
	}
	return `You are compressing earlier conversation context while preserving everything the assistant needs to continue the task.

## Format
Output exactly the following structure. Keep every section, even when empty (use "(none)").
Use terse bullets, not prose paragraphs.

### Goal
- [single-sentence task summary]

### Constraints & Preferences
- [user constraints, preferences, specs, or "(none)"]

### Progress
#### Done
- [completed work or "(none)"]

#### In Progress
- [current work or "(none)"]

#### Blocked
- [blockers or "(none)"]

### Key Decisions
- [decision and why, or "(none)"]

### Next Steps
- [ordered next actions or "(none)"]

### Critical Context
- [important technical facts, errors, open questions, or "(none)"]

### Relevant Files
- [file or directory path: why it matters, or "(none)"]

## Instructions
- Preserve exact file paths, commands, error strings, and identifiers when known.
- Do not mention the summarization process or these instructions.
- Focus on what the model needs to know for the next turn.` + limit + `

## Conversation To Summarize
` + renderConversation(messages)
}

// renderConversation renders messages as a readable transcript for summarization.
func renderConversation(messages []model.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		role := string(msg.Role)
		content := strings.TrimSpace(msg.Content)
		switch msg.Role {
		case model.RoleTool:
			fmt.Fprintf(&b, "[tool result]: %s\n", content)
		default:
			if content != "" {
				fmt.Fprintf(&b, "[%s]: %s\n", role, content)
			}
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(&b, "[%s calls %s]: %s\n", role, tc.Function.Name, tc.Function.Arguments)
		}
	}
	return b.String()
}
