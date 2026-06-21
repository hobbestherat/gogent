package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamThinkingDefaultsOff: the streaming-thinking option must default to
// off so a config that does not mention it keeps the prior behaviour (issue
// #217, opt-in).
func TestStreamThinkingDefaultsOff(t *testing.T) {
	var ex ExperimentalConfig
	if ex.StreamThinking {
		t.Error("ExperimentalConfig.StreamThinking must default to false")
	}
	// A config without the key keeps it off after decoding.
	var cfg Config
	if err := json.Unmarshal([]byte(`{}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Experimental.StreamThinking {
		t.Error("absent stream_thinking key must decode to false")
	}
}

// TestStreamThinkingJSONRoundTrip: the stream_thinking key round-trips through
// JSON and maps onto ExperimentalConfig.StreamThinking in both directions.
func TestStreamThinkingJSONRoundTrip(t *testing.T) {
	const on = `{"stream_thinking":true}`
	var ex ExperimentalConfig
	if err := json.Unmarshal([]byte(on), &ex); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !ex.StreamThinking {
		t.Error("stream_thinking:true did not set ExperimentalConfig.StreamThinking")
	}

	// Marshal back: the field uses omitempty, so an "on" value must serialise
	// the key, and an "off" value must omit it (keeping payloads stable for a
	// default-off feature).
	out, err := json.Marshal(ExperimentalConfig{StreamThinking: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"stream_thinking":true`) {
		t.Errorf("marshal(true) = %s, want stream_thinking:true present", out)
	}
	outOff, _ := json.Marshal(ExperimentalConfig{StreamThinking: false})
	if strings.Contains(string(outOff), "stream_thinking") {
		t.Errorf("marshal(false) = %s, want stream_thinking omitted (omitempty)", outOff)
	}
}

// TestStreamThinkingFalseExplicit decodes an explicit false, asserting the key
// is understood (not just absent) and still yields the off state.
func TestStreamThinkingFalseExplicit(t *testing.T) {
	var ex ExperimentalConfig
	if err := json.Unmarshal([]byte(`{"stream_thinking":false}`), &ex); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ex.StreamThinking {
		t.Error("explicit stream_thinking:false must decode to false")
	}
}
