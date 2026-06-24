package model

import (
	"strings"
	"testing"
)

func terminalEventFromParser(t *testing.T, parse func(chan<- StreamResponse) error) StreamResponse {
	t.Helper()
	streamCh := make(chan StreamResponse, 8)
	if err := parse(streamCh); err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	close(streamCh)
	var terminal StreamResponse
	for ev := range streamCh {
		if ev.Done {
			terminal = ev
		}
	}
	if !terminal.Done {
		t.Fatal("parser did not emit a terminal stream event")
	}
	return terminal
}

func TestParseOpenAIStreamFlagsLengthTruncatedToolArgsIssue390(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_truncated","type":"function","function":{"name":"calc","arguments":"{\"expression\":\"1+"}}]},"index":0}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"length"}]}

data: [DONE]

`

	terminal := terminalEventFromParser(t, func(streamCh chan<- StreamResponse) error {
		_, _, err := parseOpenAIStream(strings.NewReader(sse), streamCh)
		return err
	})

	if terminal.FinishReason == nil || *terminal.FinishReason != "length" {
		t.Fatalf("finish reason = %v, want length", terminal.FinishReason)
	}
	if len(terminal.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(terminal.ToolCalls), terminal.ToolCalls)
	}
	call := terminal.ToolCalls[0]
	if !call.Truncated {
		t.Fatalf("ToolCall.Truncated = false, want true for partial args %q", call.Function.Arguments)
	}
	if call.Function.Name != "calc" || call.Function.Arguments != `{"expression":"1+` {
		t.Fatalf("assembled call = %+v, want calc with partial arguments", call)
	}
}

func TestParseOpenAIStreamDoesNotFlagCompleteArgsOnLengthIssue390(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_complete","type":"function","function":{"name":"calc","arguments":"{\"expression\":\"1+1\"}"}}]},"index":0}]}

data: {"choices":[{"delta":{},"index":0,"finish_reason":"length"}]}

data: [DONE]

`

	terminal := terminalEventFromParser(t, func(streamCh chan<- StreamResponse) error {
		_, _, err := parseOpenAIStream(strings.NewReader(sse), streamCh)
		return err
	})

	if len(terminal.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(terminal.ToolCalls))
	}
	if terminal.ToolCalls[0].Truncated {
		t.Fatalf("ToolCall.Truncated = true for complete JSON args %q", terminal.ToolCalls[0].Function.Arguments)
	}
}

func TestAnthropicParseStreamFlagsLengthTruncatedToolArgsIssue390(t *testing.T) {
	const sse = `data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"calc"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"expression\":\"2+"}}

data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}

data: {"type":"message_stop"}

`

	terminal := terminalEventFromParser(t, func(streamCh chan<- StreamResponse) error {
		_, _, err := (anthropicAdapter{}).parseStream(strings.NewReader(sse), streamCh)
		return err
	})

	if terminal.FinishReason == nil || *terminal.FinishReason != "length" {
		t.Fatalf("finish reason = %v, want length", terminal.FinishReason)
	}
	if len(terminal.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(terminal.ToolCalls), terminal.ToolCalls)
	}
	call := terminal.ToolCalls[0]
	if !call.Truncated {
		t.Fatalf("ToolCall.Truncated = false, want true for partial args %q", call.Function.Arguments)
	}
	if call.ID != "toolu_1" || call.Function.Name != "calc" || call.Function.Arguments != `{"expression":"2+` {
		t.Fatalf("assembled call = %+v, want anthropic calc with partial args", call)
	}
}

func TestAnthropicParseStreamDoesNotFlagEmptyToolInputOnLengthIssue390(t *testing.T) {
	const sse = `data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_empty","name":"ping"}}

data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}

data: {"type":"message_stop"}

`

	terminal := terminalEventFromParser(t, func(streamCh chan<- StreamResponse) error {
		_, _, err := (anthropicAdapter{}).parseStream(strings.NewReader(sse), streamCh)
		return err
	})

	if len(terminal.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(terminal.ToolCalls))
	}
	call := terminal.ToolCalls[0]
	if call.Function.Arguments != "{}" {
		t.Fatalf("empty anthropic input args = %q, want {}", call.Function.Arguments)
	}
	if call.Truncated {
		t.Fatal("ToolCall.Truncated = true for empty input normalized to {}")
	}
}
