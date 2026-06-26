package gogent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
)

// issue487Conn is a scripted fake Connector for issue #487 persistence tests.
// next() is invoked on each completion; it can return a classified *ModelError,
// a successful response with Usage, or (resp, err) together to exercise the
// forward-compatible error-path usage guard. It deliberately omits
// StreamingToolCompleter so sendCtx takes the blocking CompleteWithToolsCtx path.
type issue487Conn struct {
	next func() (*model.CompletionResponse, error)
}

func (c *issue487Conn) Complete(messages []model.Message) (*model.CompletionResponse, error) {
	return c.CompleteWithTools(messages, nil)
}
func (c *issue487Conn) CompleteWithTools(messages []model.Message, tools []model.ToolDef) (*model.CompletionResponse, error) {
	return c.CompleteWithToolsCtx(context.Background(), messages, tools)
}
func (c *issue487Conn) CompleteWithToolsCtx(ctx context.Context, messages []model.Message, tools []model.ToolDef) (*model.CompletionResponse, error) {
	if c.next == nil {
		return &model.CompletionResponse{Role: model.RoleAssistant, Content: "ok"}, nil
	}
	return c.next()
}
func (c *issue487Conn) CompleteStream(messages []model.Message) (<-chan model.StreamResponse, <-chan error) {
	ch := make(chan model.StreamResponse)
	errCh := make(chan error, 1)
	close(ch)
	close(errCh)
	return ch, errCh
}
func (c *issue487Conn) GetStats() *model.ModelStats        { return &model.ModelStats{} }
func (c *issue487Conn) StatsSnapshot() model.StatsSnapshot { return model.StatsSnapshot{} }

func errorConn(me *model.ModelError) *issue487Conn {
	return &issue487Conn{next: func() (*model.CompletionResponse, error) { return nil, me }}
}
func successConn(resp *model.CompletionResponse) *issue487Conn {
	return &issue487Conn{next: func() (*model.CompletionResponse, error) { return resp, nil }}
}
func usageResp(prompt, completion, cached, reasoning int) *model.CompletionResponse {
	return &model.CompletionResponse{
		Role:    model.RoleAssistant,
		Content: "answer",
		Usage: &model.TokenUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
			CachedTokens:     cached,
			ReasoningTokens:  reasoning,
		},
	}
}

// newSessionWithConn builds a UserSession whose root agent's ThoughtTrain is
// backed by conn, ready for direct sendCtx-driven persistence tests.
func newSessionWithConn(id string, conn model.Connector) (*agent.UserSession, *model.ModelSession) {
	sess := model.NewModelSession("main", conn)
	root := agent.NewAgent("root", sess)
	return agent.NewUserSession(id, root), sess
}

// findLoaded returns the loaded session with the given id, failing the test if
// absent.
func findLoaded(t *testing.T, loaded []LoadedSession, id string) LoadedSession {
	t.Helper()
	for _, l := range loaded {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("loaded session %q not found among %d sessions", id, len(loaded))
	return LoadedSession{}
}

// freshStoreOn opens a brand-new SessionStore over dir (simulating a restart).
func freshStoreOn(t *testing.T, dir string) *SessionStore {
	t.Helper()
	st, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore(%q): %v", dir, err)
	}
	return st
}

// parseShardFile reads every JSONL line of a shard file into raw jsonlRecords,
// for exact on-disk-shape assertions.
func parseShardFile(t *testing.T, path string) []jsonlRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shard %s: %v", path, err)
	}
	var out []jsonlRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec jsonlRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal shard line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// firstShardPath returns the path to the single (active) shard of a freshly
// written session, failing if there isn't exactly one shard file.
func firstShardPath(t *testing.T, dir string) string {
	t.Helper()
	var matches []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, shardFileExt) {
			matches = append(matches, p)
		}
		return nil
	})
	if len(matches) != 1 {
		t.Fatalf("expected exactly one shard in %s, got %d: %v", dir, len(matches), matches)
	}
	return matches[0]
}

// ----------------------------------------------------------------------------
// encodeTurnMeta: one record per turn, precedence, skip-empty
// ----------------------------------------------------------------------------

func TestEncodeTurnMetaOneRecordPerTurnPrecedenceSkip_Issue487(t *testing.T) {
	sess := model.NewModelSession("main", &issue487Conn{})
	// t0: usage only        -> "usage"
	sess.AddTurn(nil, "r0", &model.TokenUsage{PromptTokens: 10, TotalTokens: 11}, nil)
	// t1: error only        -> "turn_error"
	sess.AddTurn(nil, "r1", nil, &model.ModelError{Type: model.ErrorTimeout, Message: "timed out"})
	// t2: both error+usage  -> "turn_error" carrying top-level Usage
	sess.AddTurn(nil, "r2",
		&model.TokenUsage{PromptTokens: 20, TotalTokens: 21},
		&model.ModelError{Type: model.ErrorConnection, Message: "down", HTTPStatusCode: 503})
	// t3: neither           -> skipped
	sess.AddTurn(nil, "r3", nil, nil)

	root := agent.NewAgent("root", sess)
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	n, err := encodeTurnMeta(enc, []*agent.Agent{root}, func(string) int { return 0 })
	if err != nil {
		t.Fatalf("encodeTurnMeta: %v", err)
	}
	if n != 3 {
		t.Fatalf("encoded %d records, want 3 (the empty turn is skipped)", n)
	}

	var recs []jsonlRecord
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var r jsonlRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	if recs[0].Kind != "usage" || recs[0].Usage == nil || recs[0].Usage.PromptTokens != 10 {
		t.Errorf("rec0 = %+v, want a usage record with prompt=10", recs[0])
	}
	if recs[1].Kind != "turn_error" || recs[1].Err == nil || recs[1].Err.Type != string(model.ErrorTimeout) {
		t.Errorf("rec1 = %+v, want a turn_error of type timeout", recs[1])
	}
	if recs[1].Usage != nil {
		t.Errorf("rec1 (error-only turn) should not carry a top-level Usage, got %+v", recs[1].Usage)
	}
	if recs[2].Kind != "turn_error" || recs[2].Err == nil || recs[2].Err.Type != string(model.ErrorConnection) {
		t.Errorf("rec2 = %+v, want a turn_error of type connection (error takes precedence)", recs[2])
	}
	if recs[2].Usage == nil || recs[2].Usage.PromptTokens != 20 {
		t.Errorf("rec2 should carry the turn's usage on the top-level Usage field, got %+v", recs[2].Usage)
	}
	for i, r := range recs {
		if r.AgentID != "root" {
			t.Errorf("rec%d AgentID = %q, want root", i, r.AgentID)
		}
	}
}

// ----------------------------------------------------------------------------
// loadShard: reconstructs meta records, skips unknown kinds (forward/back-compat)
// ----------------------------------------------------------------------------

func TestLoadShardReconstructsMetaAndSkipsUnknownKinds_Issue487(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.0000.jsonl")
	body := strings.Join([]string{
		`{"kind":"message","agent_id":"root","message":{"role":"user","content":"hi"}}`,
		`{"kind":"usage","agent_id":"root","at":"2026-06-26T12:00:00Z","usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"cached_tokens":1}}`,
		`{"kind":"turn_error","agent_id":"root","at":"2026-06-26T12:00:01Z","error":{"type":"refusal","http_status_code":403,"message":"no"},"usage":{"prompt_tokens":9,"total_tokens":9}}`,
		`{"kind":"meta","agent_id":"root"}`,             // legacy/unknown kind → skipped
		`{"kind":"some_future_kind","agent_id":"root"}`, // unknown kind → skipped
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write shard: %v", err)
	}

	store := freshStoreOn(t, dir)
	records, err := store.loadShard(path)
	if err != nil {
		t.Fatalf("loadShard: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("loadShard returned %d records, want 3 (message + usage + turn_error; unknown kinds skipped): %+v", len(records), records)
	}
	if records[0].kind != "message" || records[0].msg.Content != "hi" {
		t.Errorf("record0 = %+v, want the user message", records[0])
	}
	if records[1].kind != "usage" || records[1].turn.Usage == nil || records[1].turn.Usage.PromptTokens != 5 || records[1].turn.Usage.CachedTokens != 1 {
		t.Errorf("record1 = %+v, want a usage turn with prompt=5 cached=1", records[1])
	}
	// The "at":"2026-06-26T12:00:00Z" field must round-trip into the Turn's Timestamp.
	if records[1].turn.Timestamp.IsZero() {
		t.Error("record1 Timestamp is zero, want it reconstructed from the 'at' field")
	}
	if records[2].kind != "turn_error" || records[2].turn.Error == nil || records[2].turn.Error.Type != model.ErrorRefusal || records[2].turn.Error.HTTPStatusCode != 403 {
		t.Errorf("record2 = %+v, want a turn_error turn with the refusal", records[2])
	}
	if records[2].turn.Usage == nil || records[2].turn.Usage.PromptTokens != 9 {
		t.Errorf("record2 should also carry the record's top-level usage, got %+v", records[2].turn.Usage)
	}
}

// ----------------------------------------------------------------------------
// loadTranscripts: meta records must NOT pollute the transcript map
// ----------------------------------------------------------------------------

func TestLoadTranscriptsKeepsMetaOutOfTranscript_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	conn := successConn(usageResp(10, 2, 0, 0))
	us, sess := newSessionWithConn("s-meta", conn)
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "hi"}}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := store.Save(us, "Meta"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-meta")
	msgs := ls.Transcripts["root"]
	// The transcript must hold only the user + assistant messages — the usage meta
	// record must not have been injected as a zero-value Message.
	if len(msgs) != 2 {
		t.Fatalf("transcript len = %d, want 2 (meta must stay out of the transcript): %+v", len(msgs), msgs)
	}
	for i, m := range msgs {
		if m.Role == "" || m.Content == "" {
			t.Errorf("transcript[%d] looks like an injected zero-value meta message: %+v", i, m)
		}
	}
	if len(ls.RootHistory) != 1 || ls.RootHistory[0].Usage == nil {
		t.Fatalf("RootHistory = %+v, want one usage turn", ls.RootHistory)
	}
}

// ----------------------------------------------------------------------------
// Integration: a failed turn through the Gogent entrypoint is persisted
// (call-site persist-on-error) with a turn_error record + partial transcript.
// ----------------------------------------------------------------------------

func TestFailedTurnIntegrationPersistsTurnErrorAndPartialTranscript_Issue487(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	// Neutralize model resolution so the agent keeps our fake connector (otherwise
	// the default "local-lan" config would rebuild a real connection over it).
	cfg := config.GetDefaultConfig()
	cfg.ModelConfigs = nil
	cfg.DefaultModel = ""
	g.config = cfg

	me := &model.ModelError{
		Type:           model.ErrorContextOverflow,
		Message:        "context window overflow",
		HTTPStatusCode: 400,
		RawResponse:    `{"error":"too long"}`,
	}
	sess := model.NewModelSession("main", errorConn(me))
	root := agent.NewAgent("root", sess)
	g.CreateUserSession("s-fail", root)

	resp, err := g.SendMessageToSessionWithModelAndEffort(context.Background(), "s-fail", "root", "hello world", "", "")
	if err == nil {
		t.Fatal("expected the failed turn to return an error, got nil")
	}
	if resp != nil {
		t.Errorf("expected nil response on a failed turn, got %+v", resp)
	}

	// Reload via a fresh store over the same sessions dir (simulates a restart).
	loaded, err := freshStoreOn(t, filepath.Join(home, ".gogent", "sessions")).ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-fail")

	// Partial transcript: the user message must be present even though the turn
	// failed (previously nothing was written on error).
	var sawUser bool
	for _, m := range ls.Transcripts["root"] {
		if m.Role == model.RoleUser && m.Content == "hello world" {
			sawUser = true
		}
	}
	if !sawUser {
		t.Errorf("user message missing from restored transcript: %+v", ls.Transcripts["root"])
	}

	// turn_error record reconstructed with the full classification.
	if len(ls.RootHistory) != 1 {
		t.Fatalf("RootHistory len = %d, want 1 (the failed round-trip): %+v", len(ls.RootHistory), ls.RootHistory)
	}
	got := ls.RootHistory[0].Error
	if got == nil {
		t.Fatal("RootHistory[0].Error is nil — the failure was not persisted")
	}
	if got.Type != model.ErrorContextOverflow || got.HTTPStatusCode != 400 || got.Message != "context window overflow" {
		t.Errorf("restored error = %+v, want the classified context_overflow", got)
	}
	if got.RawResponse != `{"error":"too long"}` {
		t.Errorf("restored RawResponse = %q, want the raw body", got.RawResponse)
	}
	if ls.RootHistory[0].Timestamp.IsZero() {
		t.Error("restored failure turn has no timestamp — the 'at' field was not persisted")
	}
}

// ----------------------------------------------------------------------------
// Integration: a successful turn persists a per-round-trip usage record.
// ----------------------------------------------------------------------------

func TestSuccessfulTurnIntegrationPersistsUsageRecord_Issue487(t *testing.T) {
	home := t.TempDir()
	g := NewGogent(home)
	cfg := config.GetDefaultConfig()
	cfg.ModelConfigs = nil
	cfg.DefaultModel = ""
	g.config = cfg

	sess := model.NewModelSession("main", successConn(usageResp(1234, 56, 100, 7)))
	root := agent.NewAgent("root", sess)
	g.CreateUserSession("s-ok", root)

	if _, err := g.SendMessageToSessionWithModelAndEffort(context.Background(), "s-ok", "root", "hello", "", ""); err != nil {
		t.Fatalf("successful turn errored: %v", err)
	}

	loaded, err := freshStoreOn(t, filepath.Join(home, ".gogent", "sessions")).ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-ok")
	if len(ls.RootHistory) != 1 || ls.RootHistory[0].Usage == nil {
		t.Fatalf("RootHistory = %+v, want one usage turn", ls.RootHistory)
	}
	u := ls.RootHistory[0].Usage
	if u.PromptTokens != 1234 || u.CompletionTokens != 56 || u.CachedTokens != 100 || u.ReasoningTokens != 7 {
		t.Errorf("restored usage = %+v, want prompt/completion/cached/reasoning preserved", u)
	}
	if ls.RootHistory[0].Error != nil {
		t.Errorf("a successful turn restored an error: %+v", ls.RootHistory[0].Error)
	}
}

// ----------------------------------------------------------------------------
// A turn_error record carries top-level usage when the provider returns usage
// alongside the error (the forward-compatible guard).
// ----------------------------------------------------------------------------

func TestTurnErrorRecordCarriesUsageWhenProviderReturnsIt_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	me := &model.ModelError{Type: model.ErrorConnection, Message: "reset", HTTPStatusCode: 502}
	conn := &issue487Conn{next: func() (*model.CompletionResponse, error) {
		return usageResp(4242, 0, 0, 0), me // resp-with-usage AND error
	}}
	us, sess := newSessionWithConn("s-eu", conn)
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected error")
	}
	if err := store.Save(us, "ErrUsage"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-eu")
	if len(ls.RootHistory) != 1 {
		t.Fatalf("RootHistory len = %d, want 1", len(ls.RootHistory))
	}
	turn := ls.RootHistory[0]
	if turn.Error == nil || turn.Error.Type != model.ErrorConnection {
		t.Errorf("restored error = %+v, want the connection error", turn.Error)
	}
	if turn.Usage == nil || turn.Usage.PromptTokens != 4242 {
		t.Errorf("restored usage = %+v, want prompt=4242 carried on the turn_error record", turn.Usage)
	}

	// Confirm the on-disk shape: a single turn_error record with BOTH error and
	// a top-level usage field (not usage nested inside the error payload).
	recs := parseShardFile(t, firstShardPath(t, dir))
	var te *jsonlRecord
	for i := range recs {
		if recs[i].Kind == "turn_error" {
			te = &recs[i]
		}
	}
	if te == nil {
		t.Fatal("no turn_error record on disk")
	}
	if te.Err == nil || te.Usage == nil {
		t.Errorf("on-disk turn_error = %+v, want both Err and a top-level Usage", te)
	}
}

// ----------------------------------------------------------------------------
// Restore: a session whose last turn failed reopens with the failure indicator
// and token accounting reconstructed, plus the full partial transcript.
// ----------------------------------------------------------------------------

func TestRestoreReconstructsFailureIndicatorAndUsage_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	// Scripted connector: round-trip 1 succeeds with usage, round-trip 2 fails.
	me := &model.ModelError{Type: model.ErrorRateLimit, Message: "slow down", HTTPStatusCode: 429, RawResponse: "rl"}
	step := 0
	conn := &issue487Conn{next: func() (*model.CompletionResponse, error) {
		step++
		if step == 1 {
			return usageResp(500, 50, 0, 0), nil
		}
		return nil, me
	}}
	us, sess := newSessionWithConn("s-multi", conn)

	// Round-trip 1: success.
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "first"}}, nil); err != nil {
		t.Fatalf("first send: %v", err)
	}
	// Round-trip 2: failure (leaves a half-transcript ending on "second").
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "second"}}, nil); err == nil {
		t.Fatal("expected the second send to fail")
	}
	if err := store.Save(us, "Multi"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload (simulated restart).
	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-multi")

	// RootHistory: usage turn then the failing turn, in order.
	if len(ls.RootHistory) != 2 {
		t.Fatalf("RootHistory len = %d, want 2: %+v", len(ls.RootHistory), ls.RootHistory)
	}
	if ls.RootHistory[0].Usage == nil || ls.RootHistory[0].Usage.PromptTokens != 500 {
		t.Errorf("RootHistory[0] = %+v, want the usage turn", ls.RootHistory[0])
	}
	last := ls.RootHistory[1]
	if last.Error == nil || last.Error.Type != model.ErrorRateLimit || last.Error.HTTPStatusCode != 429 {
		t.Errorf("RootHistory[1].Error = %+v, want the rate-limit failure (the indicator)", last.Error)
	}

	// adoptLoaded-equivalent: RestoreHistoryMeta seeds the count + indicator.
	restored := model.NewModelSession("main", &issue487Conn{})
	restored.ReplaceTranscript(ls.Transcripts["root"])
	restored.RestoreHistoryMeta(ls.RootHistory)
	if got, want := restored.GetCurrentTokenCount(), 550; got != want {
		t.Errorf("restored CurrentTokenCount = %d, want %d (the prior turn's context size)", got, want)
	}
	rh := restored.GetHistory()
	if len(rh) == 0 || rh[len(rh)-1].Error == nil {
		t.Errorf("restored History does not surface the failure indicator: %+v", rh)
	}

	// The partial transcript (user/assistant from round 1 + the unanswered round-2
	// user message) must be intact.
	var contents []string
	for _, m := range ls.Transcripts["root"] {
		contents = append(contents, string(m.Role)+":"+m.Content)
	}
	joined := strings.Join(contents, "|")
	if !strings.Contains(joined, "first") || !strings.Contains(joined, "second") {
		t.Errorf("partial transcript missing expected turns: %s", joined)
	}
}

// ----------------------------------------------------------------------------
// Backward-compat: a message-only shard (no meta records) loads cleanly, with no
// zero-value messages injected and no RootHistory.
// ----------------------------------------------------------------------------

func TestBackwardCompatMessageOnlyShardLoads_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	// buildSessionWithTranscript leaves History empty, so Save writes a message-
	// only shard (no usage/turn_error records) — representative of a shard written
	// before this change.
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "hello"},
		{Role: model.RoleAssistant, Content: "hi"},
	}
	us := buildSessionWithTranscript("s-old", msgs)
	if err := store.Save(us, "Old"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-old")
	if len(ls.Transcripts["root"]) != 2 {
		t.Errorf("transcript len = %d, want 2: %+v", len(ls.Transcripts["root"]), ls.Transcripts["root"])
	}
	if ls.RootHistory != nil {
		t.Errorf("RootHistory = %+v, want nil for a message-only shard", ls.RootHistory)
	}
}

// ----------------------------------------------------------------------------
// RawResponse truncation bounds shard growth from a large provider error body.
// ----------------------------------------------------------------------------

func TestRawResponseTruncation_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	huge := strings.Repeat("x", rawResponseCap*2) // ASCII → byte count is unambiguous
	us, sess := newSessionWithConn("s-big", errorConn(&model.ModelError{
		Type:           model.ErrorGeneric,
		Message:        "big body",
		HTTPStatusCode: 500,
		RawResponse:    huge,
	}))
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "hi"}}, nil); err == nil {
		t.Fatal("expected error")
	}
	if err := store.Save(us, "Big"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-big")
	raw := ls.RootHistory[0].Error.RawResponse
	if !strings.Contains(raw, "(truncated)") {
		t.Errorf("truncated RawResponse = %q..., want it to carry the truncation marker", safeHead(raw, 64))
	}
	// Cap + the marker length. Use >= so a JSON round-trip that can only shrink or
	// keep the byte count (ASCII) does not cause a spurious failure.
	if got, want := len(raw), rawResponseCap+len("…(truncated)"); got != want {
		t.Errorf("truncated RawResponse len = %d, want %d (cap %d + marker)", got, want, rawResponseCap)
	}
	if got, lim := len(raw), rawResponseCap*2; got >= lim {
		t.Errorf("truncated RawResponse len = %d, not bounded below the original %d", got, lim)
	}
}

// ----------------------------------------------------------------------------
// Meta frontier: after restore + a new turn + re-save, meta records are appended
// exactly once (the restored turn is NOT re-emitted, the new one is).
// ----------------------------------------------------------------------------

func TestMetaFrontierNoDuplicationAfterRestore_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	// Turn 1: persist a usage turn.
	us1, sess1 := newSessionWithConn("s-front", successConn(usageResp(100, 10, 0, 0)))
	if _, err := sess1.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "one"}}, nil); err != nil {
		t.Fatalf("send1: %v", err)
	}
	if err := store.Save(us1, "Front"); err != nil {
		t.Fatalf("Save1: %v", err)
	}

	// Restart: reload + adopt into a restored session.
	store2 := freshStoreOn(t, dir)
	loaded, err := store2.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-front")
	us2, sess2 := newSessionWithConn("s-front", successConn(usageResp(200, 20, 0, 0)))
	sess2.ReplaceTranscript(ls.Transcripts["root"])
	sess2.RestoreHistoryMeta(ls.RootHistory)
	store2.Adopt(ls.ID, ls.File, us2.RootAgent.ListAllAgents())

	// Turn 2: a new successful round-trip, then save.
	if _, err := sess2.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "two"}}, nil); err != nil {
		t.Fatalf("send2: %v", err)
	}
	if err := store2.Save(us2, ls.Title); err != nil {
		t.Fatalf("Save2: %v", err)
	}

	// Restart again and assert exactly two distinct usage turns — no duplication.
	loaded2, err := freshStoreOn(t, dir).ListActive()
	if err != nil {
		t.Fatalf("ListActive2: %v", err)
	}
	ls2 := findLoaded(t, loaded2, "s-front")
	if len(ls2.RootHistory) != 2 {
		t.Fatalf("RootHistory len = %d, want 2 (restored turn not re-emitted, new turn appended): %+v", len(ls2.RootHistory), ls2.RootHistory)
	}
	prompts := map[int]bool{}
	for _, turn := range ls2.RootHistory {
		if turn.Usage == nil {
			t.Fatalf("restored turn has no usage: %+v", turn)
		}
		if prompts[turn.Usage.PromptTokens] {
			t.Errorf("duplicated usage turn with prompt=%d — the restored turn was re-emitted", turn.Usage.PromptTokens)
		}
		prompts[turn.Usage.PromptTokens] = true
	}
	if !prompts[100] || !prompts[200] {
		t.Errorf("expected usage turns prompt=100 and prompt=200, got %v", prompts)
	}
}

// ----------------------------------------------------------------------------
// Happy-path no-regression: a successful turn persists and restores its
// transcript byte-for-byte as before (plus the now-recovered usage).
// ----------------------------------------------------------------------------

func TestHappyPathNoRegressionPersistsAndRestoresTranscript_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	us, sess := newSessionWithConn("s-happy", successConn(&model.CompletionResponse{
		Role:    model.RoleAssistant,
		Content: "the answer",
	}))
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "the question"}}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := store.Save(us, "Happy"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-happy")
	msgs := ls.Transcripts["root"]
	if len(msgs) != 2 || msgs[0].Content != "the question" || msgs[1].Content != "the answer" {
		t.Errorf("restored transcript = %+v, want [user=the question, assistant=the answer]", msgs)
	}
	// Message records remain byte-identical in kind: exactly two "message" records,
	// no stray usage/turn_error on a clean successful turn with no usage.
	recs := parseShardFile(t, firstShardPath(t, dir))
	var msgCount int
	for _, r := range recs {
		if r.Kind == "message" {
			msgCount++
		}
	}
	if msgCount != 2 {
		t.Errorf("message records = %d, want 2 (happy-path message shape unchanged)", msgCount)
	}
}

// ----------------------------------------------------------------------------
// Compaction full-rewrite: an in-place transcript replacement (epoch advance,
// as a compaction performs) forces writeFullTranscript, which must re-emit the
// full meta stream from History idempotently — no loss, no duplication.
// ----------------------------------------------------------------------------

func TestCompactionFullRewritePreservesMeta_Issue487(t *testing.T) {
	dir := t.TempDir()
	store := freshStoreOn(t, dir)

	us, sess := newSessionWithConn("s-rw", successConn(usageResp(100, 10, 0, 0)))
	if _, err := sess.SendWithToolsCtx(context.Background(), []model.Message{{Role: model.RoleUser, Content: "one"}}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := store.Save(us, "RW"); err != nil { // first save = full write
		t.Fatalf("Save1: %v", err)
	}

	// Simulate a compaction: rewrite the transcript in place. ReplaceTranscript
	// bumps the epoch even for identical content, so the next Save detects the
	// advance and takes the writeFullTranscript branch.
	sess.ReplaceTranscript(sess.GetTranscript())
	if err := store.Save(us, "RW"); err != nil {
		t.Fatalf("Save2 (full rewrite): %v", err)
	}

	// Reload: the usage turn must survive the rewrite exactly once.
	loaded, err := freshStoreOn(t, dir).ListActive()
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	ls := findLoaded(t, loaded, "s-rw")
	if len(ls.RootHistory) != 1 {
		t.Fatalf("RootHistory len = %d, want 1 (meta re-emitted once, not lost or duplicated): %+v", len(ls.RootHistory), ls.RootHistory)
	}
	if ls.RootHistory[0].Usage == nil || ls.RootHistory[0].Usage.PromptTokens != 100 {
		t.Errorf("RootHistory[0] = %+v, want the usage turn preserved across the rewrite", ls.RootHistory[0])
	}
	// Only one shard should exist (the rewrite rebuilds the set, dropping orphans).
	shard := firstShardPath(t, dir)
	recs := parseShardFile(t, shard)
	var usageCount int
	for _, r := range recs {
		if r.Kind == "usage" {
			usageCount++
		}
	}
	if usageCount != 1 {
		t.Errorf("usage records on disk = %d, want 1 (rewrite replaced the shard set, not appended)", usageCount)
	}
}

// safeHead returns the first n bytes of s as a string for safe error logging.
func safeHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
