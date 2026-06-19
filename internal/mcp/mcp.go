// Package mcp implements a minimal Model Context Protocol (MCP) client. It speaks
// JSON-RPC 2.0 over two transports — a launched stdio subprocess and
// streamable-HTTP — and exposes the handful of operations gogent needs to surface
// a remote server's tools through its own tool registry: the initialize
// handshake, tools/list and tools/call.
//
// It depends only on the standard library. The client is deliberately small: it
// implements the synchronous request/response subset of MCP a tool host needs and
// ignores the optional server-initiated features (sampling, roots, elicitation)
// it does not advertise.
package mcp

import (
	"encoding/json"
	"fmt"
)

// protocolVersion is the MCP revision this client advertises in initialize.
const protocolVersion = "2025-06-18"

// ServerConfig describes how to reach a single MCP server. Transport selects the
// wire ("stdio", the default, or "http"/"streamable-http"); the remaining fields
// are transport-specific.
type ServerConfig struct {
	Name string
	// Transport is "stdio" (default) or "http"/"streamable-http".
	Transport string
	// Command/Args/Env configure a stdio server (a launched subprocess).
	Command string
	Args    []string
	Env     map[string]string
	// URL/Headers configure an http server.
	URL     string
	Headers map[string]string
}

// Tool describes a remote tool advertised by a server (the tools/list shape).
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// Content is a single content block of a tools/call result. Only text blocks are
// surfaced; other block types (image, resource) are reported by Type with empty
// Text.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallResult is the outcome of a tools/call.
type CallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Text joins the text of every content block, one block per line. It is the
// flattened, model-facing form of a tool result.
func (r *CallResult) Text() string {
	out := ""
	for i, c := range r.Content {
		if i > 0 {
			out += "\n"
		}
		out += c.Text
	}
	return out
}

// transport exchanges JSON-RPC messages with a server. A call sends a request and
// returns its raw result; notify sends a one-way notification.
type transport interface {
	call(method string, params interface{}) (json.RawMessage, error)
	notify(method string, params interface{}) error
	close() error
}

// rpcRequest is a JSON-RPC 2.0 request or (when ID is 0/omitted) notification.
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message)
}

// Client is a connected MCP server. It is safe for concurrent use; the underlying
// transport serializes overlapping calls.
type Client struct {
	t    transport
	name string
}

// Dial connects to the server described by cfg, performs the initialize
// handshake, and returns a ready Client. The caller owns the returned Client and
// must Close it to release the transport (e.g. terminate a stdio subprocess).
func Dial(cfg ServerConfig) (*Client, error) {
	t, err := newTransport(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{t: t, name: cfg.Name}
	if err := c.initialize(); err != nil {
		_ = t.close()
		return nil, err
	}
	return c, nil
}

// newTransport builds the transport selected by cfg.Transport.
func newTransport(cfg ServerConfig) (transport, error) {
	switch cfg.Transport {
	case "", "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("mcp server %q: stdio transport requires a command", cfg.Name)
		}
		return newStdioTransport(cfg.Command, cfg.Args, cfg.Env)
	case "http", "streamable-http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcp server %q: http transport requires a url", cfg.Name)
		}
		return newHTTPTransport(cfg.URL, cfg.Headers), nil
	default:
		return nil, fmt.Errorf("mcp server %q: unknown transport %q", cfg.Name, cfg.Transport)
	}
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// initialize performs the MCP handshake: an initialize request followed by the
// notifications/initialized acknowledgement the spec requires before any other
// request.
func (c *Client) initialize() error {
	params := map[string]interface{}{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "gogent",
			"version": "1.0",
		},
	}
	if _, err := c.t.call("initialize", params); err != nil {
		return err
	}
	return c.t.notify("notifications/initialized", nil)
}

// ListTools returns every tool the server advertises, following the tools/list
// cursor pagination to completion.
func (c *Client) ListTools() ([]Tool, error) {
	var tools []Tool
	cursor := ""
	for {
		params := map[string]interface{}{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.t.call("tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return tools, nil
}

// CallTool invokes a remote tool by name with the given arguments and returns its
// result. A protocol-level failure is returned as an error; a tool-reported
// failure is carried in CallResult.IsError.
func (c *Client) CallTool(name string, args map[string]interface{}) (*CallResult, error) {
	if args == nil {
		args = map[string]interface{}{}
	}
	raw, err := c.t.call("tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call: %w", err)
	}
	return &res, nil
}

// Close releases the client's transport.
func (c *Client) Close() error { return c.t.close() }
