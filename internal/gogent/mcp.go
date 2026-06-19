package gogent

import (
	"fmt"
	"strings"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/mcp"
	"gogent/internal/permission"
	"gogent/internal/tool"
)

// StartMCPServers connects to every configured Model Context Protocol server,
// lists its tools and registers them in the tool registry so the model can call
// them transparently through ExecuteToolCall (issue #36).
//
// Launching a server is gated through the permission service (ActionMCP): with no
// interactive prompter or allowing rule the launch resolves to "deny", so a
// config synced from elsewhere cannot silently spawn processes. A server that is
// denied, disabled or unreachable is skipped with a warning so one misconfigured
// server never blocks startup.
//
// It should be called once, after the permission prompter is installed. Newly
// registered tools are propagated to any already-created sessions' registries so
// the long-lived "default" session sees them too.
func (g *Gogent) StartMCPServers() {
	g.mu.RLock()
	cfg := g.config
	g.mu.RUnlock()
	if cfg == nil || len(cfg.MCPServers) == 0 {
		return
	}

	registered := false
	for _, sc := range cfg.MCPServers {
		if sc.Disabled || strings.TrimSpace(sc.Name) == "" {
			continue
		}
		// Gate the launch. The server name is the resource so an "always" grant is
		// scoped to a single server.
		if g.permissions != nil {
			if err := g.permissions.CheckWithDetail(permission.ActionMCP, sc.Name, mcpLaunchDetail(sc)); err != nil {
				g.logger().Warn("mcp server not started", "server", sc.Name, "error", err)
				continue
			}
		}

		client, err := mcp.Dial(mcpServerConfig(sc))
		if err != nil {
			g.logger().Warn("mcp server failed to start", "server", sc.Name, "error", err)
			continue
		}
		tools, err := client.ListTools()
		if err != nil {
			g.logger().Warn("mcp server tools/list failed", "server", sc.Name, "error", err)
			_ = client.Close()
			continue
		}
		for _, mt := range tools {
			g.toolRegistry.Register(newMCPTool(sc.Name, client, mt))
		}
		g.mu.Lock()
		g.mcpClients = append(g.mcpClients, client)
		g.mu.Unlock()
		registered = true
		g.logger().Info("mcp server registered tools", "server", sc.Name, "tools", len(tools))
	}

	if registered {
		g.refreshSessionRegistries()
	}
}

// CloseMCPServers releases every connected MCP server (terminating stdio
// subprocesses). It is safe to call when none are connected.
func (g *Gogent) CloseMCPServers() {
	g.mu.Lock()
	clients := g.mcpClients
	g.mcpClients = nil
	g.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}

// refreshSessionRegistries re-clones the global registry into every existing
// session's root agent so tools registered after a session was created (the MCP
// tools) become visible to it. It mirrors the propagation SetSubAgentSettings
// does when the sub-agent mode changes.
func (g *Gogent) refreshSessionRegistries() {
	g.mu.RLock()
	sessions := make([]*agent.UserSession, 0, len(g.userSessions))
	for _, s := range g.userSessions {
		sessions = append(sessions, s)
	}
	cfg := g.config.SubAgents
	g.mu.RUnlock()

	registry := g.toolRegistryForMode(cfg)
	for _, s := range sessions {
		if s.RootAgent != nil {
			s.RootAgent.SetToolRegistry(registry)
		}
	}
}

// mcpToolPrefix namespaces a server's tools so two servers exposing the same tool
// name do not collide and MCP tools are distinguishable from built-ins. It mirrors
// the convention used by other MCP hosts (mcp__<server>__<tool>).
const mcpToolPrefix = "mcp__"

// newMCPTool wraps a remote MCP tool as a registry tool. Its Execute dispatches to
// the server over the shared client; a transport failure is returned as an error
// and a tool-reported failure (CallResult.IsError) is surfaced as one too.
func newMCPTool(server string, client *mcp.Client, mt mcp.Tool) *tool.Tool {
	var schema interface{} = mt.InputSchema
	if mt.InputSchema == nil {
		schema = map[string]interface{}{"type": "object"}
	}
	desc := mt.Description
	if strings.TrimSpace(desc) == "" {
		desc = fmt.Sprintf("MCP tool %q from server %q.", mt.Name, server)
	}
	return &tool.Tool{
		Name:        mcpToolPrefix + server + "__" + mt.Name,
		Description: desc,
		InputSchema: schema,
		Execute: func(args map[string]interface{}, ctx tool.ToolContext) (interface{}, error) {
			res, err := client.CallTool(mt.Name, args)
			if err != nil {
				return nil, fmt.Errorf("mcp %s/%s: %v", server, mt.Name, err)
			}
			if res.IsError {
				return nil, fmt.Errorf("mcp %s/%s: %s", server, mt.Name, res.Text())
			}
			return map[string]interface{}{
				"success": true,
				"server":  server,
				"tool":    mt.Name,
				"content": res.Text(),
			}, nil
		},
	}
}

// mcpServerConfig maps a config.MCPServerConfig to the transport-agnostic
// mcp.ServerConfig consumed by the client, keeping the mcp package free of any
// dependency on the config package.
func mcpServerConfig(sc config.MCPServerConfig) mcp.ServerConfig {
	return mcp.ServerConfig{
		Name:      sc.Name,
		Transport: sc.Transport,
		Command:   sc.Command,
		Args:      sc.Args,
		Env:       sc.Env,
		URL:       sc.URL,
		Headers:   sc.Headers,
	}
}

// mcpLaunchDetail renders a human-readable summary of what launching a server
// entails, shown in the permission prompt.
func mcpLaunchDetail(sc config.MCPServerConfig) string {
	switch sc.Transport {
	case "http", "streamable-http":
		return "connect to MCP server " + sc.URL
	default:
		return "launch MCP server: " + strings.TrimSpace(sc.Command+" "+strings.Join(sc.Args, " "))
	}
}
