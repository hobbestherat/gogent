package agent

import (
	"context"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

const visionTestImage = "data:image/png;base64,iVBORw0KGgo="

// visionTestSession builds a session whose connector reports the given vision
// capability, using a real *model.ModelConnection (which implements
// model.VisionReporter).
func visionTestSession(t *testing.T, vision bool) (*UserSession, *model.ModelSession) {
	t.Helper()
	conn := model.NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: model.DefaultModelURL},
		&config.ModelConfig{DisplayName: "Test Model", Caps: config.ModelCapabilities{Vision: vision}},
	)
	sess := model.NewModelSession("vis", conn)
	us := NewUserSession("vis", NewAgent("root", sess))
	return us, sess
}

// collectNotices returns an emit func plus a pointer to the slice of notice texts
// it records, so a test can assert exactly how many warnings fired.
func collectNotices() (func(SessionEvent), *[]string) {
	var notices []string
	emit := func(ev SessionEvent) {
		if ev.Type == SessionEventNotice {
			notices = append(notices, ev.Text)
		}
	}
	return emit, &notices
}

func TestTurnHasImages(t *testing.T) {
	if turnHasImages([]model.Message{{Role: model.RoleUser, Content: "hi"}}) {
		t.Error("turnHasImages = true for a text-only turn")
	}
	if !turnHasImages([]model.Message{model.UserImageMessage("look", visionTestImage)}) {
		t.Error("turnHasImages = false for a turn carrying an image")
	}
	// Mixed batch: image lives on a later message.
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "text"},
		model.UserImageMessage("", visionTestImage),
	}
	if !turnHasImages(msgs) {
		t.Error("turnHasImages = false when a later message carries an image")
	}
}

// TestWarnVisionMismatchWarnsOnNonVisionModel: images + non-vision model => exactly
// one notice that names the model and reads as non-blocking.
func TestWarnVisionMismatchWarnsOnNonVisionModel(t *testing.T) {
	us, sess := visionTestSession(t, false)
	emit, notices := collectNotices()

	msgs := []model.Message{model.UserImageMessage("what is this?", visionTestImage)}
	us.warnVisionMismatch(sess, msgs, emit)

	if len(*notices) != 1 {
		t.Fatalf("expected exactly 1 notice, got %d: %v", len(*notices), *notices)
	}
	text := (*notices)[0]
	if !strings.Contains(text, "Test Model") {
		t.Errorf("notice should name the model, got %q", text)
	}
	if !strings.Contains(strings.ToLower(text), "vision") {
		t.Errorf("notice should mention vision, got %q", text)
	}
	// The reassurance wording is the whole point of a non-blocking notice.
	if !strings.Contains(strings.ToLower(text), "sent anyway") {
		t.Errorf("notice should reassure the images were sent anyway, got %q", text)
	}

	// The images must NOT be stripped or altered — they are still sent intact.
	if len(msgs[0].Images) != 1 || msgs[0].Images[0].URL != visionTestImage {
		t.Errorf("warnVisionMismatch must not strip/mutate images; got %+v", msgs[0].Images)
	}
}

// TestWarnVisionMismatchSilentOnVisionModel: a vision-capable model never warns.
func TestWarnVisionMismatchSilentOnVisionModel(t *testing.T) {
	us, sess := visionTestSession(t, true)
	emit, notices := collectNotices()

	us.warnVisionMismatch(sess, []model.Message{model.UserImageMessage("hi", visionTestImage)}, emit)

	if len(*notices) != 0 {
		t.Fatalf("vision-capable model must not warn, got %v", *notices)
	}
}

// TestWarnVisionMismatchSilentWithoutImages: a text-only turn never warns, even on
// a non-vision model.
func TestWarnVisionMismatchSilentWithoutImages(t *testing.T) {
	us, sess := visionTestSession(t, false)
	emit, notices := collectNotices()

	us.warnVisionMismatch(sess, []model.Message{{Role: model.RoleUser, Content: "no images here"}}, emit)

	if len(*notices) != 0 {
		t.Fatalf("a text-only turn must not warn, got %v", *notices)
	}
}

// noCapsConnector is a minimal Connector that does NOT implement
// model.VisionReporter, standing in for a mock/future backend whose vision
// capability is unknown.
type noCapsConnector struct{}

func (noCapsConnector) Complete(messages []model.Message) (*model.CompletionResponse, error) {
	return &model.CompletionResponse{}, nil
}

func (noCapsConnector) CompleteWithTools(messages []model.Message, tools []model.ToolDef) (*model.CompletionResponse, error) {
	return &model.CompletionResponse{}, nil
}

func (noCapsConnector) CompleteWithToolsCtx(ctx context.Context, messages []model.Message, tools []model.ToolDef) (*model.CompletionResponse, error) {
	return &model.CompletionResponse{}, nil
}

func (noCapsConnector) CompleteStream(messages []model.Message) (<-chan model.StreamResponse, <-chan error) {
	return nil, nil
}

func (noCapsConnector) GetStats() *model.ModelStats        { return &model.ModelStats{} }
func (noCapsConnector) StatsSnapshot() model.StatsSnapshot { return model.StatsSnapshot{} }

// TestWarnVisionMismatchSilentWhenCapsUnknown: a connector that does not report a
// vision capability must never trigger a false warning.
func TestWarnVisionMismatchSilentWhenCapsUnknown(t *testing.T) {
	sess := model.NewModelSession("unknown", noCapsConnector{})
	us := NewUserSession("unknown", NewAgent("root", sess))
	emit, notices := collectNotices()

	us.warnVisionMismatch(sess, []model.Message{model.UserImageMessage("hi", visionTestImage)}, emit)

	if len(*notices) != 0 {
		t.Fatalf("unknown-caps connector must not warn, got %v", *notices)
	}
}

func TestVisionMismatchNoticeFallsBackWhenUnnamed(t *testing.T) {
	got := visionMismatchNotice("")
	if !strings.Contains(got, "current model") {
		t.Errorf("expected a generic fallback name, got %q", got)
	}
}
