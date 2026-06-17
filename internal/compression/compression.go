package compression

import (
	"encoding/json"
	"fmt"
	"strings"

	"gogent/internal/model"
)

// CompressionResult represents the result of compression
type CompressionResult struct {
	Content    string
	TokenCount int
	Method     string
}

// Config holds compression configuration
type Config struct {
	// ModelURL is the URL to use for compression (if not set, uses same model as session)
	ModelURL string
	// MaxCompressedTokens is the target token count after compression
	MaxCompressedTokens int
	// KeepRecentTurns is how many recent turns to keep uncompressed
	KeepRecentTurns int
}

// CompressionAgent handles context compression using model summarization
type CompressionAgent struct {
	config       *Config
	modelSession *model.ModelSession
}

// NewCompressionAgent creates a new compression agent
func NewCompressionAgent(config *Config, modelSession *model.ModelSession) *CompressionAgent {
	if config == nil {
		config = &Config{
			MaxCompressedTokens: 4000,
			KeepRecentTurns:     2,
		}
	}
	if config.MaxCompressedTokens == 0 {
		config.MaxCompressedTokens = 4000
	}
	if config.KeepRecentTurns == 0 {
		config.KeepRecentTurns = 2
	}

	return &CompressionAgent{
		config:       config,
		modelSession: modelSession,
	}
}

// SmartCompress performs intelligent context compression
func (ca *CompressionAgent) SmartCompress(messages []model.Message) (*CompressionResult, error) {
	if len(messages) <= 2 {
		// Not enough messages to compress
		return ca.buildSimpleResult(messages, "no_compression_needed")
	}

	// Keep recent turns uncompressed
	recent := ca.getLastTurns(messages, ca.config.KeepRecentTurns)
	older := messages[:len(messages)-len(recent)]

	// Compress older messages
	compressedOlder, err := ca.compressMessages(older)
	if err != nil {
		return nil, fmt.Errorf("failed to compress older messages: %w", err)
	}

	// Combine with recent turns
	result := &CompressionResult{
		Content: fmt.Sprintf("%s\n\n## Recent Activity\n%s",
			compressedOlder,
			ca.buildConversationSummary(recent)),
		Method: "smart_sequential",
	}

	// Estimate token count
	result.TokenCount = ca.estimateTokens(result.Content)

	return result, nil
}

// compressMessages compresses a list of messages using model summarization
func (ca *CompressionAgent) compressMessages(messages []model.Message) (string, error) {
	if len(messages) == 0 {
		return "", nil
	}

	prompt := ca.buildCompressionPrompt(messages)

	// Send to model for summarization
	resp, err := ca.modelSession.Send([]model.Message{
		{Role: model.RoleUser, Content: prompt},
	})
	if err != nil {
		return "", err
	}

	return resp.Content, nil
}

// buildCompressionPrompt builds a prompt for context compression
func (ca *CompressionAgent) buildCompressionPrompt(messages []model.Message) string {
	return `You are an expert at compressing conversation context while preserving important information.

## Your Task
Summarize the conversation history below into a structured format that preserves:
- Key goals and objectives
- Important decisions and why they were made
- Critical technical facts and constraints
- Next steps and pending actions
- Relevant file paths and why they matter

## Format
Output exactly the following structure. Keep every section, even when empty.
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
- Keep every section above, even when empty
- Use terse bullets, not prose paragraphs  
- Preserve exact file paths, commands, error strings, and identifiers when known
- Do not mention the summarization process
- Focus on what the model needs to know for the next turn

## Conversation History
` + ca.buildConversationSummary(messages)
}

// buildConversationSummary builds a summary of the conversation
func (ca *CompressionAgent) buildConversationSummary(messages []model.Message) string {
	var summary string
	for _, msg := range messages {
		if msg.Role == model.RoleUser {
			summary += fmt.Sprintf("[User]: %s\n", msg.Content)
		} else if msg.Role == model.RoleAssistant {
			summary += fmt.Sprintf("[Assistant]: %s\n", msg.Content)
		}
	}
	return summary
}

// getLastTurns keeps the last N user-assistant pairs
func (ca *CompressionAgent) getLastTurns(messages []model.Message, pairs int) []model.Message {
	var result []model.Message
	count := 0
	for i := len(messages) - 1; i >= 0; i-- {
		result = append([]model.Message{messages[i]}, result...)
		if messages[i].Role == model.RoleUser {
			count++
			if count >= pairs {
				break
			}
		}
	}
	return result
}

// estimateTokens estimates token count (rough heuristic: 4 chars per token)
func (ca *CompressionAgent) estimateTokens(text string) int {
	return len(text) / 4
}

// buildSimpleResult builds a simple result without compression
func (ca *CompressionAgent) buildSimpleResult(messages []model.Message, method string) (*CompressionResult, error) {
	content := ca.buildConversationSummary(messages)
	return &CompressionResult{
		Content:    content,
		TokenCount: ca.estimateTokens(content),
		Method:     method,
	}, nil
}

// ParseStructuredOutput parses the model's structured output response
func (ca *CompressionAgent) ParseStructuredOutput(content string) map[string]interface{} {
	// This is a simplified parser - in production would use proper JSON parsing
	result := make(map[string]interface{})

	// Try to extract JSON from triple backticks
	if extracted := extractJSON(content); extracted != "" {
		json.Unmarshal([]byte(extracted), &result)
	}

	return result
}

func extractJSON(text string) string {
	start := -1
	end := -1

	for i := 0; i < len(text)-2; i++ {
		if text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			if start == -1 {
				start = i + 3
			} else {
				end = i
				break
			}
		}
	}

	if start != -1 && end != -1 && end > start {
		jsonStr := text[start:end]
		if idx := strings.Index(jsonStr, "{"); idx != -1 {
			return extractJSONFrom(jsonStr[idx:])
		}
	}

	if idx := strings.Index(text, "{"); idx != -1 {
		return extractJSONFrom(text[idx:])
	}

	return ""
}

func extractJSONFrom(text string) string {
	braceCount := 0
	start := -1

	for i, ch := range text {
		if ch == '{' {
			if braceCount == 0 {
				start = i
			}
			braceCount++
		} else if ch == '}' {
			braceCount--
			if braceCount == 0 && start != -1 {
				return text[start : i+1]
			}
		}
	}

	return ""
}
