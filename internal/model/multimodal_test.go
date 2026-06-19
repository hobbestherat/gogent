package model

import (
	"encoding/json"
	"testing"
)

// pngDataURL is a tiny valid base64 payload used as a stand-in image.
const pngDataURL = "data:image/png;base64,iVBORw0KGgo="

func TestMessageMarshalTextOnlyUnchanged(t *testing.T) {
	// A text-only message must serialize byte-for-byte as it always has: a
	// scalar string content, so no existing transcript or request shape changes.
	cases := []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "result"},
	}
	want := []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
		`{"role":"tool","content":"result","tool_call_id":"c1"}`,
	}
	for i, m := range cases {
		got, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		if string(got) != want[i] {
			t.Errorf("case %d:\n got %s\nwant %s", i, got, want[i])
		}
	}
}

func TestMessageMarshalWithImages(t *testing.T) {
	m := UserImageMessage("what is this?", pngDataURL)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// content is now an OpenAI content-parts array: leading text then image_url.
	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != "user" || len(got.Content) != 2 {
		t.Fatalf("content = %+v", got.Content)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "what is this?" {
		t.Errorf("text part = %+v", got.Content[0])
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil ||
		got.Content[1].ImageURL.URL != pngDataURL {
		t.Errorf("image part = %+v", got.Content[1])
	}
}

func TestMessageMarshalImageOnlyNoText(t *testing.T) {
	// With no accompanying text, only the image part is emitted (no empty text part).
	raw, err := json.Marshal(UserImageMessage("", pngDataURL))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "image_url" {
		t.Errorf("content = %+v, want a single image_url part", got.Content)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	cases := []Message{
		{Role: RoleUser, Content: "plain text"},
		UserImageMessage("describe", pngDataURL, "https://example.com/a.png"),
		{Role: RoleUser, Images: []ImageURL{{URL: pngDataURL, Detail: "high"}}},
	}
	for i, m := range cases {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var back Message
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if back.Content != m.Content {
			t.Errorf("case %d content = %q, want %q", i, back.Content, m.Content)
		}
		if len(back.Images) != len(m.Images) {
			t.Fatalf("case %d images = %d, want %d", i, len(back.Images), len(m.Images))
		}
		for j := range m.Images {
			if back.Images[j] != m.Images[j] {
				t.Errorf("case %d image %d = %+v, want %+v", i, j, back.Images[j], m.Images[j])
			}
		}
	}
}

func TestMessageUnmarshalLegacyStringContent(t *testing.T) {
	// Older transcripts persisted content as a scalar string; it must still load.
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"legacy"}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "legacy" || len(m.Images) != 0 {
		t.Errorf("got %+v", m)
	}
}

func TestDataURL(t *testing.T) {
	got := DataURL("image/png", []byte("hi"))
	if want := "data:image/png;base64,aGk="; got != want {
		t.Errorf("DataURL = %q, want %q", got, want)
	}
}

func TestParseDataURL(t *testing.T) {
	tests := []struct {
		in        string
		mediaType string
		data      string
		ok        bool
	}{
		{"data:image/png;base64,iVBORw0KGgo=", "image/png", "iVBORw0KGgo=", true},
		{"data:image/jpeg;base64,abc", "image/jpeg", "abc", true},
		{"https://example.com/a.png", "", "", false}, // remote URL, not a data URL
		{"data:image/png,notbase64", "", "", false},  // missing ;base64
		{"data:image/png;base64", "", "", false},     // no comma
		{"", "", "", false},
	}
	for _, tt := range tests {
		mt, d, ok := parseDataURL(tt.in)
		if ok != tt.ok || mt != tt.mediaType || d != tt.data {
			t.Errorf("parseDataURL(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tt.in, mt, d, ok, tt.mediaType, tt.data, tt.ok)
		}
	}
}

func TestAnthropicBuildBodyWithImages(t *testing.T) {
	req := CompletionRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []Message{UserImageMessage("look", pngDataURL, "https://example.com/a.png")},
	}
	raw, err := anthropicAdapter{}.buildBody(req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(got.Messages))
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (text + 2 images): %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "look" {
		t.Errorf("text block = %+v", blocks[0])
	}
	// data: URL → inline base64 source.
	if blocks[1].Type != "image" || blocks[1].Source == nil ||
		blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" ||
		blocks[1].Source.Data != "iVBORw0KGgo=" {
		t.Errorf("base64 image block = %+v", blocks[1])
	}
	// remote URL → url source.
	if blocks[2].Type != "image" || blocks[2].Source == nil ||
		blocks[2].Source.Type != "url" || blocks[2].Source.URL != "https://example.com/a.png" {
		t.Errorf("url image block = %+v", blocks[2])
	}
}

func TestOpenAIBuildBodyWithImages(t *testing.T) {
	// The OpenAI adapter marshals the request directly, so image messages flow
	// through Message.MarshalJSON into the content-parts array form on the wire.
	req := CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{UserImageMessage("hi", pngDataURL)},
	}
	raw, err := openAIAdapter{}.buildBody(req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 ||
		got.Messages[0].Content[0].Type != "text" || got.Messages[0].Content[1].Type != "image_url" {
		t.Errorf("content = %+v", got.Messages)
	}
}
