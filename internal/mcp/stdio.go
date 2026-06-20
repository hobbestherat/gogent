package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// streamTransport carries newline-delimited JSON-RPC messages over a paired
// reader/writer. It backs the stdio transport (over a subprocess's pipes) and is
// kept separate from process management so the framing can be unit-tested over an
// in-memory pipe. A mutex serializes calls: this client issues one request at a
// time and reads its response before sending the next.
type streamTransport struct {
	mu     sync.Mutex
	w      io.Writer
	r      *bufio.Reader
	id     int64
	closer func() error
}

func newStreamTransport(r io.Reader, w io.Writer, closer func() error) *streamTransport {
	return &streamTransport{w: w, r: bufio.NewReader(r), closer: closer}
}

// newStdioTransport launches command (with args/env) and speaks MCP over its
// stdin/stdout. The child's stderr is forwarded to gogent's so server diagnostics
// remain visible.
func newStdioTransport(command string, args []string, env map[string]string) (*streamTransport, error) {
	cmd := exec.Command(command, args...) //nolint:gosec // launches user-configured MCP server command
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp stdio: start %q: %w", command, err)
	}

	closer := func() error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return nil
	}
	return newStreamTransport(stdout, stdin, closer), nil
}

// writeMessage marshals v as a single newline-terminated JSON line. The caller
// holds mu.
func (t *streamTransport) writeMessage(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp stdio: marshal message: %w", err)
	}
	b = append(b, '\n')
	if _, err := t.w.Write(b); err != nil {
		return fmt.Errorf("mcp stdio: write: %w", err)
	}
	return nil
}

func (t *streamTransport) call(method string, params interface{}) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.id++
	id := t.id
	if err := t.writeMessage(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	// Read until the response with the matching id arrives, skipping any
	// server-sent notifications or non-JSON diagnostic lines in between.
	for {
		line, err := t.r.ReadBytes('\n')
		if err != nil && len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("mcp stdio: read response for %q: %w", method, err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if err != nil {
				return nil, fmt.Errorf("mcp stdio: read response for %q: %w", method, err)
			}
			continue
		}
		var resp rpcResponse
		if jerr := json.Unmarshal(line, &resp); jerr != nil {
			continue // a log line or unrelated message; ignore
		}
		if resp.ID != id {
			continue // a notification (id 0) or an unrelated response
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (t *streamTransport) notify(method string, params interface{}) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// A notification omits the id (rpcRequest.ID stays 0, dropped by omitempty).
	return t.writeMessage(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *streamTransport) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closer != nil {
		return t.closer()
	}
	return nil
}
