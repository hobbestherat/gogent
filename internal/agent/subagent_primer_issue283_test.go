package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gogent/internal/model"
)

// Tests for issue #283: reduce sub-agent spin-up overhead by pre-seeding a
// bounded context primer (subAgentPrimer), folding it into the first user
// message (SeededMessage), and trimming the one-shot prompt. These exercise the
// helpers white-box; do not modify the implementation.

// callMsg builds an assistant transcript message carrying one native tool call
// with JSON-encoded arguments, matching how the model records calls.
func callMsg(name string, args map[string]interface{}) model.Message {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return model.Message{
		Role: model.RoleAssistant,
		ToolCalls: []model.ToolCall{
			{
				Type:     "function",
				Function: model.FunctionCall{Name: name, Arguments: string(raw)},
			},
		},
	}
}

// rawCallMsg builds an assistant message with verbatim (possibly malformed)
// argument bytes, so tests can drive the json.Unmarshal failure path.
func rawCallMsg(name, rawArgs string) model.Message {
	return model.Message{
		Role: model.RoleAssistant,
		ToolCalls: []model.ToolCall{
			{
				Type:     "function",
				Function: model.FunctionCall{Name: name, Arguments: rawArgs},
			},
		},
	}
}

// parentWith returns an Agent whose transcript holds the given messages.
func parentWith(msgs ...model.Message) *Agent {
	sess := model.NewModelSession("parent", nil)
	sess.AppendMessages(msgs...)
	return NewAgent("parent", sess)
}

// --- subAgentPrimer: empty / nil cases -------------------------------------

func TestSubAgentPrimerNilParent(t *testing.T) {
	if got := subAgentPrimer(nil); got != "" {
		t.Fatalf("nil parent: want empty primer, got %q", got)
	}
}

func TestSubAgentPrimerNilThoughtTrain(t *testing.T) {
	a := &Agent{} // ThoughtTrain is nil
	if got := subAgentPrimer(a); got != "" {
		t.Fatalf("nil ThoughtTrain: want empty primer, got %q", got)
	}
}

func TestSubAgentPrimerEmptyTranscript(t *testing.T) {
	if got := subAgentPrimer(parentWith()); got != "" {
		t.Fatalf("empty transcript: want empty primer, got %q", got)
	}
}

func TestSubAgentPrimerNoRelevantTools(t *testing.T) {
	// Tools that are neither path nor search tools must not produce a primer.
	p := parentWith(
		callMsg("spawn_agent", map[string]interface{}{"task": "do thing"}),
		callMsg("think", map[string]interface{}{"thought": "hmm"}),
	)
	if got := subAgentPrimer(p); got != "" {
		t.Fatalf("non-discovery tools: want empty primer, got %q", got)
	}
}

// --- subAgentPrimer: path collection ---------------------------------------

func TestSubAgentPrimerCollectsPaths(t *testing.T) {
	p := parentWith(
		callMsg("read", map[string]interface{}{"path": "internal/agent/agent.go"}),
		callMsg("edit", map[string]interface{}{"path": "internal/agent/user_session.go"}),
		callMsg("list", map[string]interface{}{"path": "internal/model"}),
		callMsg("write", map[string]interface{}{"path": "docs/new.md"}),
	)
	got := subAgentPrimer(p)
	for _, want := range []string{
		"internal/agent/agent.go",
		"internal/agent/user_session.go",
		"internal/model",
		"docs/new.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("primer missing path %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Files/paths already inspected") {
		t.Errorf("primer missing files header:\n%s", got)
	}
	// The authority/no-rediscover guidance must be present (G3).
	if !strings.Contains(got, "authoritative") {
		t.Errorf("primer missing authoritative framing:\n%s", got)
	}
}

func TestSubAgentPrimerDedupsPaths(t *testing.T) {
	p := parentWith(
		callMsg("read", map[string]interface{}{"path": "a.go"}),
		callMsg("read", map[string]interface{}{"path": "a.go"}),
		callMsg("edit", map[string]interface{}{"path": "a.go"}),
	)
	got := subAgentPrimer(p)
	if n := strings.Count(got, "- a.go"); n != 1 {
		t.Fatalf("path a.go listed %d times, want 1:\n%s", n, got)
	}
}

func TestSubAgentPrimerSkipsEmptyAndWhitespacePath(t *testing.T) {
	p := parentWith(
		callMsg("read", map[string]interface{}{"path": ""}),
		callMsg("read", map[string]interface{}{"path": "   "}),
	)
	if got := subAgentPrimer(p); got != "" {
		t.Fatalf("empty/whitespace paths should yield no primer, got %q", got)
	}
}

func TestSubAgentPrimerTrimsPathWhitespace(t *testing.T) {
	p := parentWith(callMsg("read", map[string]interface{}{"path": "  spaced.go  "}))
	got := subAgentPrimer(p)
	if strings.Contains(got, "  spaced.go  ") {
		t.Fatalf("path not trimmed:\n%s", got)
	}
	if !strings.Contains(got, "spaced.go") {
		t.Fatalf("trimmed path missing:\n%s", got)
	}
}

func TestSubAgentPrimerCapsPaths(t *testing.T) {
	var msgs []model.Message
	for i := 0; i < maxPrimerPaths+10; i++ {
		msgs = append(msgs, callMsg("read", map[string]interface{}{"path": fmt.Sprintf("file%03d.go", i)}))
	}
	got := subAgentPrimer(parentWith(msgs...))
	// Only the first maxPrimerPaths distinct paths are kept.
	if !strings.Contains(got, "file000.go") {
		t.Errorf("first path dropped:\n%s", got)
	}
	if strings.Contains(got, fmt.Sprintf("file%03d.go", maxPrimerPaths)) {
		t.Errorf("path beyond cap (index %d) leaked into primer:\n%s", maxPrimerPaths, got)
	}
	// Count listed file paths; must not exceed the cap.
	if n := strings.Count(got, ".go"); n > maxPrimerPaths {
		t.Errorf("listed %d paths, exceeds cap %d", n, maxPrimerPaths)
	}
}

// --- subAgentPrimer: search collection -------------------------------------

func TestSubAgentPrimerCollectsSearches(t *testing.T) {
	p := parentWith(
		callMsg("grep", map[string]interface{}{"pattern": "newSubAgent", "path": "internal/agent"}),
		callMsg("grep", map[string]interface{}{"pattern": "globalThing"}),
		callMsg("glob", map[string]interface{}{"pattern": "**/*.go"}),
	)
	got := subAgentPrimer(p)
	if !strings.Contains(got, "Searches already run") {
		t.Errorf("primer missing searches header:\n%s", got)
	}
	if !strings.Contains(got, `grep "newSubAgent" in internal/agent`) {
		t.Errorf("primer missing grep-with-path descriptor:\n%s", got)
	}
	if !strings.Contains(got, `grep "globalThing"`) {
		t.Errorf("primer missing grep-no-path descriptor:\n%s", got)
	}
	if !strings.Contains(got, `glob "**/*.go"`) {
		t.Errorf("primer missing glob descriptor:\n%s", got)
	}
}

func TestSubAgentPrimerDedupsSearches(t *testing.T) {
	p := parentWith(
		callMsg("grep", map[string]interface{}{"pattern": "foo"}),
		callMsg("grep", map[string]interface{}{"pattern": "foo"}),
	)
	got := subAgentPrimer(p)
	if n := strings.Count(got, `grep "foo"`); n != 1 {
		t.Fatalf("grep foo listed %d times, want 1:\n%s", n, got)
	}
}

func TestSubAgentPrimerSkipsSearchWithoutPattern(t *testing.T) {
	p := parentWith(
		callMsg("grep", map[string]interface{}{"path": "internal/agent"}), // no pattern
		callMsg("glob", map[string]interface{}{}),                         // no pattern
	)
	if got := subAgentPrimer(p); got != "" {
		t.Fatalf("searches without patterns should yield no primer, got %q", got)
	}
}

func TestSubAgentPrimerCapsSearches(t *testing.T) {
	var msgs []model.Message
	for i := 0; i < maxPrimerSearches+5; i++ {
		msgs = append(msgs, callMsg("grep", map[string]interface{}{"pattern": fmt.Sprintf("pat%02d", i)}))
	}
	got := subAgentPrimer(parentWith(msgs...))
	if n := strings.Count(got, "grep "); n > maxPrimerSearches {
		t.Errorf("listed %d searches, exceeds cap %d:\n%s", n, maxPrimerSearches, got)
	}
	if strings.Contains(got, fmt.Sprintf("pat%02d", maxPrimerSearches)) {
		t.Errorf("search beyond cap leaked:\n%s", got)
	}
}

// --- subAgentPrimer: malformed args & mixed messages -----------------------

func TestSubAgentPrimerSkipsMalformedArgs(t *testing.T) {
	p := parentWith(
		rawCallMsg("read", "{not valid json"),
		callMsg("read", map[string]interface{}{"path": "good.go"}),
	)
	got := subAgentPrimer(p)
	if strings.Contains(got, "not valid") {
		t.Errorf("malformed args leaked into primer:\n%s", got)
	}
	if !strings.Contains(got, "good.go") {
		t.Errorf("valid call after malformed one was dropped:\n%s", got)
	}
}

func TestSubAgentPrimerNonStringPathIgnored(t *testing.T) {
	// path as a number — stringField returns "", so nothing is collected.
	p := parentWith(rawCallMsg("read", `{"path": 123}`))
	if got := subAgentPrimer(p); got != "" {
		t.Fatalf("non-string path should be ignored, got %q", got)
	}
}

func TestSubAgentPrimerMixedPathsAndSearches(t *testing.T) {
	p := parentWith(
		callMsg("read", map[string]interface{}{"path": "x.go"}),
		callMsg("grep", map[string]interface{}{"pattern": "y"}),
	)
	got := subAgentPrimer(p)
	if !strings.Contains(got, "Files/paths already inspected") || !strings.Contains(got, "Searches already run") {
		t.Fatalf("expected both sections:\n%s", got)
	}
	if !strings.Contains(got, "x.go") || !strings.Contains(got, `grep "y"`) {
		t.Fatalf("expected both entries:\n%s", got)
	}
}

func TestSubAgentPrimerMultipleCallsPerMessage(t *testing.T) {
	// A single assistant message can batch several tool calls.
	msg := model.Message{
		Role: model.RoleAssistant,
		ToolCalls: []model.ToolCall{
			{Function: model.FunctionCall{Name: "read", Arguments: `{"path":"a.go"}`}},
			{Function: model.FunctionCall{Name: "read", Arguments: `{"path":"b.go"}`}},
		},
	}
	got := subAgentPrimer(parentWith(msg))
	if !strings.Contains(got, "a.go") || !strings.Contains(got, "b.go") {
		t.Fatalf("batched tool calls not both captured:\n%s", got)
	}
}

// --- subAgentPrimer: bounded-size guarantee --------------------------------

func TestSubAgentPrimerIsBounded(t *testing.T) {
	// Long paths that would blow past maxPrimerBytes if not truncated.
	var msgs []model.Message
	long := strings.Repeat("a", 200)
	for i := 0; i < maxPrimerPaths; i++ {
		msgs = append(msgs, callMsg("read", map[string]interface{}{"path": fmt.Sprintf("%s/%03d.go", long, i)}))
	}
	got := subAgentPrimer(parentWith(msgs...))
	if len(got) > maxPrimerBytes {
		t.Fatalf("primer length %d exceeds maxPrimerBytes %d", len(got), maxPrimerBytes)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("oversized primer not marked truncated:\n%s", got)
	}
}

func TestSubAgentPrimerDoesNotLeakTranscriptContents(t *testing.T) {
	// File contents / reasoning must never enter the primer — only references.
	secret := "SUPER_SECRET_FILE_BODY"
	msgs := []model.Message{
		{Role: model.RoleAssistant, Content: "my private reasoning " + secret},
		{Role: model.RoleTool, Content: "tool result containing " + secret},
		callMsg("read", map[string]interface{}{"path": "real.go"}),
	}
	got := subAgentPrimer(parentWith(msgs...))
	if strings.Contains(got, secret) {
		t.Fatalf("primer leaked transcript content:\n%s", got)
	}
	if !strings.Contains(got, "real.go") {
		t.Fatalf("primer dropped the legitimate path:\n%s", got)
	}
}

// --- SeededMessage ----------------------------------------------------------

func TestSeededMessageNilAgent(t *testing.T) {
	var a *Agent
	if got := a.SeededMessage("task"); got != "task" {
		t.Fatalf("nil agent: want task unchanged, got %q", got)
	}
}

func TestSeededMessageEmptySeed(t *testing.T) {
	a := &Agent{}
	if got := a.SeededMessage("do it"); got != "do it" {
		t.Fatalf("empty seed: want task unchanged, got %q", got)
	}
}

func TestSeededMessageWhitespaceSeed(t *testing.T) {
	a := &Agent{SeedContext: "   \n  "}
	if got := a.SeededMessage("do it"); got != "do it" {
		t.Fatalf("whitespace seed: want task unchanged, got %q", got)
	}
}

func TestSeededMessagePrependsSeed(t *testing.T) {
	a := &Agent{SeedContext: "PRIMER"}
	want := "PRIMER\n\ntask body"
	if got := a.SeededMessage("task body"); got != want {
		t.Fatalf("SeededMessage = %q, want %q", got, want)
	}
}

func TestSeededMessagePreservesTask(t *testing.T) {
	a := &Agent{SeedContext: "P"}
	got := a.SeededMessage("the original task")
	if !strings.HasSuffix(got, "the original task") {
		t.Fatalf("task not preserved at end: %q", got)
	}
}

// --- truncatePrimer ---------------------------------------------------------

func TestTruncatePrimerUnderMax(t *testing.T) {
	s := "short string"
	if got := truncatePrimer(s, 100); got != s {
		t.Fatalf("under-max input changed: %q", got)
	}
}

func TestTruncatePrimerAtExactMax(t *testing.T) {
	s := "exactly ten"[:10] // 10 bytes
	if got := truncatePrimer(s, 10); got != s {
		t.Fatalf("at-max input changed: %q", got)
	}
}

func TestTruncatePrimerOverMaxRespectsBound(t *testing.T) {
	s := "line one\nline two\nline three\nline four\nline five"
	max := 25
	got := truncatePrimer(s, max)
	if len(got) > max {
		t.Fatalf("truncated length %d exceeds max %d: %q", len(got), max, got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("missing truncation marker: %q", got)
	}
}

func TestTruncatePrimerCutsOnLineBoundary(t *testing.T) {
	// A path on its own line must not be sliced mid-token; the kept prefix
	// should end at a newline boundary before the suffix.
	s := "header\n- /very/long/path/that/should/not/be/cut/in/half.go\n- /b.go"
	got := truncatePrimer(s, 40)
	if len(got) > 40 {
		t.Fatalf("length %d exceeds 40", len(got))
	}
	// The surviving content before the marker should be a clean prefix of s.
	marker := "\n- … (truncated)"
	body := strings.TrimSuffix(got, marker)
	if !strings.HasPrefix(s, body) {
		t.Fatalf("truncated body %q is not a clean prefix of original", body)
	}
}

func TestTruncatePrimerTinyMax(t *testing.T) {
	// max smaller than the suffix must not panic and must still respect bound==max
	// only as far as the implementation guarantees (budget floored at 0).
	s := "some long content here that exceeds"
	got := truncatePrimer(s, 3)
	// Should not panic; result is the suffix-only form (budget 0 -> empty body).
	if !strings.Contains(got, "truncated") {
		t.Fatalf("tiny max: expected truncation marker, got %q", got)
	}
}

// --- describePrimerSearch ---------------------------------------------------

func TestDescribePrimerSearchGrepWithPath(t *testing.T) {
	got := describePrimerSearch("grep", map[string]interface{}{"pattern": "foo", "path": "dir"})
	if got != `grep "foo" in dir` {
		t.Fatalf("got %q", got)
	}
}

func TestDescribePrimerSearchGrepNoPath(t *testing.T) {
	got := describePrimerSearch("grep", map[string]interface{}{"pattern": "foo"})
	if got != `grep "foo"` {
		t.Fatalf("got %q", got)
	}
}

func TestDescribePrimerSearchGlob(t *testing.T) {
	got := describePrimerSearch("glob", map[string]interface{}{"pattern": "*.go"})
	if got != `glob "*.go"` {
		t.Fatalf("got %q", got)
	}
}

func TestDescribePrimerSearchEmptyPattern(t *testing.T) {
	if got := describePrimerSearch("grep", map[string]interface{}{"pattern": "  "}); got != "" {
		t.Fatalf("blank pattern: want empty, got %q", got)
	}
	if got := describePrimerSearch("glob", map[string]interface{}{}); got != "" {
		t.Fatalf("missing pattern: want empty, got %q", got)
	}
}

func TestDescribePrimerSearchUnknownTool(t *testing.T) {
	if got := describePrimerSearch("read", map[string]interface{}{"pattern": "x"}); got != "" {
		t.Fatalf("unknown tool: want empty, got %q", got)
	}
}

// --- stringField ------------------------------------------------------------

func TestStringField(t *testing.T) {
	args := map[string]interface{}{"s": "val", "n": 42, "b": true}
	if got := stringField(args, "s"); got != "val" {
		t.Errorf("string value: got %q", got)
	}
	if got := stringField(args, "missing"); got != "" {
		t.Errorf("missing key: got %q", got)
	}
	if got := stringField(args, "n"); got != "" {
		t.Errorf("number value: want empty, got %q", got)
	}
	if got := stringField(args, "b"); got != "" {
		t.Errorf("bool value: want empty, got %q", got)
	}
}

// --- leaner one-shot prompt (G2) -------------------------------------------

func TestOneShotPromptCarriesContract(t *testing.T) {
	if !strings.Contains(subAgentOneShotPrompt, "SUCCESS:") || !strings.Contains(subAgentOneShotPrompt, "FAILURE:") {
		t.Fatalf("one-shot prompt missing SUCCESS/FAILURE contract:\n%s", subAgentOneShotPrompt)
	}
}

func TestOneShotPromptHasPathTrustGuidance(t *testing.T) {
	// G3: the prompt must tell the child to trust provided paths and not re-grep.
	low := strings.ToLower(subAgentOneShotPrompt)
	if !strings.Contains(low, "authoritative") {
		t.Fatalf("one-shot prompt missing path-trust guidance:\n%s", subAgentOneShotPrompt)
	}
}

func TestOneShotPromptIsLeanerThanInteractive(t *testing.T) {
	// G2: the one-shot persona should be trimmed relative to the interactive one.
	// The interactive prompt opens with the verbose persona line; the lean
	// one-shot prompt should not.
	if strings.HasPrefix(subAgentOneShotPrompt, "You are a") {
		t.Errorf("one-shot prompt still leads with broad persona framing:\n%s", subAgentOneShotPrompt)
	}
}

// --- end-to-end: newSubAgent seeds the child -------------------------------
// These confirm the primer is wired into SeededMessage so the child's first
// message references parent-known paths without a discovery round-trip.

func TestSeededMessageUsesPrimerEndToEnd(t *testing.T) {
	primer := subAgentPrimer(parentWith(
		callMsg("read", map[string]interface{}{"path": "internal/agent/agent.go"}),
	))
	if primer == "" {
		t.Fatal("expected a non-empty primer for the parent")
	}
	child := &Agent{SeedContext: primer}
	msg := child.SeededMessage("Summarize internal/agent/agent.go")
	if !strings.Contains(msg, "internal/agent/agent.go") {
		t.Fatalf("seeded child message lacks the parent-known path:\n%s", msg)
	}
	if !strings.HasSuffix(msg, "Summarize internal/agent/agent.go") {
		t.Fatalf("task not appended after primer:\n%s", msg)
	}
}
