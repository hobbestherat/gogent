package diag

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// These tests cover the fan-out slog handler (issue #562) that tees every diag
// record into BOTH the existing file/stderr sink AND the in-memory ring. The
// load-bearing invariants: the file sink is byte-for-byte unchanged, structured
// records (level/time) are captured first-class, and Secret redaction holds on
// the captured path exactly as on the file path.

// dropTime strips the leading "time=<ts> " token the TextHandler emits so two
// loggers logged at slightly different instants can still be compared for
// byte-identity of everything except the wall-clock timestamp.
func dropTime(s string) string {
	const marker = "time="
	if i := strings.Index(s, marker); i >= 0 {
		if j := strings.IndexByte(s[i:], ' '); j >= 0 {
			return s[:i] + s[i+j+1:]
		}
	}
	return s
}

func TestNewWithRing_FileSinkByteIdenticalToNew(t *testing.T) {
	t.Parallel()
	// The ring is an ADDITIONAL branch; the file/stderr sink must not change.
	var plain, teed bytes.Buffer
	New(&plain).Info("hello", "k", "v", "n", 7)
	NewWithRing(&teed, NewRing(10)).Info("hello", "k", "v", "n", 7)
	if dropTime(plain.String()) != dropTime(teed.String()) {
		t.Fatalf("file sink changed by the ring tee:\nnew:  %q\nteed: %q", plain.String(), teed.String())
	}
}

func TestNewWithRing_NilRingEquivalentToNew(t *testing.T) {
	t.Parallel()
	var a, b bytes.Buffer
	New(&a).Warn("w", "x", "y")
	NewWithRing(&b, nil).Warn("w", "x", "y")
	if dropTime(a.String()) != dropTime(b.String()) {
		t.Fatalf("nil-ring output differs from New:\n%q\n%q", a.String(), b.String())
	}
}

func TestRing_CapturesStructuredRecords(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	lg := NewWithRing(&bytes.Buffer{}, ring)
	lg.Info("starting", "session", "s1")
	lg.Warn("slow", "tool", "bash")
	lg.Error("boom")

	recs := ring.Snapshot()
	if len(recs) != 3 {
		t.Fatalf("captured %d records, want 3", len(recs))
	}
	wantLevels := []slog.Level{slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for i, want := range wantLevels {
		if recs[i].Level != want {
			t.Fatalf("record %d level = %v, want %v", i, recs[i].Level, want)
		}
	}
	// Text is "msg key=value ..." with NO time=/level= text-sink prefix (those are
	// carried as typed Record fields for colouring/ordering).
	if !strings.Contains(recs[0].Text, "starting") || !strings.Contains(recs[0].Text, "session=s1") {
		t.Fatalf("info text = %q", recs[0].Text)
	}
	for _, r := range recs {
		if strings.HasPrefix(r.Text, "time=") || strings.Contains(r.Text, "level=") {
			t.Fatalf("ring record carries the text-sink prefix: %q", r.Text)
		}
		if r.Time.IsZero() {
			t.Fatalf("record %q has zero time", r.Text)
		}
	}
}

func TestRing_FormattedMethodsCaptured(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	lg := NewWithRing(&bytes.Buffer{}, ring)
	lg.Infof("count=%d", 42)
	lg.Warnf("warn %s", "fmt")
	lg.Errorf("err %d", 1)
	recs := ring.Snapshot()
	if len(recs) != 3 {
		t.Fatalf("captured %d, want 3", len(recs))
	}
	if !strings.Contains(recs[0].Text, "count=42") {
		t.Fatalf("Infof text = %q", recs[0].Text)
	}
}

// Records below the Info gate (Debug) must be captured by neither the file sink
// nor the ring — both handlers gate at LevelInfo, keeping embedded/headless
// behaviour unchanged.
func TestRing_DoesNotCaptureBelowInfo(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	var buf bytes.Buffer
	lg := NewWithRing(&buf, ring)
	lg.sl.Debug("not captured")
	if len(ring.Snapshot()) != 0 {
		t.Fatalf("debug record captured by ring: %+v", ring.Snapshot())
	}
	if strings.Contains(buf.String(), "not captured") {
		t.Fatalf("debug record reached the file sink: %q", buf.String())
	}
}

// Secret redaction on the captured path is a hard acceptance criterion: a leaked
// secret in the in-app Logs window or over the daemon stream would be a real
// security defect.
func TestRing_SecretRedactedOnRingPath(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	lg := NewWithRing(&bytes.Buffer{}, ring)
	lg.Info("auth", "key", Secret("hunter2"), "plain", "visible")
	recs := ring.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("captured %d, want 1", len(recs))
	}
	if strings.Contains(recs[0].Text, "hunter2") {
		t.Fatalf("SECRET LEAKED into ring record: %q", recs[0].Text)
	}
	if !strings.Contains(recs[0].Text, "[REDACTED]") {
		t.Fatalf("ring record did not redact the secret: %q", recs[0].Text)
	}
	if !strings.Contains(recs[0].Text, "plain=visible") {
		t.Fatalf("ring record dropped the non-secret attr: %q", recs[0].Text)
	}
}

// The file sink must ALSO redact (it always did; this guards against the fan-out
// somehow exposing the raw value on the text branch).
func TestRing_SecretRedactedOnFileSink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := NewWithRing(&buf, NewRing(4))
	lg.Info("auth", "key", Secret("hunter2"))
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("SECRET LEAKED into the file sink: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED]") {
		t.Fatalf("file sink did not redact the secret: %q", buf.String())
	}
}

func TestRing_SecretRedactedViaWithAttrs(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	lg := NewWithRing(&bytes.Buffer{}, ring).With("token", Secret("leak-me"))
	lg.Info("request")
	recs := ring.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("captured %d, want 1", len(recs))
	}
	if strings.Contains(recs[0].Text, "leak-me") {
		t.Fatalf("SECRET LEAKED via WithAttrs: %q", recs[0].Text)
	}
	if !strings.Contains(recs[0].Text, "[REDACTED]") {
		t.Fatalf("WithAttrs secret not redacted: %q", recs[0].Text)
	}
}

func TestRing_SecretRedactedViaWithGroup(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	// diag.Logger does not expose WithGroup, so exercise the handler's group path
	// directly to confirm a Secret nested under a group is still redacted.
	fanout := &fanoutHandler{handlers: []slog.Handler{
		slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}),
		newRingHandler(ring),
	}}
	slog.New(fanout).WithGroup("req").Info("x", "password", Secret("p4ss"))
	recs := ring.Snapshot()
	if len(recs) != 1 || strings.Contains(recs[0].Text, "p4ss") {
		t.Fatalf("SECRET LEAKED via WithGroup: %+v", recs)
	}
	if !strings.Contains(recs[0].Text, "[REDACTED]") {
		t.Fatalf("WithGroup secret not redacted: %q", recs[0].Text)
	}
	// The open group prefix should ride on the attr key.
	if !strings.Contains(recs[0].Text, "req.password") {
		t.Fatalf("group prefix lost: %q", recs[0].Text)
	}
}

func TestRing_EmptySecretRenderedEmpty(t *testing.T) {
	t.Parallel()
	ring := NewRing(8)
	lg := NewWithRing(&bytes.Buffer{}, ring)
	lg.Info("cfg", "key", Secret(""))
	recs := ring.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("captured %d, want 1", len(recs))
	}
	if strings.Contains(recs[0].Text, "[REDACTED]") {
		t.Fatalf("empty secret should render empty, not redacted: %q", recs[0].Text)
	}
}
