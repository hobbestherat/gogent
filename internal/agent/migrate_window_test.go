package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/model"
)

// These tests lock the issue #589 safe-session-migration contract: switching an
// existing session to a model with a smaller context window compresses the
// transcript in bounded, chunked rounds until it fits, or fails with a clear,
// actionable error that is surfaced (not silently dropped) and never panics.
//
// They drive MigrateToContextWindow directly with a stub completer so no network
// is involved. Token sizes are real (model.EstimateTokens is char/4), so the
// guard's measurements are deterministic.

// makeMigrationSession builds a session whose root agent's ThoughtTrain carries a
// transcript of `turns` user/assistant pairs, each message `chars` characters, with
// the migration baseline (lastMigrationWindow) set to prevWindow. CurrentTokenCount
// is seeded from the real estimate so the guard's before/after measurements are
// honest; callers may overwrite it to force an over/under-budget scenario.
func makeMigrationSession(id string, turns, chars, prevWindow int) (*UserSession, *model.ModelSession) {
	conn := model.NewModelConnection()
	sess := model.NewModelSession(id, conn)
	var msgs []model.Message
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			model.Message{Role: model.RoleUser, Content: strings.Repeat("q", chars)},
			model.Message{Role: model.RoleAssistant, Content: strings.Repeat("a", chars)},
		)
	}
	sess.ReplaceTranscript(msgs)
	sess.SetLastMigrationWindow(prevWindow)
	sess.CurrentTokenCount = model.EstimateTokens(msgs)
	ag := NewAgent("root", sess)
	return NewUserSession(id, ag), sess
}

// withStub wires a stub compression completer whose digest is `digest` and returns
// the session, the stub (for call-counting), and a turn-id-stamped context.
func withStub(us *UserSession, digest, turnID string) (*stubCompleter, context.Context) {
	stub := &stubCompleter{content: digest}
	us.SetCompressionCompleter(stub)
	ctx := context.Background()
	if turnID != "" {
		ctx = WithTurnID(context.Background(), turnID)
	}
	return stub, ctx
}

// TestMigrateNilArgs: nil session or config is a no-op, never a panic.
func TestMigrateNilArgs(t *testing.T) {
	us, sess := makeMigrationSession("nil", 3, 10, 0)
	stub, ctx := withStub(us, "D", "t-nil")
	cfg := &config.ModelConfig{Name: "m", ContextWindow: 100}

	for name, tc := range map[string]struct {
		sess *model.ModelSession
		cfg  *config.ModelConfig
	}{
		"nil session": {nil, cfg},
		"nil config":  {sess, nil},
		"both nil":    {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("MigrateToContextWindow panicked on %s: %v", name, r)
				}
			}()
			if err := us.MigrateToContextWindow(ctx, tc.sess, tc.cfg); err != nil {
				t.Errorf("nil input returned err %v, want nil", err)
			}
			if stub.calls != 0 {
				t.Errorf("nil input invoked compression %d times, want 0", stub.calls)
			}
		})
	}
}

// TestMigrateUnknownWindowNoGuard: an unknown window (0, or negative) must NOT
// engage the guard or block migration — today's behavior is preserved. Issue #589
// explicitly forbids blocking migration on missing data.
func TestMigrateUnknownWindowNoGuard(t *testing.T) {
	for _, cw := range []int{0, -1024} {
		us, sess := makeMigrationSession("unknown", 6, 100, 0)
		stub, ctx := withStub(us, "D", "t-unknown")
		sess.CurrentTokenCount = 99999 // far over any budget — must still be a no-op

		cfg := &config.ModelConfig{Name: "m", ContextWindow: cw}
		if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
			t.Errorf("ContextWindow=%d returned err %v, want nil (unknown window)", cw, err)
		}
		if stub.calls != 0 {
			t.Errorf("ContextWindow=%d invoked compression %d times, want 0", cw, stub.calls)
		}
		// The raw target is recorded verbatim (0 for unset, the negative value for a
		// mis-configured model). Either way it is <= 0, so the prev>0 skip-gate treats
		// it as "not yet calibrated" and a later known window is evaluated fresh.
		if got := sess.LastMigrationWindow(); got != cw {
			t.Errorf("LastMigrationWindow after ContextWindow=%d = %d, want %d (raw value)", cw, got, cw)
		}
	}
}

// TestMigrateFitsNoCompression: a session already under the headroom target
// migrates unchanged — zero compression calls, transcript untouched.
func TestMigrateFitsNoCompression(t *testing.T) {
	us, sess := makeMigrationSession("fits", 6, 10, 0)
	stub, ctx := withStub(us, "D", "t-fits")
	before := sess.GetTranscript()
	cfg := &config.ModelConfig{Name: "m", ContextWindow: 100000} // fit = 80000 >> session size

	if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
		t.Fatalf("fits: unexpected err %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("fits invoked compression %d times, want 0", stub.calls)
	}
	if got := sess.GetTranscript(); len(got) != len(before) {
		t.Errorf("fits mutated transcript: %d -> %d messages", len(before), len(got))
	}
	if got := sess.LastMigrationWindow(); got != 100000 {
		t.Errorf("LastMigrationWindow after fit = %d, want 100000", got)
	}
}

// TestMigrateShrinksToFit: an over-budget session (first calibration, prev=0)
// triggers the chunked fallback, actually shrinks, and ends at/under the headroom
// target with no error. Each compression round splices the shared digest marker.
func TestMigrateShrinksToFit(t *testing.T) {
	us, sess := makeMigrationSession("shrink", 6, 40, 0) // ~120 tokens
	stub, ctx := withStub(us, "D", "t-shrink")
	const target = 100 // fit = 80; session ~120 > 80
	cfg := &config.ModelConfig{Name: "m", ContextWindow: target}
	beforeTokens := sess.GetCurrentTokenCount()
	beforeLen := len(sess.GetTranscript())

	if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
		t.Fatalf("shrink: unexpected err %v", err)
	}
	if stub.calls < 1 {
		t.Fatalf("shrink invoked compression %d times, want >= 1", stub.calls)
	}
	fit := int(float64(target) * migrationTargetFraction)
	if got := sess.GetCurrentTokenCount(); got > fit {
		t.Errorf("shrink ended at %d tokens, want <= fit %d", got, fit)
	}
	if got := sess.GetCurrentTokenCount(); got >= beforeTokens {
		t.Errorf("shrink did not reduce tokens: %d -> %d", beforeTokens, got)
	}
	if got := len(sess.GetTranscript()); got >= beforeLen {
		t.Errorf("shrink did not reduce transcript length: %d -> %d", beforeLen, got)
	}
	// The compressed prefix must be the shared digest marker (same shape the in-loop
	// compaction produces), proving the two paths splice identical messages.
	if first := sess.GetTranscript()[0]; first.Role != model.RoleUser ||
		!strings.HasPrefix(first.Content, "[Earlier conversation summarized to save context]") {
		t.Errorf("compressed prefix = %+v, want digest marker", first)
	}
}

// TestMigrateStopsOnBackendFailure: when the compression backend fails (empty
// digest), the fallback stops without spinning and reports the shortfall as a
// clean error — never a panic, never a half-applied mutation it can't account for.
func TestMigrateStopsOnBackendFailure(t *testing.T) {
	us, sess := makeMigrationSession("backendfail", 6, 100, 0)
	stub, ctx := withStub(us, "", "t-fail") // empty digest ⇒ summarizeOlder returns false
	sess.CurrentTokenCount = 99999
	cfg := &config.ModelConfig{Name: "m", DisplayName: "FailModel", ContextWindow: 100}

	err := us.MigrateToContextWindow(ctx, sess, cfg)
	if err == nil {
		t.Fatal("backend failure returned nil err, want a clean unfit error")
	}
	if stub.calls < 1 {
		t.Fatalf("backend failure did not attempt compression: %d calls, want >= 1", stub.calls)
	}
	if stub.calls > maxMigrationRounds {
		t.Errorf("backend failure spun %d rounds, want <= %d", stub.calls, maxMigrationRounds)
	}
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("error %q does not name the target window 100", err.Error())
	}
}

// TestMigrateImpossibleFailsCleanly: a transcript that cannot fit even after
// maximal compression (a single turn larger than the window) yields a clear error
// naming the model and its window — no panic, no silent truncation.
func TestMigrateImpossibleFailsCleanly(t *testing.T) {
	us, sess := makeMigrationSession("impossible", 0, 0, 0)
	sess.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: strings.Repeat("x", 400)}, // 100 tokens
	})
	sess.CurrentTokenCount = model.EstimateTokens(sess.GetTranscript())
	stub, ctx := withStub(us, "D", "t-impossible")
	cfg := &config.ModelConfig{Name: "tiny-id", DisplayName: "TinyWindow", ContextWindow: 50} // fit=40

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("impossible case panicked: %v", r)
		}
	}()
	err := us.MigrateToContextWindow(ctx, sess, cfg)
	if err == nil {
		t.Fatal("impossible case returned nil err, want a clean unfit error")
	}
	// The single message can't be split (nothing older), so no compression is even
	// attempted — the error is reported, not a silent truncation.
	if stub.calls != 0 {
		t.Errorf("impossible case invoked compression %d times, want 0 (nothing older to fold)", stub.calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, "50") {
		t.Errorf("error %q does not name the target window 50", msg)
	}
	if !strings.Contains(msg, "TinyWindow") {
		t.Errorf("error %q does not name the target model TinyWindow", msg)
	}
}

// TestMigrateImpossibleEmitsErrorEvent (D1): the unfit error is surfaced as a
// SessionEventError through the observer (the daemon/SSE path discards the
// returned error), stamped with the originating turn id — not a silent idle.
func TestMigrateImpossibleEmitsErrorEvent(t *testing.T) {
	us, sess := makeMigrationSession("emit", 0, 0, 0)
	sess.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: strings.Repeat("x", 400)},
	})
	sess.CurrentTokenCount = model.EstimateTokens(sess.GetTranscript())
	stub, ctx := withStub(us, "D", "turn-abc")
	cfg := &config.ModelConfig{Name: "tiny", ContextWindow: 50}

	var events []SessionEvent
	us.SetObserver(func(ev SessionEvent) { events = append(events, ev) })

	err := us.MigrateToContextWindow(ctx, sess, cfg)
	if err == nil {
		t.Fatal("expected unfit error")
	}
	// The single turn is unsplittable, so no compression round runs — the error is
	// reported and emitted without any backend call.
	if stub.calls != 0 {
		t.Errorf("impossible-emit invoked compression %d times, want 0", stub.calls)
	}
	var errEvents []SessionEvent
	for _, ev := range events {
		if ev.Type == SessionEventError {
			errEvents = append(errEvents, ev)
		}
	}
	if len(errEvents) != 1 {
		t.Fatalf("got %d SessionEventError events, want exactly 1 (no duplicate, no silence): %+v", len(errEvents), events)
	}
	if errEvents[0].Err == nil || !strings.Contains(errEvents[0].Err.Error(), "cannot fit") {
		t.Errorf("error event payload = %+v, want the unfit error", errEvents[0].Err)
	}
	if errEvents[0].TurnID != "turn-abc" {
		t.Errorf("error event TurnID = %q, want turn-abc (stamped from ctx)", errEvents[0].TurnID)
	}
	if !errors.Is(errEvents[0].Err, err) && errEvents[0].Err.Error() != err.Error() {
		t.Errorf("emitted error != returned error: emitted=%v returned=%v", errEvents[0].Err, err)
	}
}

// TestMigrateBoundedRounds: a non-converging fallback (digest larger than the
// original) terminates within the round cap and reports the clean error — no
// infinite loop, no panic.
func TestMigrateBoundedRounds(t *testing.T) {
	us, sess := makeMigrationSession("bounded", 6, 400, 0) // ~1200 tokens
	// A digest so large the transcript can never get near the target window.
	stub, ctx := withStub(us, strings.Repeat("z", 5000), "t-bounded")
	beforeTokens := sess.GetCurrentTokenCount()
	cfg := &config.ModelConfig{Name: "m", ContextWindow: 200} // fit=160

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("bounded case panicked: %v", r)
		}
	}()
	err := us.MigrateToContextWindow(ctx, sess, cfg)
	if err == nil {
		t.Fatal("bounded case returned nil err, want a clean unfit error")
	}
	if stub.calls < 1 {
		t.Fatalf("bounded case invoked compression %d times, want >= 1", stub.calls)
	}
	if stub.calls > maxMigrationRounds {
		t.Errorf("bounded case spun %d rounds, want <= cap %d (no infinite loop)", stub.calls, maxMigrationRounds)
	}
	// D3 lock: the error names the ACHIEVED (post-compression) token count, not a
	// stale pre-loop size. Compression ran here (stub.calls >= 1) so the two differ.
	achieved := sess.GetCurrentTokenCount()
	if achieved == beforeTokens {
		t.Fatalf("setup invariant: compression did not change the count, cannot validate D3")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(achieved)) {
		t.Errorf("error %q does not name the achieved size %d (D3: live count, not stale)", err.Error(), achieved)
	}
	// Transcript stays structurally valid: non-empty, recent tail preserved.
	if got := sess.GetTranscript(); len(got) == 0 {
		t.Error("bounded failure left an empty transcript")
	}
}

// TestMigrateSameWindowSkips (D2): a target window equal to the one last evaluated
// is a no-op even when the session is over budget — same-model compaction cadence
// and cost are unchanged (ordinary growth stays owned by compactIfNeeded).
func TestMigrateSameWindowSkips(t *testing.T) {
	const window = 1000
	us, sess := makeMigrationSession("same", 6, 100, window)
	stub, ctx := withStub(us, "D", "t-same")
	sess.CurrentTokenCount = 9999 // over fit (800) — must still skip

	cfg := &config.ModelConfig{Name: "m", ContextWindow: window}
	if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
		t.Fatalf("same window returned err %v, want nil", err)
	}
	if stub.calls != 0 {
		t.Errorf("same window invoked compression %d times, want 0", stub.calls)
	}
	if got := sess.GetCurrentTokenCount(); got != 9999 {
		t.Errorf("same window mutated token count: %d -> %d", 9999, got)
	}
}

// TestMigrateLargerWindowSkips (D2): switching to an equal-or-larger window never
// overflows a session the in-loop compaction already keeps in check, so the guard
// skips entirely.
func TestMigrateLargerWindowSkips(t *testing.T) {
	us, sess := makeMigrationSession("larger", 6, 100, 500)
	stub, ctx := withStub(us, "D", "t-larger")
	sess.CurrentTokenCount = 9999

	cfg := &config.ModelConfig{Name: "m", ContextWindow: 1000} // > prev (500)
	if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
		t.Fatalf("larger window returned err %v, want nil", err)
	}
	if stub.calls != 0 {
		t.Errorf("larger window invoked compression %d times, want 0", stub.calls)
	}
}

// TestMigrateSmallerWindowEngages (D2): a strict shrink (target < prev) engages the
// fallback when the session is over budget — proving the gate fires in exactly the
// migration direction the issue is about.
func TestMigrateSmallerWindowEngages(t *testing.T) {
	us, sess := makeMigrationSession("smaller", 6, 100, 2000) // ~300 tokens, prev=2000
	stub, ctx := withStub(us, "D", "t-smaller")

	cfg := &config.ModelConfig{Name: "m", ContextWindow: 200} // < prev, fit=160 < 300
	if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
		t.Fatalf("smaller window returned err %v, want nil (should compress to fit)", err)
	}
	if stub.calls < 1 {
		t.Fatalf("smaller window invoked compression %d times, want >= 1", stub.calls)
	}
	if got := sess.LastMigrationWindow(); got != 200 {
		t.Errorf("LastMigrationWindow after shrink = %d, want 200", got)
	}
}

// TestMigrateEmitsCompactionPerRound: each compression round emits a
// SessionEventCompaction stamped with the turn id, so the user sees progress.
func TestMigrateEmitsCompactionPerRound(t *testing.T) {
	us, sess := makeMigrationSession("events", 6, 40, 0) // ~120 tokens
	stub, ctx := withStub(us, "D", "turn-xyz")
	cfg := &config.ModelConfig{Name: "m", ContextWindow: 100} // fit=80

	var events []SessionEvent
	us.SetObserver(func(ev SessionEvent) { events = append(events, ev) })

	if err := us.MigrateToContextWindow(ctx, sess, cfg); err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	var compactions []SessionEvent
	for _, ev := range events {
		if ev.Type == SessionEventCompaction {
			compactions = append(compactions, ev)
		}
	}
	if len(compactions) < 1 {
		t.Fatalf("got %d compaction events, want >= 1; events=%+v", len(compactions), events)
	}
	for _, ev := range compactions {
		if ev.TurnID != "turn-xyz" {
			t.Errorf("compaction event TurnID = %q, want turn-xyz", ev.TurnID)
		}
	}
	// The number of compaction events matches the compression rounds actually run.
	if len(compactions) != stub.calls {
		t.Errorf("compaction events %d != compression calls %d", len(compactions), stub.calls)
	}
}

// TestMigrateFailureLeavesValidTranscript: on the unfit path the transcript is
// never left empty or corrupt — the recent tail is preserved (SafeSplit never
// strands content), so a later switch to a larger model still has a usable history.
func TestMigrateFailureLeavesValidTranscript(t *testing.T) {
	us, sess := makeMigrationSession("validfail", 0, 0, 0)
	huge := strings.Repeat("x", 400)
	sess.ReplaceTranscript([]model.Message{{Role: model.RoleUser, Content: huge}})
	sess.CurrentTokenCount = model.EstimateTokens(sess.GetTranscript())
	stub, ctx := withStub(us, "D", "t-valid")
	cfg := &config.ModelConfig{Name: "tiny", ContextWindow: 50}

	if err := us.MigrateToContextWindow(ctx, sess, cfg); err == nil {
		t.Fatal("expected unfit error")
	}
	// The unsplittable single turn means no compression round ran.
	if stub.calls != 0 {
		t.Errorf("valid-fail invoked compression %d times, want 0", stub.calls)
	}
	got := sess.GetTranscript()
	if len(got) != 1 {
		t.Fatalf("transcript length = %d, want 1 (the unsplittable turn, untouched)", len(got))
	}
	if got[0].Content != huge {
		t.Errorf("transcript content mutated on failure (len %d -> %d)", len(huge), len(got[0].Content))
	}
}

// TestExecuteTaskLoopWithModelMigratesBeforeLoop: ExecuteTaskLoopWithModel runs the
// migration guard before the task loop, so an over-budget smaller-window turn
// returns the migration error WITHOUT ever entering ExecuteTaskLoop (no model /
// network call). This is the seam the daemon and embedded paths funnel through.
func TestExecuteTaskLoopWithModelMigratesBeforeLoop(t *testing.T) {
	us, sess := makeMigrationSession("etl", 0, 0, 0)
	sess.ReplaceTranscript([]model.Message{
		{Role: model.RoleUser, Content: strings.Repeat("x", 400)}, // 100 tokens
	})
	sess.CurrentTokenCount = model.EstimateTokens(sess.GetTranscript())
	stub, ctx := withStub(us, "D", "t-etl")
	cfg := &config.ModelConfig{Name: "tiny", ContextWindow: 50} // fit=40 < 100

	_, err := us.ExecuteTaskLoopWithModel(ctx, "root", "hi", cfg)
	if err == nil {
		t.Fatal("ExecuteTaskLoopWithModel returned nil err, want the migration unfit error")
	}
	// The migration error is the one returned — proving the task loop was never
	// reached (it would otherwise attempt a model round-trip).
	if !strings.Contains(err.Error(), "cannot fit") {
		t.Errorf("err = %q, want the migration unfit error (loop should not have run)", err.Error())
	}
	// Nothing was compressed (the single turn is unsplittable) and the loop never
	// ran, so the backend was never called.
	if stub.calls != 0 {
		t.Errorf("backend called %d times, want 0 (no compression, loop not reached)", stub.calls)
	}
}
