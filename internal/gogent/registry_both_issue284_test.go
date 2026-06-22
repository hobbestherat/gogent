package gogent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/internal/tool"
)

// Issue #284: in the default "both" execution model a single session must expose
// BOTH the blocking spawn_subagent and the asynchronous launch_agent family, and
// the two engines must work side by side. These tests cover the registry
// filtering matrix, default-session wiring, runtime mode switching, and the
// end-to-end concurrency contract (launch returns immediately; the parent keeps
// control; wait_agent_event later harvests the child's result).

const (
	spawnTool = "spawn_subagent"
)

var interactiveTools = []string{"launch_agent", "agent_status", "agent_send", "agent_terminate", "wait_agent_event"}

// registryNames returns the set of tool names present in a registry.
func registryNames(reg *tool.ToolRegistry) map[string]bool {
	names := map[string]bool{}
	for _, tl := range reg.List() {
		names[tl.Name] = true
	}
	return names
}

func hasTool(reg *tool.ToolRegistry, name string) bool {
	return reg.Get(name) != nil
}

// TestToolRegistryForModeMatrix pins exactly which coordination tools survive
// toolRegistryForMode for each execution model.
func TestToolRegistryForModeMatrix(t *testing.T) {
	g := NewGogent(t.TempDir())

	cases := []struct {
		name        string
		model       config.SubAgentExecutionModel
		wantSpawn   bool
		wantInterac bool
	}{
		{"both", config.SubAgentBothModel, true, true},
		{"one_shot", config.SubAgentOneShotModel, true, false},
		{"interactive", config.SubAgentInteractiveModel, false, true},
		{"empty_default", config.SubAgentExecutionModel(""), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := g.toolRegistryForMode(config.SubAgentConfig{ExecutionModel: tc.model})

			if got := hasTool(reg, spawnTool); got != tc.wantSpawn {
				t.Errorf("spawn_subagent present = %v, want %v", got, tc.wantSpawn)
			}
			for _, name := range interactiveTools {
				if got := hasTool(reg, name); got != tc.wantInterac {
					t.Errorf("interactive tool %q present = %v, want %v", name, got, tc.wantInterac)
				}
			}

			// Non-coordination tools must always survive regardless of mode (the
			// filter must strip ONLY the coordination tools it targets).
			for _, keep := range []string{"read", "write", "grep", "todo"} {
				if g.GetToolRegistry().Get(keep) == nil {
					t.Fatalf("test assumption broken: tool %q not registered", keep)
				}
				if !hasTool(reg, keep) {
					t.Errorf("mode filter wrongly stripped non-coordination tool %q", keep)
				}
			}
		})
	}
}

// TestToolRegistryForModeBothIsSupersetOfEach asserts the "both" registry equals
// the union of the one-shot and interactive registries over the coordination
// tools — i.e. it strips nothing the other two keep.
func TestToolRegistryForModeBothIsSupersetOfEach(t *testing.T) {
	g := NewGogent(t.TempDir())
	both := registryNames(g.toolRegistryForMode(config.SubAgentConfig{ExecutionModel: config.SubAgentBothModel}))
	one := registryNames(g.toolRegistryForMode(config.SubAgentConfig{ExecutionModel: config.SubAgentOneShotModel}))
	inter := registryNames(g.toolRegistryForMode(config.SubAgentConfig{ExecutionModel: config.SubAgentInteractiveModel}))

	for name := range one {
		if !both[name] {
			t.Errorf("both-mode registry missing %q present in one-shot mode", name)
		}
	}
	for name := range inter {
		if !both[name] {
			t.Errorf("both-mode registry missing %q present in interactive mode", name)
		}
	}
}

// TestDefaultSessionExposesBothStyles is the headline acceptance test: a freshly
// created session (default config) hands its root agent a registry containing
// both spawn_subagent and the full launch_agent family, with no mode switch.
func TestDefaultSessionExposesBothStyles(t *testing.T) {
	g := NewGogent(t.TempDir())
	if g.SubAgentSettings().ExecutionModel != config.SubAgentBothModel {
		t.Fatalf("expected default 'both' config, got %q", g.SubAgentSettings().ExecutionModel)
	}
	if g.SubAgentOneShot() {
		t.Error("default session should NOT report one-shot mode")
	}

	sess := g.NewSession("s1")
	reg := sess.RootAgent.ToolRegistry
	if reg == nil {
		t.Fatal("root agent has no tool registry")
	}
	if !hasTool(reg, spawnTool) {
		t.Error("default session registry missing spawn_subagent")
	}
	for _, name := range interactiveTools {
		if !hasTool(reg, name) {
			t.Errorf("default session registry missing interactive tool %q", name)
		}
	}
}

// TestSetSubAgentSettingsRefreshesRegistry verifies runtime mode switching
// re-filters the live session's registry, both ways.
func TestSetSubAgentSettingsRefreshesRegistry(t *testing.T) {
	g := NewGogent(t.TempDir())
	sess := g.NewSession("s1")

	// Switch to one-shot only: async family must disappear, spawn stays.
	g.SetSubAgentSettings(config.SubAgentConfig{ExecutionModel: config.SubAgentOneShotModel})
	reg := sess.RootAgent.ToolRegistry
	if !hasTool(reg, spawnTool) {
		t.Error("one-shot mode dropped spawn_subagent")
	}
	for _, name := range interactiveTools {
		if hasTool(reg, name) {
			t.Errorf("one-shot mode left interactive tool %q registered", name)
		}
	}

	// Switch to interactive only: spawn disappears, async family returns.
	g.SetSubAgentSettings(config.SubAgentConfig{ExecutionModel: config.SubAgentInteractiveModel})
	reg = sess.RootAgent.ToolRegistry
	if hasTool(reg, spawnTool) {
		t.Error("interactive mode left spawn_subagent registered")
	}
	for _, name := range interactiveTools {
		if !hasTool(reg, name) {
			t.Errorf("interactive mode missing interactive tool %q", name)
		}
	}

	// Back to both.
	g.SetSubAgentSettings(config.SubAgentConfig{ExecutionModel: config.SubAgentBothModel})
	reg = sess.RootAgent.ToolRegistry
	if !hasTool(reg, spawnTool) {
		t.Error("both mode missing spawn_subagent")
	}
	for _, name := range interactiveTools {
		if !hasTool(reg, name) {
			t.Errorf("both mode missing interactive tool %q", name)
		}
	}
}

// TestSetSubAgentOneShotLegacyToggle documents the legacy binary toggle: true ->
// one_shot only, false -> interactive only (never "both"). This guards against an
// accidental change to the toggle's mapping.
func TestSetSubAgentOneShotLegacyToggle(t *testing.T) {
	g := NewGogent(t.TempDir())

	g.SetSubAgentOneShot(true)
	if got := g.SubAgentSettings().ExecutionModel; got != config.SubAgentOneShotModel {
		t.Errorf("SetSubAgentOneShot(true) => %q, want one_shot", got)
	}
	if !g.SubAgentOneShot() {
		t.Error("SubAgentOneShot() should be true after SetSubAgentOneShot(true)")
	}

	g.SetSubAgentOneShot(false)
	if got := g.SubAgentSettings().ExecutionModel; got != config.SubAgentInteractiveModel {
		t.Errorf("SetSubAgentOneShot(false) => %q, want interactive", got)
	}
	if g.SubAgentOneShot() {
		t.Error("SubAgentOneShot() should be false after SetSubAgentOneShot(false)")
	}
}

// TestBothModePlanModeStripsAsyncKeepsSpawn validates the #281/#284 interaction:
// in the default "both" mode, the plan-mode filter (CloneForPlanMode with the
// kept tools) must retain the blocking spawn_subagent for read-only fan-out yet
// strip the non-read-only launch_agent family — which is exactly what the
// plan-mode prompt promises ("the async launch_agent family is NOT available this
// turn"). If the async tools were accidentally read-only or kept, the prompt
// would lie and the model could emit calls that fail this turn.
func TestBothModePlanModeStripsAsyncKeepsSpawn(t *testing.T) {
	g := NewGogent(t.TempDir())
	both := g.toolRegistryForMode(config.SubAgentConfig{ExecutionModel: config.SubAgentBothModel})

	// Mirror agent.planKeptTools (unexported): the plan-mode turn keeps these.
	planKept := []string{"todo", "structured_output", "spawn_subagent"}
	planReg := both.CloneForPlanMode(planKept...)

	if !hasTool(planReg, spawnTool) {
		t.Error("plan mode (both) should keep spawn_subagent for read-only delegation")
	}
	for _, name := range interactiveTools {
		if hasTool(planReg, name) {
			t.Errorf("plan mode (both) should strip async tool %q (prompt says it is unavailable)", name)
		}
	}
	// Read-only investigation tools must still be present so planning can proceed.
	for _, ro := range []string{"read", "grep", "glob", "list"} {
		if g.GetToolRegistry().Get(ro) == nil {
			t.Fatalf("test assumption broken: read-only tool %q not registered", ro)
		}
		if !hasTool(planReg, ro) {
			t.Errorf("plan mode wrongly stripped read-only tool %q", ro)
		}
	}
}

// gatedModelServer answers model requests with a fixed final reply, but only
// after its release channel is closed. This lets a test hold a sub-agent
// mid-flight to prove the parent is not blocked, then release it to harvest the
// result. requests counts how many model round-trips arrived.
type gatedModelServer struct {
	release  chan struct{}
	requests int64
	reply    string
}

func newGatedModelServer(reply string) *gatedModelServer {
	return &gatedModelServer{release: make(chan struct{}), reply: reply}
}

func (s *gatedModelServer) Release() {
	select {
	case <-s.release: // already closed
	default:
		close(s.release)
	}
}

func (s *gatedModelServer) handler(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.requests, 1)
	<-s.release
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       map[string]interface{}{"role": "assistant", "content": s.reply},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
	})
}

// newBothModeSession spins up a Gogent + session whose model backend points at
// the given server URL, in the default "both" mode.
func newBothModeSession(t *testing.T, serverURL string) (*Gogent, *agent.UserSession) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "gogent-284-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tempDir) })
	workspace := filepath.Join(tempDir, ".gogent", "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}

	g := NewGogentWithWorkspace(tempDir, workspace)
	conn := model.NewModelConnection()
	conn.SetURL(serverURL)
	sess := model.NewModelSession("session_284", conn)
	root := agent.NewAgent("root", sess)
	us := g.CreateUserSession("session_284", root)
	return g, us
}

func execTool(t *testing.T, g *Gogent, name string, args map[string]interface{}) (*tool.ToolCallResponse, error) {
	t.Helper()
	call := &tool.ToolCall{Tool: name, Args: args}
	return g.GetToolRegistry().ExecuteToolCall(call, tool.ToolContext{
		SessionID: "session_284",
		AgentID:   "root",
		Context:   context.Background(),
	})
}

// TestInteractiveLaunchDoesNotBlockParent is the core concurrency contract of
// issue #284: launch_agent returns an agent_id IMMEDIATELY (before the child's
// model work finishes), the parent can keep issuing tool calls while the child
// runs, and wait_agent_event eventually harvests the completed result.
func TestInteractiveLaunchDoesNotBlockParent(t *testing.T) {
	srv := newGatedModelServer("SUCCESS: research done")
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()
	defer srv.Release() // ensure the child goroutine is never left blocked

	g, _ := newBothModeSession(t, server.URL)

	// Launch while the model is gated (child cannot finish yet).
	resp, err := execTool(t, g, "launch_agent", map[string]interface{}{"name": "research", "task": "research X"})
	if err != nil {
		t.Fatalf("launch_agent errored: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("launch_agent unsuccessful: %+v", resp)
	}
	result, _ := resp.Result.(map[string]interface{})
	agentID, _ := result["agent_id"].(string)
	if strings.TrimSpace(agentID) == "" {
		t.Fatalf("launch_agent returned empty agent_id: %+v", resp.Result)
	}

	// Parent is NOT blocked: it can call another tool while the child is still
	// running. agent_status should report a non-terminal state (running/waiting).
	statusResp, err := execTool(t, g, "agent_status", map[string]interface{}{"agent_id": agentID})
	if err != nil {
		t.Fatalf("agent_status errored: %v", err)
	}
	sres, _ := statusResp.Result.(map[string]interface{})
	if st, _ := sres["status"].(string); st == string(agent.StatusCompleted) || st == string(agent.StatusFailed) {
		t.Errorf("child reported terminal status %q while model still gated; expected it to still be running", st)
	}

	// wait_agent_event with a short timeout proves the parent regains control
	// without the child having finished (the model is still gated).
	waitResp, err := execTool(t, g, "wait_agent_event", map[string]interface{}{"timeout_ms": float64(150)})
	if err != nil {
		t.Fatalf("wait_agent_event (gated) errored: %v", err)
	}
	wres, _ := waitResp.Result.(map[string]interface{})
	if timedOut, _ := wres["timed_out"].(bool); !timedOut {
		t.Errorf("expected wait_agent_event to time out while child gated, got %+v", wres)
	}

	// Release the model; the child completes and the terminal event is delivered.
	srv.Release()

	harvestResp, err := execTool(t, g, "wait_agent_event", map[string]interface{}{"timeout_ms": float64(5000)})
	if err != nil {
		t.Fatalf("wait_agent_event (harvest) errored: %v", err)
	}
	hres, _ := harvestResp.Result.(map[string]interface{})
	if to, _ := hres["timed_out"].(bool); to {
		t.Fatal("wait_agent_event timed out after releasing the model; child never reported")
	}
	if evType, _ := hres["type"].(string); evType != string(agent.AgentEventCompleted) {
		t.Errorf("expected completed event, got type %q (%+v)", evType, hres)
	}
	if text, _ := hres["text"].(string); !strings.Contains(text, "SUCCESS") {
		t.Errorf("harvested event text missing SUCCESS: %q", text)
	}
	if gotID, _ := hres["agent_id"].(string); gotID != agentID {
		t.Errorf("harvested event agent_id = %q, want %q", gotID, agentID)
	}
}

// TestBothStylesFunctionalInOneSession exercises the blocking spawn_subagent and
// the async launch_agent in the SAME session, proving they coexist (they share
// the SubAgentLimiter and rate limiter).
func TestBothStylesFunctionalInOneSession(t *testing.T) {
	srv := newGatedModelServer("SUCCESS: done")
	srv.Release() // ungated: every model call returns immediately
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()

	g, _ := newBothModeSession(t, server.URL)

	// Blocking path: spawn_subagent runs the child to completion and returns it.
	spawnResp, err := execTool(t, g, spawnTool, map[string]interface{}{"name": "solo", "task": "do the thing"})
	if err != nil {
		t.Fatalf("spawn_subagent errored: %v", err)
	}
	if spawnResp == nil || !spawnResp.Success {
		t.Fatalf("spawn_subagent unsuccessful: %+v", spawnResp)
	}
	sm, _ := spawnResp.Result.(map[string]interface{})
	if res, _ := sm["result"].(string); !strings.Contains(res, "SUCCESS") {
		t.Errorf("spawn_subagent result missing SUCCESS: %q", res)
	}
	// spawn_subagent is always the blocking one-shot primitive (issue #284 wiring).
	if mode, ok := sm["mode"].(map[string]interface{}); ok {
		if one, _ := mode["one_shot"].(bool); !one {
			t.Errorf("spawn_subagent should report one_shot mode, got %+v", mode)
		}
	}

	// Async path in the same session.
	launchResp, err := execTool(t, g, "launch_agent", map[string]interface{}{"name": "bg", "task": "background work"})
	if err != nil {
		t.Fatalf("launch_agent errored: %v", err)
	}
	lm, _ := launchResp.Result.(map[string]interface{})
	if id, _ := lm["agent_id"].(string); strings.TrimSpace(id) == "" {
		t.Fatal("launch_agent returned no agent_id in both-mode session")
	}
	// Harvest the async result so we don't leak a goroutine and to confirm it ran.
	deadline := time.Now().Add(3 * time.Second)
	var completed bool
	for time.Now().Before(deadline) {
		wr, err := execTool(t, g, "wait_agent_event", map[string]interface{}{"timeout_ms": float64(500)})
		if err != nil {
			t.Fatalf("wait_agent_event errored: %v", err)
		}
		m, _ := wr.Result.(map[string]interface{})
		if to, _ := m["timed_out"].(bool); to {
			continue
		}
		if ty, _ := m["type"].(string); ty == string(agent.AgentEventCompleted) {
			completed = true
			break
		}
	}
	if !completed {
		t.Error("async launch_agent child never completed in both-mode session")
	}
}

// TestLaunchAgentRequiresTask covers the error path: an empty task is rejected
// before any sub-agent is created.
func TestLaunchAgentRequiresTask(t *testing.T) {
	srv := newGatedModelServer("SUCCESS: x")
	srv.Release()
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()
	g, _ := newBothModeSession(t, server.URL)

	if _, err := execTool(t, g, "launch_agent", map[string]interface{}{"name": "x", "task": "   "}); err == nil {
		t.Error("launch_agent with blank task should error")
	}
}

// TestInteractiveToolsUnknownAgentError covers the error paths for the status /
// send / terminate tools when given an unknown agent id.
func TestInteractiveToolsUnknownAgentError(t *testing.T) {
	srv := newGatedModelServer("SUCCESS: x")
	srv.Release()
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()
	g, _ := newBothModeSession(t, server.URL)

	cases := []struct {
		tool string
		args map[string]interface{}
	}{
		{"agent_status", map[string]interface{}{"agent_id": "nope"}},
		{"agent_send", map[string]interface{}{"agent_id": "nope", "message": "hi"}},
		{"agent_terminate", map[string]interface{}{"agent_id": "nope"}},
	}
	for _, tc := range cases {
		if _, err := execTool(t, g, tc.tool, tc.args); err == nil {
			t.Errorf("%s on unknown agent_id should error", tc.tool)
		}
	}
}

// TestWaitAgentEventTimesOutWithNoAgents confirms wait_agent_event returns a
// timed_out marker (not a block forever) when no interactive agents exist.
func TestWaitAgentEventTimesOutWithNoAgents(t *testing.T) {
	srv := newGatedModelServer("SUCCESS: x")
	srv.Release()
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()
	g, _ := newBothModeSession(t, server.URL)

	resp, err := execTool(t, g, "wait_agent_event", map[string]interface{}{"timeout_ms": float64(100)})
	if err != nil {
		t.Fatalf("wait_agent_event errored: %v", err)
	}
	m, _ := resp.Result.(map[string]interface{})
	if to, _ := m["timed_out"].(bool); !to {
		t.Errorf("expected timed_out with no agents, got %+v", m)
	}
}

// TestTerminateInteractiveAgentStopsIt launches a gated agent, terminates it,
// and confirms the terminal failed event surfaces (terminate path under #284's
// coexisting engines).
func TestTerminateInteractiveAgentStopsIt(t *testing.T) {
	srv := newGatedModelServer("SUCCESS: never delivered")
	server := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer server.Close()
	defer srv.Release()

	g, _ := newBothModeSession(t, server.URL)

	resp, err := execTool(t, g, "launch_agent", map[string]interface{}{"name": "doomed", "task": "loop"})
	if err != nil {
		t.Fatalf("launch_agent errored: %v", err)
	}
	m, _ := resp.Result.(map[string]interface{})
	agentID, _ := m["agent_id"].(string)

	if _, err := execTool(t, g, "agent_terminate", map[string]interface{}{"agent_id": agentID}); err != nil {
		t.Fatalf("agent_terminate errored: %v", err)
	}

	// A terminated agent yields a terminal (failed) event.
	wr, err := execTool(t, g, "wait_agent_event", map[string]interface{}{"timeout_ms": float64(3000)})
	if err != nil {
		t.Fatalf("wait_agent_event errored: %v", err)
	}
	wm, _ := wr.Result.(map[string]interface{})
	if to, _ := wm["timed_out"].(bool); to {
		t.Fatal("expected a terminal event after terminate, got timeout")
	}
	if ty, _ := wm["type"].(string); ty != string(agent.AgentEventFailed) {
		t.Errorf("terminated agent event type = %q, want failed", ty)
	}
}
