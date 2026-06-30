package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gogent/internal/config"
	"gogent/internal/stats"
)

// APIClient is a thin HTTP/SSE client for the existing internal/server /api
// surface (issue #358, Phase 2). It is the transport the attached ("thin
// client") TUI uses to drive a running daemon: every Handlers call the remote
// TUI makes maps to one request here, and the live event stream is consumed
// from the global SSE endpoint. It is stdlib-only (net/http) and safe for
// concurrent use — the underlying http.Client is, and its only mutable state is
// the notification handler, guarded by a mutex.
//
// Transports are selected by the connect address scheme:
//   - unix:///path/to/daemon.sock — the local daemon socket (default). The
//     socket's 0600 filesystem permissions are the access gate; the server
//     treats a Unix-socket caller as a local human, so no token is needed.
//   - http://host:port | https://host:port — TCP, for a remote daemon reached
//     over (manually forwarded) SSH or a trusted network. A bearer token
//     (GOGENT_HTTP_TOKEN on the daemon) authenticates non-loopback callers.
//   - ssh://[user@]host[:sshport] — a native in-process SSH tunnel (issue #482).
//     The caller (cmd/attach.go) builds the tunnel and injects its DialContext
//     via WithDialContext; this client then dials the remote daemon over an SSH
//     channel per request, mirroring the unix:// transport with no local
//     listener. The token is carried but usually inert (the channel lands on the
//     daemon's Unix/loopback listener → local human).
type APIClient struct {
	http  *http.Client
	base  string // request base, e.g. "http://unix" or "http://host:port"
	token string // optional bearer token (TCP auth); empty for the local socket

	// notifyMu guards onNotification, onApprovalSignal and onApprovalExpired — the
	// callbacks for "notification" (issue #358 §9), "approval" and "approval_expired"
	// (issue #569) SSE frames on the global stream. They are the client's only
	// mutable state.
	notifyMu          sync.Mutex
	onNotification    func(NotificationDTO)
	onApprovalSignal  func()
	onApprovalExpired func(ApprovalExpiredDTO)
}

// SetNotificationHandler installs the callback invoked for each "notification"
// SSE frame on the global stream (issue #358 §9). The attached TUI points it at
// its desktop notifier so a daemon-side watcher completion surfaces on the TUI's
// machine. A nil handler drops notification frames. Safe to call from any
// goroutine and at any time.
func (c *APIClient) SetNotificationHandler(h func(NotificationDTO)) {
	c.notifyMu.Lock()
	c.onNotification = h
	c.notifyMu.Unlock()
}

// notificationHandler returns the currently-installed notification callback (nil
// if none), read under the lock so it never races SetNotificationHandler.
func (c *APIClient) notificationHandler() func(NotificationDTO) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	return c.onNotification
}

// SetApprovalSignalHandler installs the callback invoked for each "approval" SSE
// frame on the global stream (issue #569). The attached TUI points it at the
// RemoteClient's approval re-scan so a freshly-raised remote prompt surfaces its
// ⏳ badge + dialog immediately rather than on the next poll tick. The handler
// takes no argument: the frame is only a nudge, and GET /approvals is the
// authoritative source. A nil handler drops the frame. Safe to call from any
// goroutine and at any time.
func (c *APIClient) SetApprovalSignalHandler(h func()) {
	c.notifyMu.Lock()
	c.onApprovalSignal = h
	c.notifyMu.Unlock()
}

// approvalSignalHandler returns the currently-installed approval-signal callback
// (nil if none), read under the lock so it never races SetApprovalSignalHandler.
func (c *APIClient) approvalSignalHandler() func() {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	return c.onApprovalSignal
}

// SetApprovalExpiredHandler installs the callback invoked for each
// "approval_expired" SSE frame on the global stream (issue #569). The attached TUI
// points it at the RemoteClient so a presented prompt that timed out before the
// user answered surfaces an in-window notice (the user is told their late answer no
// longer applies) instead of being silently denied. A nil handler drops the frame.
// Safe to call from any goroutine and at any time.
func (c *APIClient) SetApprovalExpiredHandler(h func(ApprovalExpiredDTO)) {
	c.notifyMu.Lock()
	c.onApprovalExpired = h
	c.notifyMu.Unlock()
}

// approvalExpiredHandler returns the currently-installed approval-expired callback
// (nil if none), read under the lock so it never races SetApprovalExpiredHandler.
func (c *APIClient) approvalExpiredHandler() func(ApprovalExpiredDTO) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	return c.onApprovalExpired
}

// quickTimeout bounds the short request/response calls (create, stop, settings,
// …). It deliberately does NOT apply to SendMessage or the SSE stream, which
// run for the lifetime of a turn / the whole attachment and must not be capped.
const quickTimeout = 30 * time.Second

// APIClientOption customises NewAPIClient. It is the seam through which the
// attach layer injects an out-of-process transport (the ssh:// tunnel) without
// NewAPIClient itself depending on the SSH machinery.
type APIClientOption func(*apiClientOptions)

type apiClientOptions struct {
	base string
	dial func(context.Context, string, string) (net.Conn, error)
}

// WithDialContext supplies a custom transport (base URL placeholder + per-request
// DialContext) for the ssh:// scheme. cmd/attach.go builds the SSH tunnel, owns
// its lifecycle, and passes tunnel.DialContext here so this client opens an SSH
// channel to the remote daemon per request — exactly as the unix:// case dials
// the socket.
func WithDialContext(base string, dial func(context.Context, string, string) (net.Conn, error)) APIClientOption {
	return func(o *apiClientOptions) {
		o.base = base
		o.dial = dial
	}
}

// NewAPIClient builds a client for the daemon at addr. addr is a scheme-
// qualified connect address:
//
//	unix:///home/u/.gogent/daemon.sock
//	http://localhost:8080
//	https://host:8080
//	ssh://user@host[:sshport]   (requires an injected WithDialContext tunnel)
//
// token is an optional bearer token used only for the TCP transports (it is
// harmless but unnecessary over the Unix socket and usually over SSH). A bare
// path or an unknown scheme is rejected so a malformed --connect value fails fast
// and visibly.
func NewAPIClient(addr, token string, opts ...APIClientOption) (*APIClient, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parse connect address %q: %w", addr, err)
	}
	switch u.Scheme {
	case "ssh":
		// The tunnel is built and injected by the attach layer (which owns its
		// lifecycle); this client is just the HTTP/SSE driver over its DialContext.
		var o apiClientOptions
		for _, opt := range opts {
			opt(&o)
		}
		if o.dial == nil {
			return nil, fmt.Errorf("ssh connect %q requires an injected tunnel (internal error)", addr)
		}
		base := o.base
		if base == "" {
			base = "http://ssh"
		}
		return &APIClient{
			base:  base,
			token: token,
			http:  &http.Client{Transport: &http.Transport{DialContext: o.dial}},
		}, nil
	case "unix":
		// The path is the socket; the HTTP host is a conventional placeholder the
		// custom DialContext ignores.
		sock := u.Path
		if sock == "" {
			return nil, fmt.Errorf("unix connect address %q has no socket path", addr)
		}
		return &APIClient{
			base: "http://unix",
			http: &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, "unix", sock)
					},
				},
			},
		}, nil
	case "http", "https":
		return &APIClient{
			base:  strings.TrimRight(u.Scheme+"://"+u.Host, "/"),
			token: token,
			http:  &http.Client{Transport: &http.Transport{}},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported connect scheme %q (want unix:// | http:// | https:// | ssh://)", u.Scheme)
	}
}

// CloseIdleConnections drops pooled keep-alive connections. The attach layer
// calls it after the ssh:// tunnel actually redials (RemoteClient.reconnect), so
// the http.Transport pool cannot hand a stale channel bound to the dead SSH
// session to the next request. It is a no-op-equivalent for the other transports.
func (c *APIClient) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// --- low-level request helpers ----------------------------------------------

// newRequest builds an /api request with the bearer token applied. path is the
// API path WITHOUT the /api prefix (e.g. "/sessions"); body is marshalled as
// JSON when non-nil.
func (c *APIClient) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api"+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// do executes a short request and, when out is non-nil, decodes the JSON
// response body into it. A non-2xx status becomes an error carrying the body so
// the caller can surface a meaningful failure. It applies quickTimeout.
func (c *APIClient) do(method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), quickTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// --- wire DTOs (mirror internal/server's unexported view types) -------------

// SessionDTO mirrors the server's sessionView for GET /sessions[/:id].
type SessionDTO struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
	State        string `json:"state"`
	PrimaryModel string `json:"primary_model"`
	Persisted    bool   `json:"persisted"`
	Live         bool   `json:"live"`
}

// MessageDTO mirrors the server's messageView (transcript responses) and the
// non-blocking send/approve acceptedView. For a transcript entry Role/Content are
// set; for a dispatched turn (issue #481) the response carries only TurnID and the
// final answer arrives over SSE, so callers use TurnID/err rather than Content.
type MessageDTO struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// TurnID is the id of the async-dispatched turn (issue #481), set on the
	// send/approve response; empty on transcript entries. The POST/approve response
	// carries it as "turnId" (the SSE EventDTO uses "turn_id" — distinct messages).
	TurnID string `json:"turnId"`
}

// EventDTO mirrors the server's eventView — the JSON payload of one SSE event.
type EventDTO struct {
	Type      string         `json:"type"`
	Step      int            `json:"step"`
	Text      string         `json:"text"`
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	Result    string         `json:"result"`
	CallID    string         `json:"call_id"`
	Error     string         `json:"error"`
	Stats     *StatsView     `json:"stats"`
	AgentID   string         `json:"agent_id"`
	Name      string         `json:"name"`
	Status    string         `json:"status"`
	Kind      string         `json:"kind"`
	Todos     []TodoView     `json:"todos"`
	Plan      string         `json:"plan"`
	SessionID string         `json:"session_id"`
	TurnID    string         `json:"turn_id"`
}

// StatsView mirrors the server's sessionStatsView, carried on usage events.
type StatsView struct {
	Turns         int `json:"turns"`
	TokensIn      int `json:"tokens_in"`
	TokensOut     int `json:"tokens_out"`
	ToolCalls     int `json:"tool_calls"`
	ContextTokens int `json:"context_tokens"`
	ContextWindow int `json:"context_window"`
}

// TodoView mirrors one server todoItemView.
type TodoView struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// GlobalEventDTO mirrors the server's globalEventView: an event tagged with the
// session it belongs to (the shape of every frame on GET /api/events).
type GlobalEventDTO struct {
	SessionID string   `json:"session_id"`
	Event     EventDTO `json:"event"`
}

// NotificationDTO mirrors the server's NotificationEvent: a backend notification
// (a watcher/agent completion) carried on the global SSE stream as an event named
// "notification" (issue #358 §9). The attached TUI raises an OS desktop
// notification on its own machine from it.
type NotificationDTO struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Reason    string `json:"reason"`
	SessionID string `json:"session_id"`
	Timestamp string `json:"timestamp"`
}

// notificationEventName is the SSE event: name the server uses for a backend
// notification on the global stream (issue #358 §9); every other frame there is a
// GlobalEventDTO.
const notificationEventName = "notification"

// approvalEventName is the SSE event: name the server uses for an "approval
// pending" nudge on the global stream (issue #569). Its body is an approval id the
// client ignores — the frame just triggers an immediate /approvals re-scan.
const approvalEventName = "approval"

// approvalExpiredEventName is the SSE event: name the server uses for a "presented
// approval timed out" signal on the global stream (issue #569). Its body is an
// ApprovalExpiredDTO; the client surfaces a timeout notice in the named session.
const approvalExpiredEventName = "approval_expired"

// ApprovalExpiredDTO mirrors the server's approvalExpiredView: a presented approval
// that reached its auto-deny timeout before being answered (issue #569). The
// attached TUI uses SessionID to surface the timeout notice in the right window.
type ApprovalExpiredDTO struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
}

// LogRecordDTO mirrors the server's streamed diagnostic-log record (issue #562):
// the daemon's gogent.log lines surfaced over GET /api/logs/stream so the Logs
// window can interlace them with the client's own logs. Text is already redacted
// server-side. Time is RFC3339Nano; Level is "INFO"|"WARN"|"ERROR".
type LogRecordDTO struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// logEventName is the SSE event name the server uses for a streamed log record
// (issue #562); the client filters the logs stream on it.
const logEventName = "log"

// ApprovalDTO mirrors the server's approvalView (a pending interactive gate).
type ApprovalDTO struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"` // "permission" | "edit_review"
	SessionID  string            `json:"session_id"`
	AgentID    string            `json:"agent_id"`
	Permission *PermissionDetail `json:"permission"`
	EditReview *EditReviewDetail `json:"edit_review"`
	CreatedAt  string            `json:"created_at"`
}

// PermissionDetail mirrors the server's permissionDetail.
type PermissionDetail struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Detail   string `json:"detail"`
}

// EditReviewDetail mirrors the server's editReviewDetail.
type EditReviewDetail struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	Diff string `json:"diff"`
}

// ModelDTO mirrors the server's (redacted) modelView. The api_key is never sent
// back by the server; HasAPIKey reports whether one is configured.
type ModelDTO struct {
	Name                string   `json:"name"`
	DisplayName         string   `json:"display_name"`
	APIType             string   `json:"api_type"`
	Endpoint            string   `json:"endpoint"`
	Project             string   `json:"project"`
	Location            string   `json:"location"`
	Model               string   `json:"model"`
	HasAPIKey           bool     `json:"has_api_key"`
	Temperature         float32  `json:"temperature"`
	TopP                float32  `json:"top_p"`
	MaxTokens           int      `json:"max_tokens"`
	ContextWindow       int      `json:"context_window"`
	ModelTimeoutSeconds int      `json:"model_timeout_seconds"`
	ReasoningEffort     string   `json:"reasoning_effort"`
	EffortOptions       []string `json:"effort_options"`
	Thinking            *bool    `json:"thinking"`
	Free                bool     `json:"free"`
}

// ToModelConfig projects a redacted ModelDTO back into a config.ModelConfig for
// the TUI's model dropdown and editor. The api_key is intentionally left empty
// (the server redacts it); an empty key in a later PUT /models preserves the
// daemon's stored key, so a view→edit→save round-trip never wipes it.
func (m ModelDTO) ToModelConfig() config.ModelConfig {
	return config.ModelConfig{
		Name:                m.Name,
		DisplayName:         m.DisplayName,
		APIType:             m.APIType,
		Endpoint:            m.Endpoint,
		Project:             m.Project,
		Location:            m.Location,
		Model:               m.Model,
		Temperature:         m.Temperature,
		TopP:                m.TopP,
		MaxTokens:           m.MaxTokens,
		ContextWindow:       m.ContextWindow,
		ModelTimeoutSeconds: m.ModelTimeoutSeconds,
		ReasoningEffort:     m.ReasoningEffort,
		EffortOptions:       m.EffortOptions,
		Thinking:            m.Thinking,
		Free:                m.Free,
	}
}

// ToolDTO mirrors the server's toolView.
type ToolDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema string `json:"input_schema"`
	Enabled     bool   `json:"enabled"`
	Invocations int    `json:"invocations"`
	ReadOnly    bool   `json:"read_only"`
}

// SkillDTO mirrors the server's skillView.
type SkillDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
	Success     int    `json:"success"`
	Failure     int    `json:"failure"`
	TotalCalls  int    `json:"total_calls"`
}

// SettingsDTO mirrors the server's settingsView (GET/PUT /api/settings). It
// reuses the config types verbatim, exactly as the server does, so a read-
// modify-write round-trip is lossless.
type SettingsDTO struct {
	SubAgents config.SubAgentConfig `json:"sub_agents"`
	Timeouts  config.TimeoutConfig  `json:"timeouts"`
	Budget    config.BudgetConfig   `json:"budget"`
	// DefaultModel is the daemon's default-model name for new sessions (issue #507),
	// daemon-owned exactly as Budget is. A read-modify-write round-trip preserves it.
	DefaultModel string `json:"default_model"`
	ReviewEdits  bool   `json:"review_edits"`
}

// --- health & sessions ------------------------------------------------------

// Health reports whether the daemon answers GET /api/health. It is used to
// confirm a live attachment before building the TUI.
func (c *APIClient) Health() error {
	return c.do(http.MethodGet, "/health", nil, nil)
}

// WorkspaceDTO mirrors the server's workspaceView (GET /api/workspace): the
// DAEMON's own working directory — where ! shell commands and the agent's shell
// tool calls actually run — plus optional git info. The attached TUI consumes
// Root for the status-line working-directory affordance (issue #570), matching
// what the embedded TUI shows locally; Git is carried verbatim for forward use
// (a later status-line git decoration) but is not consumed today.
type WorkspaceDTO struct {
	Root string      `json:"root"`
	Git  *GitInfoDTO `json:"git,omitempty"`
}

// GitInfoDTO mirrors the server's gitInfo (the optional git block on
// workspaceView): the daemon workspace's current branch and dirty state.
type GitInfoDTO struct {
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty"`
}

// Workspace returns the daemon's workspace root (and optional git info) from GET
// /api/workspace. It backs the attached status-line path (issue #570): the root
// is the daemon-side directory where shell/tool calls run, so an SSH-attached
// user sees the same path the local TUI shows rather than their client cwd. The
// daemon root is immutable for the daemon's lifetime, so the caller caches it.
func (c *APIClient) Workspace() (WorkspaceDTO, error) {
	var out WorkspaceDTO
	if err := c.do(http.MethodGet, "/workspace", nil, &out); err != nil {
		return WorkspaceDTO{}, err
	}
	return out, nil
}

// shellRequestDTO is the body of POST /api/shell (issue #571).
type shellRequestDTO struct {
	Command string `json:"command"`
}

// ShellResultDTO mirrors the server's shellView (POST /api/shell): the result of
// a !-prefixed shell command the daemon ran out-of-band at its workspace root.
// Error is set only when the command could not be launched; a non-zero exit is
// carried in ExitCode (and Timeout marks a command killed at the shell timeout).
type ShellResultDTO struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code,omitempty"`
	Timeout  bool   `json:"timeout,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Shell runs a !-prefixed shell command on the daemon at its workspace root via
// POST /api/shell (issue #571) and returns the result. It backs the attached
// TUI's OnShell handler: an SSH-attached user's !cmd executes daemon-side, never
// through an agent turn.
//
// It deliberately does NOT use c.do(): that helper caps every request at
// quickTimeout (30s), which would kill a longer-running command and diverge from
// the embedded path, where shell.Execute honours the full shell timeout (5 min by
// default). Instead — exactly like SendMessage — it issues the request with the
// caller's context and a direct http.Do, so the only bound is the daemon-side
// shell timeout. Callers pass context.Background() for that 5-minute ceiling.
func (c *APIClient) Shell(ctx context.Context, command string) (ShellResultDTO, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/shell", shellRequestDTO{Command: command})
	if err != nil {
		return ShellResultDTO{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ShellResultDTO{}, fmt.Errorf("shell: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ShellResultDTO{}, fmt.Errorf("shell: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out ShellResultDTO
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		return ShellResultDTO{}, fmt.Errorf("decode shell response: %w", err)
	}
	return out, nil
}

// ListSessions returns every saved + live session known to the daemon (index
// metadata; no transcript replay), backing both Restore and the Saved Sessions
// browser.
func (c *APIClient) ListSessions() ([]SessionDTO, error) {
	var out []SessionDTO
	if err := c.do(http.MethodGet, "/sessions", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSessionsBounded lists sessions with the issue #517 bounding params applied
// (index metadata only; no transcript replay). live=true restricts the result to
// live sessions — excluding archived/closed ones — and orders it most-recent-first;
// limit<=0 means no cap and offset<0 is treated as 0. It backs Restore, which must
// not pay one round-trip per session against an unbounded on-disk list. With
// live=false, limit<=0 and offset<=0 the server falls back to the full ListSessions
// listing, so callers wanting the legacy behaviour use ListSessions instead.
func (c *APIClient) ListSessionsBounded(live bool, limit, offset int) ([]SessionDTO, error) {
	q := url.Values{}
	if live {
		q.Set("live", "true")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/sessions"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out []SessionDTO
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetSession returns one session's metadata (live or saved). It backs the saved-
// session browser's open path, which needs the title + model the bare id lacks.
func (c *APIClient) GetSession(id string) (SessionDTO, error) {
	var out SessionDTO
	if err := c.do(http.MethodGet, "/sessions/"+url.PathEscape(id), nil, &out); err != nil {
		return SessionDTO{}, err
	}
	return out, nil
}

// CreateSession creates a daemon session under the caller-chosen id (so the
// TUI window id and the daemon session id stay in lock-step). persisted selects
// a durable (vs ephemeral) session.
func (c *APIClient) CreateSession(id, title string, persisted bool) (SessionDTO, error) {
	body := map[string]any{"id": id, "title": title, "persisted": persisted}
	var out SessionDTO
	if err := c.do(http.MethodPost, "/sessions", body, &out); err != nil {
		return SessionDTO{}, err
	}
	return out, nil
}

// DeleteSession closes (and archives, if persisted) a daemon session.
func (c *APIClient) DeleteSession(id string) error {
	return c.do(http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil)
}

// GetTranscript returns an agent's message transcript for a session
// (agent defaults to "root" on the server when empty).
func (c *APIClient) GetTranscript(id, agentID string) ([]MessageDTO, error) {
	path := "/sessions/" + url.PathEscape(id) + "/transcript"
	if agentID != "" {
		path += "?agent=" + url.QueryEscape(agentID)
	}
	var out []MessageDTO
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessage dispatches a turn on the daemon and returns as soon as the turn is
// accepted (issue #481) — it does NOT block until the turn completes. It is the
// remote equivalent of the embedded OnSend background goroutine: progress
// (thoughts, tool calls, the final answer) is delivered out-of-band over the
// global SSE stream, so the returned MessageDTO carries only the dispatched
// TurnID (used for correlation) and the call is used mainly to detect an outright
// dispatch failure. The turn then runs on the daemon independently of this
// connection. ctx (a background context) is NOT bounded by quickTimeout.
func (c *APIClient) SendMessage(ctx context.Context, id, message, modelName, effort string) (MessageDTO, error) {
	return c.SendMessageWithOverrides(ctx, id, message, modelName, "", false, effort)
}

// SendMessageWithOverrides sends a turn carrying a custom command's per-invocation
// agent/subtask overrides (issue #403). A non-empty agent or subtask=true makes the
// daemon route the prompt through a one-shot sub-agent (the embedded path's
// equivalent of OnSendCommand), so an attached TUI applies the overrides exactly as
// embedded. SendMessage delegates here with no overrides.
func (c *APIClient) SendMessageWithOverrides(ctx context.Context, id, message, modelName, agentName string, subtask bool, effort string) (MessageDTO, error) {
	body := sendMessageBody{Message: message, Model: modelName, Effort: effort, Agent: agentName, Subtask: subtask}
	req, err := c.newRequest(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/messages", body)
	if err != nil {
		return MessageDTO{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("send message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return MessageDTO{}, fmt.Errorf("send message: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out MessageDTO
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		return MessageDTO{}, fmt.Errorf("decode send response: %w", err)
	}
	return out, nil
}

// sendMessageBody mirrors the server's sendMessageRequest.
type sendMessageBody struct {
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Subtask bool   `json:"subtask,omitempty"`
}

// StopSession cancels a session's in-flight turn.
func (c *APIClient) StopSession(id string) error {
	return c.do(http.MethodPost, "/sessions/"+url.PathEscape(id)+"/stop", nil, nil)
}

// InjectSession splices a note into a session's running turn at the next turn
// boundary (the Interject path).
func (c *APIClient) InjectSession(id, message string) error {
	return c.do(http.MethodPost, "/sessions/"+url.PathEscape(id)+"/inject",
		map[string]string{"message": message}, nil)
}

// Undo reverts a session's last turn, returning the daemon's summary.
func (c *APIClient) Undo(id string) (string, error) {
	var out map[string]string
	if err := c.do(http.MethodPost, "/sessions/"+url.PathEscape(id)+"/undo", nil, &out); err != nil {
		return "", err
	}
	return out["result"], nil
}

// Rewind reverts a session's last n turns, returning the daemon's summary.
func (c *APIClient) Rewind(id string, turns int) (string, error) {
	var out map[string]string
	if err := c.do(http.MethodPost, "/sessions/"+url.PathEscape(id)+"/rewind",
		map[string]int{"turns": turns}, &out); err != nil {
		return "", err
	}
	return out["result"], nil
}

// SetPlanMode toggles plan mode for a session.
func (c *APIClient) SetPlanMode(id string, on bool) error {
	return c.do(http.MethodPut, "/sessions/"+url.PathEscape(id)+"/plan-mode",
		map[string]bool{"enabled": on}, nil)
}

// ApprovePlan dispatches a session's pending plan as a turn on the daemon and
// returns as soon as it is accepted (issue #481) — it does NOT block until the
// plan turn completes. Like SendMessage the turn's progress streams over SSE, so
// ctx is a background context and the call is used to detect a dispatch failure
// (e.g. no plan awaiting approval, 400).
func (c *APIClient) ApprovePlan(ctx context.Context, id string) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/plan/approve", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("approve plan: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("approve plan: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// --- settings, models, tools, skills, stats ---------------------------------

// GetSettings reads the daemon's sub-agent/timeouts/budget/review-edits block.
func (c *APIClient) GetSettings() (SettingsDTO, error) {
	var out SettingsDTO
	if err := c.do(http.MethodGet, "/settings", nil, &out); err != nil {
		return SettingsDTO{}, err
	}
	return out, nil
}

// SetSettings writes the full settings block back (PUT /api/settings merges and
// persists). Callers that change one field do a read-modify-write.
func (c *APIClient) SetSettings(s SettingsDTO) error {
	return c.do(http.MethodPut, "/settings", s, nil)
}

// ListModels returns the daemon's configured models (api_key redacted).
func (c *APIClient) ListModels() ([]ModelDTO, error) {
	var out []ModelDTO
	if err := c.do(http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateModel persists changes to one model on the daemon (matched by name). An
// empty api_key preserves the daemon's stored key.
func (c *APIClient) UpdateModel(m config.ModelConfig) error {
	return c.do(http.MethodPut, "/models/"+url.PathEscape(m.Name), m, nil)
}

// AddModel creates a NEW model on the daemon (POST /models). The daemon returns
// 409 (surfaced as an error) when the name already exists.
func (c *APIClient) AddModel(m config.ModelConfig) error {
	return c.do(http.MethodPost, "/models", m, nil)
}

// RemoveModel deletes a model on the daemon (DELETE /models/:name). The daemon
// returns 404 for an unknown name and 409 when removal is blocked (the model is
// the default while others remain, or it is in use by an active session); c.do
// surfaces the response body as a Go error the Models… dialog shows verbatim.
func (c *APIClient) RemoveModel(name string) error {
	return c.do(http.MethodDelete, "/models/"+url.PathEscape(name), nil, nil)
}

// ScanModels probes a model backend on the daemon for the model ids it serves.
func (c *APIClient) ScanModels(name string) ([]string, error) {
	var out struct {
		Models []string `json:"models"`
	}
	if err := c.do(http.MethodPost, "/models/"+url.PathEscape(name)+"/scan", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// ListTools returns the daemon's registered tools with enabled state + usage.
func (c *APIClient) ListTools() ([]ToolDTO, error) {
	var out []ToolDTO
	if err := c.do(http.MethodGet, "/tools", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetToolEnabled toggles whether a tool is advertised + executable on the daemon.
func (c *APIClient) SetToolEnabled(name string, enabled bool) error {
	return c.do(http.MethodPut, "/tools/"+url.PathEscape(name)+"/enabled",
		map[string]bool{"enabled": enabled}, nil)
}

// ListSkills returns the daemon's loaded skills with active state + usage.
func (c *APIClient) ListSkills() ([]SkillDTO, error) {
	var out []SkillDTO
	if err := c.do(http.MethodGet, "/skills", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetSkillActive toggles whether a skill is active on the daemon.
func (c *APIClient) SetSkillActive(name string, active bool) error {
	return c.do(http.MethodPut, "/skills/"+url.PathEscape(name)+"/active",
		map[string]bool{"enabled": active}, nil)
}

// --- Watchers (issue #329 Phase 5 daemon API; remote wiring issue #572) ------

// WatcherDTO mirrors the server's watcherView for the /watchers endpoints
// (internal/server/wire.go). Target is the owning session id for an attached
// watcher or "free" for a free-running one; the daemon omits zero timestamps and
// sends NextFire/LastRun as RFC3339 strings.
type WatcherDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`   // "free" | "attached"
	Target     string `json:"target"` // session id, or "free" for free-running
	Task       string `json:"task,omitempty"`
	Schedule   string `json:"schedule,omitempty"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"`
	NextFire   string `json:"next_fire,omitempty"`
	LastRun    string `json:"last_run,omitempty"`
	LastResult string `json:"last_result,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// WatcherCreateDTO is the body of POST /watchers, mirroring the server's
// createWatcherRequest. Schedule reuses config.ScheduleConfig (exactly one of
// every / daily_at). ReportToSession decides the kind: nil/omitted ⇒ free-running,
// a non-nil live session id ⇒ attached. Enabled is sent explicitly to mirror the
// embedded create's Enabled:true.
type WatcherCreateDTO struct {
	Name            string                `json:"name"`
	Task            string                `json:"task"`
	Schedule        config.ScheduleConfig `json:"schedule"`
	Model           string                `json:"model,omitempty"`
	Enabled         *bool                 `json:"enabled,omitempty"`
	ReportToSession *string               `json:"report_to_session,omitempty"`
}

// ListWatchers fetches the watchers visible to sessionID: GET /watchers lists
// free-running watchers, and ?session_id=<id> additionally includes that
// session's attached watchers (the scoping the daemon's ListWatchers enforces).
func (c *APIClient) ListWatchers(sessionID string) ([]WatcherDTO, error) {
	path := "/watchers"
	if sessionID != "" {
		q := url.Values{}
		q.Set("session_id", sessionID)
		path += "?" + q.Encode()
	}
	var out []WatcherDTO
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateWatcher registers a new watcher (POST /watchers) and returns the created
// watcher. The daemon decides the kind from req.ReportToSession.
func (c *APIClient) CreateWatcher(req WatcherCreateDTO) (WatcherDTO, error) {
	var out WatcherDTO
	if err := c.do(http.MethodPost, "/watchers", req, &out); err != nil {
		return WatcherDTO{}, err
	}
	return out, nil
}

// SetWatcherEnabled drives a watcher (by id or name) to an explicit schedule
// state via PUT /watchers/:id/enabled. The :id segment accepts an id or a name;
// the daemon resolves it (unknown → 404, ambiguous name → 409).
func (c *APIClient) SetWatcherEnabled(idOrName string, enabled bool) error {
	return c.do(http.MethodPut, "/watchers/"+url.PathEscape(idOrName)+"/enabled",
		map[string]bool{"enabled": enabled}, nil)
}

// RunWatcher fires a watcher (by id or name) immediately, ignoring its schedule
// and enabled state, via POST /watchers/:id/run.
func (c *APIClient) RunWatcher(idOrName string) error {
	return c.do(http.MethodPost, "/watchers/"+url.PathEscape(idOrName)+"/run", nil, nil)
}

// StopWatcher cancels a watcher's in-flight fire (by id or name) without stopping
// its schedule, via POST /watchers/:id/stop.
func (c *APIClient) StopWatcher(idOrName string) error {
	return c.do(http.MethodPost, "/watchers/"+url.PathEscape(idOrName)+"/stop", nil, nil)
}

// DeleteWatcher unregisters a watcher (by id or name) entirely via
// DELETE /watchers/:id.
func (c *APIClient) DeleteWatcher(idOrName string) error {
	return c.do(http.MethodDelete, "/watchers/"+url.PathEscape(idOrName), nil, nil)
}

// --- Custom commands (issue #403) -------------------------------------------

// CommandParamDTO mirrors the server's commandParamView.
type CommandParamDTO struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
}

// CommandVersionDTO mirrors the server's commandVersionView.
type CommandVersionDTO struct {
	Version    int               `json:"version"`
	Template   string            `json:"template"`
	Parameters []CommandParamDTO `json:"parameters,omitempty"`
	Model      string            `json:"model,omitempty"`
	Agent      string            `json:"agent,omitempty"`
	Subtask    bool              `json:"subtask,omitempty"`
	SavedAt    string            `json:"saved_at"`
}

// CommandDTO mirrors the server's commandView for the /commands endpoints.
type CommandDTO struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Parameters  []CommandParamDTO   `json:"parameters,omitempty"`
	Template    string              `json:"template"`
	Model       string              `json:"model,omitempty"`
	Agent       string              `json:"agent,omitempty"`
	Subtask     bool                `json:"subtask,omitempty"`
	Version     int                 `json:"version"`
	Versions    []CommandVersionDTO `json:"versions,omitempty"`
	// Warnings carries save-time template warnings on a create/update response
	// (mirrors the server's commandView.Warnings); empty on list/get.
	Warnings []string `json:"warnings,omitempty"`
}

// ListCommands returns the daemon's custom commands.
func (c *APIClient) ListCommands() ([]CommandDTO, error) {
	var out []CommandDTO
	if err := c.do(http.MethodGet, "/commands", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetCommand fetches one custom command by name.
func (c *APIClient) GetCommand(name string) (CommandDTO, error) {
	var out CommandDTO
	if err := c.do(http.MethodGet, "/commands/"+url.PathEscape(name), nil, &out); err != nil {
		return CommandDTO{}, err
	}
	return out, nil
}

// CreateCommand creates a custom command on the daemon (version 1).
func (c *APIClient) CreateCommand(def CommandDTO) error {
	return c.do(http.MethodPost, "/commands", def, nil)
}

// UpdateCommand records a new version of an existing command on the daemon.
func (c *APIClient) UpdateCommand(def CommandDTO) error {
	return c.do(http.MethodPut, "/commands/"+url.PathEscape(def.Name), def, nil)
}

// DeleteCommand removes a custom command (and its history) from the daemon.
func (c *APIClient) DeleteCommand(name string) error {
	return c.do(http.MethodDelete, "/commands/"+url.PathEscape(name), nil, nil)
}

// GetCommandHistory returns a command's append-only version history.
func (c *APIClient) GetCommandHistory(name string) ([]CommandVersionDTO, error) {
	var out []CommandVersionDTO
	if err := c.do(http.MethodGet, "/commands/"+url.PathEscape(name)+"/history", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RestoreCommandVersion restores version v of a command on the daemon (recorded
// as a new version).
func (c *APIClient) RestoreCommandVersion(name string, v int) error {
	return c.do(http.MethodPost, "/commands/"+url.PathEscape(name)+"/restore",
		map[string]int{"version": v}, nil)
}

// GetStatistics returns the daemon's aggregate statistics report.
func (c *APIClient) GetStatistics() (stats.Report, error) {
	var out stats.Report
	if err := c.do(http.MethodGet, "/stats", nil, &out); err != nil {
		return stats.Report{}, err
	}
	return out, nil
}

// DaemonStatusDTO mirrors the server's daemonStatusView (GET /api/daemon/status):
// the one-call summary the "Daemon status" menu renders (issue #358 §6).
type DaemonStatusDTO struct {
	PID           int      `json:"pid"`
	StartedAt     string   `json:"started_at"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	LiveSessions  int      `json:"live_sessions"`
	Watchers      int      `json:"watchers"`
	MCPServers    []string `json:"mcp_servers"`
}

// DaemonStatus fetches the daemon's live summary (pid, uptime, session/watcher/MCP
// counts) for the status dialog. It is a single round-trip the TUI uses instead of
// stitching together /health + /sessions + /watchers.
func (c *APIClient) DaemonStatus() (DaemonStatusDTO, error) {
	var out DaemonStatusDTO
	if err := c.do(http.MethodGet, "/daemon/status", nil, &out); err != nil {
		return DaemonStatusDTO{}, err
	}
	return out, nil
}

// NOTE: watcher management over the wire (GET/POST/PUT/DELETE /api/watchers*) is
// intentionally not exposed by this client. It is an explicitly deferred Phase-3
// API-gap item, out of scope for this bounded remote-client slice.

// --- approvals --------------------------------------------------------------

// ListApprovals returns every pending interactive gate (permission/edit-review)
// the daemon is blocked on.
func (c *APIClient) ListApprovals() ([]ApprovalDTO, error) {
	var out []ApprovalDTO
	if err := c.do(http.MethodGet, "/approvals", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DecideApproval delivers a decision for a pending approval. decision is the
// wire token the server expects ("allow"/"deny"/"always"/"always_deny" for a
// permission; "approve"/"approve_all"/"reject" for an edit review).
//
// The endpoint is idempotent (issue #560): a decision that lands after its prompt
// was removed is reconciled, not rejected. The returned status is "resolved" for
// an in-time decision and "late" for a reconciled one (e.g. a sticky "always"
// grant the daemon persisted even though the original prompt had timed out), so
// the caller can tell the user the grant will apply going forward. A genuinely
// unknown id still returns an error (a 404 the caller retries then surfaces).
func (c *APIClient) DecideApproval(aid, decision string) (status string, err error) {
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if derr := c.do(http.MethodPost, "/approvals/"+url.PathEscape(aid)+"/decision",
		map[string]string{"decision": decision}, &out); derr != nil {
		return "", derr
	}
	return out.Status, nil
}

// --- SSE event stream -------------------------------------------------------

// StreamEvents opens the global SSE stream (GET /api/events) and delivers each
// decoded GlobalEventDTO on the returned channel until ctx is cancelled or the
// stream ends, at which point the channel is closed. The initial connect is
// synchronous, so a connection failure is returned to the caller (rather than
// hidden in the goroutine); subsequent stream errors simply close the channel.
//
// The returned channel is buffered so a momentary slow consumer does not block
// the reader goroutine, mirroring the server hub's best-effort live semantics.
func (c *APIClient) StreamEvents(ctx context.Context) (<-chan GlobalEventDTO, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The body must stay open for the whole stream: it is closed in the reader
	// goroutine below (and in the non-2xx branch), not in this function, so
	// bodyclose's intra-function check cannot see the close.
	resp, err := c.http.Do(req) //nolint:bodyclose // closed in the stream goroutine / error branch
	if err != nil {
		return nil, fmt.Errorf("open event stream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("open event stream: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	out := make(chan GlobalEventDTO, 64)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		for ev := range parseSSE(ctx, resp.Body) {
			if ev.name == notificationEventName {
				// A backend notification (issue #358 §9): hand it to the notification
				// handler (the TUI's desktop notifier) rather than the session-event sink.
				if h := c.notificationHandler(); h != nil {
					var n NotificationDTO
					if err := json.Unmarshal([]byte(ev.data), &n); err == nil {
						h(n)
					}
				}
				continue
			}
			if ev.name == approvalEventName {
				// An "approval pending" nudge (issue #569): trigger an immediate
				// /approvals re-scan. The body (an approval id) is intentionally ignored
				// — GET /approvals is authoritative and the poller dedups by id.
				if h := c.approvalSignalHandler(); h != nil {
					h()
				}
				continue
			}
			if ev.name == approvalExpiredEventName {
				// A presented approval timed out before it was answered (issue #569):
				// hand it to the expired handler so the TUI tells the user their late
				// answer no longer applies, rather than the deny going unmentioned.
				if h := c.approvalExpiredHandler(); h != nil {
					var d ApprovalExpiredDTO
					if err := json.Unmarshal([]byte(ev.data), &d); err == nil {
						h(d)
					}
				}
				continue
			}
			var ge GlobalEventDTO
			if err := json.Unmarshal([]byte(ev.data), &ge); err != nil {
				continue // skip a malformed frame rather than tearing down the stream
			}
			select {
			case out <- ge:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// StreamLogs opens the daemon's diagnostic-log SSE stream (GET /api/logs/stream)
// and delivers each decoded LogRecordDTO on the returned channel until ctx is
// cancelled or the stream ends, at which point the channel is closed (issue
// #562). Like StreamEvents the initial connect is synchronous (so a failure is
// returned, not hidden) and it reuses the same auth and tolerant SSE parser; the
// reconnect/backoff is the caller's (StreamLogsTo) — a best-effort log tail, not
// a lossless stream.
func (c *APIClient) StreamLogs(ctx context.Context) (<-chan LogRecordDTO, error) {
	return c.StreamLogsSince(ctx, "")
}

// StreamLogsSince is StreamLogs with a resume cursor (issue #562): since is the
// wire timestamp (RFC3339Nano) of the last record the caller already has, sent as
// ?since= so the server's catch-up history skips records already delivered. An
// empty since primes the full recent history (a fresh connection). It is the
// reconnect entry point used by RemoteClient.StreamLogsTo to avoid duplicating
// the interlaced [daemon] lines across a reconnect.
func (c *APIClient) StreamLogsSince(ctx context.Context, since string) (<-chan LogRecordDTO, error) {
	path := "/logs/stream"
	if since != "" {
		path += "?since=" + url.QueryEscape(since)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The body stays open for the whole stream: it is closed in the reader goroutine
	// below (and in the non-2xx branch), not in this function.
	resp, err := c.http.Do(req) //nolint:bodyclose // closed in the stream goroutine / error branch
	if err != nil {
		return nil, fmt.Errorf("open log stream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("open log stream: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	out := make(chan LogRecordDTO, 64)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		for ev := range parseSSE(ctx, resp.Body) {
			if ev.name != logEventName {
				continue // ignore keep-alives and any non-log frame
			}
			var rec LogRecordDTO
			if err := json.Unmarshal([]byte(ev.data), &rec); err != nil {
				continue // skip a malformed frame rather than tearing down the stream
			}
			select {
			case out <- rec:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// sseFrame is one parsed Server-Sent Event: its event-name and concatenated
// data payload (multi-line data is joined with newlines per the SSE spec).
type sseFrame struct {
	name string
	data string
}

// parseSSE reads an SSE response body line-by-line and emits one sseFrame per
// blank-line-terminated event. It closes the returned channel when the body
// ends, errors, or ctx is cancelled. Comment lines (":"-prefixed) and unknown
// fields are ignored, matching a tolerant SSE client.
func parseSSE(ctx context.Context, body io.Reader) <-chan sseFrame {
	out := make(chan sseFrame, 16)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		// SSE data lines can be large (a full final answer); raise the line cap
		// well above bufio's 64 KiB default so a big event is not truncated.
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		var name string
		var data []string
		flush := func() bool {
			if name == "" && len(data) == 0 {
				return true
			}
			fr := sseFrame{name: name, data: strings.Join(data, "\n")}
			name, data = "", nil
			select {
			case out <- fr:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if !flush() {
					return
				}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive — ignore
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(line[len("event:"):])
			case strings.HasPrefix(line, "data:"):
				// Per the SSE spec a single leading space after the colon is stripped.
				data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
			default:
				// id:/retry:/unknown field — ignore
			}
			if ctx.Err() != nil {
				return
			}
		}
		// A trailing event with no terminating blank line is still delivered.
		flush()
	}()
	return out
}
