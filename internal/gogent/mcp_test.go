package gogent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogent/internal/config"
	"gogent/internal/permission"
	"gogent/internal/tool"
)

// mcpTestServer is a minimal in-process MCP server over streamable-HTTP (plain
// JSON replies) exposing one "greet" tool, used to drive StartMCPServers end to
// end without launching a subprocess.
func mcpTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64                  `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result interface{}
		switch req.Method {
		case "initialize":
			result = map[string]interface{}{"protocolVersion": "2025-06-18"}
		case "tools/list":
			result = map[string]interface{}{"tools": []map[string]interface{}{{
				"name":        "greet",
				"description": "Greet someone",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{"who": map[string]interface{}{"type": "string"}},
					"required":   []string{"who"},
				},
			}}}
		case "tools/call":
			args, _ := req.Params["arguments"].(map[string]interface{})
			who, _ := args["who"].(string)
			result = map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "hello " + who}}}
		}
		resp, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	}))
}

// TestStartMCPServersRegistersAndDispatches verifies that an allowed, configured
// MCP server has its tools discovered, registered under the mcp__ namespace, and
// dispatched through the registry's ExecuteToolCall.
func TestStartMCPServersRegistersAndDispatches(t *testing.T) {
	ts := mcpTestServer(t)
	defer ts.Close()

	g := NewGogent(t.TempDir())
	// Allow MCP launches (no interactive prompter is installed in tests).
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectAllow),
	})
	g.config.MCPServers = []config.MCPServerConfig{
		{Name: "demo", Transport: "http", URL: ts.URL},
	}

	g.StartMCPServers()
	defer g.CloseMCPServers()

	reg := g.GetToolRegistry()
	const name = "mcp__demo__greet"
	if reg.Get(name) == nil {
		t.Fatalf("MCP tool %q was not registered; tools=%v", name, toolNames(reg))
	}

	resp, err := reg.ExecuteToolCall(&tool.ToolCall{
		Tool: name,
		Args: map[string]interface{}{"who": "world"},
	}, tool.ToolContext{})
	if err != nil {
		t.Fatalf("ExecuteToolCall: %v", err)
	}
	if !resp.Success {
		t.Fatalf("call failed: %s", resp.Error)
	}
	out, _ := resp.Result.(map[string]interface{})
	if got := out["content"]; got != "hello world" {
		t.Fatalf("unexpected content: %v", out)
	}
}

func TestStartMCPServersBareNameFallbackDispatchesJSONTextCall(t *testing.T) {
	ts := mcpTestServer(t)
	defer ts.Close()

	g := NewGogent(t.TempDir())
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectAllow),
	})
	g.config.MCPServers = []config.MCPServerConfig{
		{Name: "demo", Transport: "http", URL: ts.URL},
	}

	g.StartMCPServers()
	defer g.CloseMCPServers()

	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "greet",
		Args: map[string]interface{}{"who": "fallback"},
	}, tool.ToolContext{})
	if err != nil {
		t.Fatalf("ExecuteToolCall: %v", err)
	}
	if !resp.Success {
		t.Fatalf("bare fallback call failed: %s", resp.Error)
	}
	out, _ := resp.Result.(map[string]interface{})
	if got := out["server"]; got != "demo" {
		t.Fatalf("fallback should dispatch to demo MCP server, got result %v", out)
	}
	if got := out["tool"]; got != "greet" {
		t.Fatalf("fallback should call bare MCP tool greet, got result %v", out)
	}
	if got := out["content"]; got != "hello fallback" {
		t.Fatalf("unexpected content: %v", out)
	}
	if got := g.GetToolRegistry().Invocations("mcp__demo__greet"); got != 1 {
		t.Fatalf("resolved MCP tool should receive invocation count, got %d", got)
	}
	if got := g.GetToolRegistry().Invocations("greet"); got != 0 {
		t.Fatalf("bare fallback name must not receive invocation count, got %d", got)
	}
}

func TestStartMCPServersBareNameFallbackAmbiguousAcrossServers(t *testing.T) {
	ts1 := mcpTestServer(t)
	defer ts1.Close()
	ts2 := mcpTestServer(t)
	defer ts2.Close()

	g := NewGogent(t.TempDir())
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectAllow),
	})
	g.config.MCPServers = []config.MCPServerConfig{
		{Name: "alpha", Transport: "http", URL: ts1.URL},
		{Name: "beta", Transport: "http", URL: ts2.URL},
	}

	g.StartMCPServers()
	defer g.CloseMCPServers()

	resp, err := g.GetToolRegistry().ExecuteToolCall(&tool.ToolCall{
		Tool: "greet",
		Args: map[string]interface{}{"who": "fallback"},
	}, tool.ToolContext{})
	if err != nil {
		t.Fatalf("ExecuteToolCall: %v", err)
	}
	if resp.Success {
		t.Fatalf("ambiguous bare MCP fallback must not dispatch silently, got result %v", resp.Result)
	}
	for _, want := range []string{
		"unknown tool: greet",
		"ambiguous MCP bare name",
		"mcp__alpha__greet",
		"mcp__beta__greet",
	} {
		if !strings.Contains(resp.Error, want) {
			t.Fatalf("ambiguous error %q does not contain %q", resp.Error, want)
		}
	}
	if got := g.GetToolRegistry().Invocations("mcp__alpha__greet"); got != 0 {
		t.Fatalf("ambiguous fallback must not execute alpha, got %d invocations", got)
	}
	if got := g.GetToolRegistry().Invocations("mcp__beta__greet"); got != 0 {
		t.Fatalf("ambiguous fallback must not execute beta, got %d invocations", got)
	}
}

func TestStartMCPServersDescriptionsAdvertiseNamespacedToolName(t *testing.T) {
	ts := mcpTestServer(t)
	defer ts.Close()

	g := NewGogent(t.TempDir())
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectAllow),
	})
	g.config.MCPServers = []config.MCPServerConfig{
		{Name: "demo", Transport: "http", URL: ts.URL},
	}

	g.StartMCPServers()
	defer g.CloseMCPServers()

	tl := g.GetToolRegistry().Get("mcp__demo__greet")
	if tl == nil {
		t.Fatalf("MCP tool was not registered; tools=%v", toolNames(g.GetToolRegistry()))
	}
	if !strings.Contains(tl.Description, "Greet someone") {
		t.Fatalf("description should preserve MCP server description, got %q", tl.Description)
	}
	if !strings.Contains(tl.Description, `use its full name "mcp__demo__greet"`) {
		t.Fatalf("description should instruct JSON fallback models to use namespaced name, got %q", tl.Description)
	}
}

// TestStartMCPServersGatedByPermission confirms a server whose launch is denied
// registers no tools and does not abort startup.
func TestStartMCPServersGatedByPermission(t *testing.T) {
	ts := mcpTestServer(t)
	defer ts.Close()

	g := NewGogent(t.TempDir())
	// Explicit deny for MCP launches.
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectDeny),
	})
	g.config.MCPServers = []config.MCPServerConfig{
		{Name: "demo", Transport: "http", URL: ts.URL},
	}

	g.StartMCPServers()
	defer g.CloseMCPServers()

	if reg := g.GetToolRegistry(); reg.Get("mcp__demo__greet") != nil {
		t.Fatal("denied MCP server must not register tools")
	}
}

// TestStartMCPServersSkipsDisabled confirms a disabled entry is ignored.
func TestStartMCPServersSkipsDisabled(t *testing.T) {
	g := NewGogent(t.TempDir())
	g.GetPermissionService().AddRule(permission.Rule{
		Action: string(permission.ActionMCP), Resource: "*", Effect: string(permission.EffectAllow),
	})
	g.config.MCPServers = []config.MCPServerConfig{
		{Name: "demo", Transport: "http", URL: "http://127.0.0.1:0", Disabled: true},
	}
	g.StartMCPServers() // must not block or panic on the unreachable URL
	defer g.CloseMCPServers()

	if got := len(g.mcpClients); got != 0 {
		t.Fatalf("disabled server should not connect, got %d clients", got)
	}
}

func toolNames(reg *tool.ToolRegistry) []string {
	var names []string
	for _, tl := range reg.List() {
		if strings.HasPrefix(tl.Name, "mcp__") {
			names = append(names, tl.Name)
		}
	}
	return names
}
