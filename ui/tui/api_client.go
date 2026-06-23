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
	"strings"
	"time"

	"gogent/internal/config"
	"gogent/internal/stats"
)

// APIClient is a thin HTTP/SSE client for the existing internal/server /api
// surface (issue #358, Phase 2). It is the transport the attached ("thin
// client") TUI uses to drive a running daemon: every Handlers call the remote
// TUI makes maps to one request here, and the live event stream is consumed
// from the global SSE endpoint. It is stdlib-only (net/http) and safe for
// concurrent use — the underlying http.Client is, and the client holds no
// mutable state of its own.
//
// Two transports are supported, selected by the connect address scheme:
//   - unix:///path/to/daemon.sock — the local daemon socket (default). The
//     socket's 0600 filesystem permissions are the access gate; the server
//     treats a Unix-socket caller as a local human, so no token is needed.
//   - http://host:port | https://host:port — TCP, for a remote daemon reached
//     over (manually forwarded) SSH or a trusted network. A bearer token
//     (GOGENT_HTTP_TOKEN on the daemon) authenticates non-loopback callers.
type APIClient struct {
	http  *http.Client
	base  string // request base, e.g. "http://unix" or "http://host:port"
	token string // optional bearer token (TCP auth); empty for the local socket
}

// quickTimeout bounds the short request/response calls (create, stop, settings,
// …). It deliberately does NOT apply to SendMessage or the SSE stream, which
// run for the lifetime of a turn / the whole attachment and must not be capped.
const quickTimeout = 30 * time.Second

// NewAPIClient builds a client for the daemon at addr. addr is a scheme-
// qualified connect address:
//
//	unix:///home/u/.gogent/daemon.sock
//	http://localhost:8080
//	https://host:8080
//
// token is an optional bearer token used only for the TCP transports (it is
// harmless but unnecessary over the Unix socket). A bare path or an unknown
// scheme is rejected so a malformed --connect value fails fast and visibly.
func NewAPIClient(addr, token string) (*APIClient, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parse connect address %q: %w", addr, err)
	}
	switch u.Scheme {
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
		return nil, fmt.Errorf("unsupported connect scheme %q (want unix:// | http:// | https://)", u.Scheme)
	}
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

// MessageDTO mirrors the server's messageView (transcript + send responses).
type MessageDTO struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name"`
	APIType         string   `json:"api_type"`
	Endpoint        string   `json:"endpoint"`
	Project         string   `json:"project"`
	Location        string   `json:"location"`
	Model           string   `json:"model"`
	HasAPIKey       bool     `json:"has_api_key"`
	Temperature     float32  `json:"temperature"`
	TopP            float32  `json:"top_p"`
	MaxTokens       int      `json:"max_tokens"`
	ContextWindow   int      `json:"context_window"`
	ReasoningEffort string   `json:"reasoning_effort"`
	EffortOptions   []string `json:"effort_options"`
	Thinking        *bool    `json:"thinking"`
	Free            bool     `json:"free"`
}

// ToModelConfig projects a redacted ModelDTO back into a config.ModelConfig for
// the TUI's model dropdown and editor. The api_key is intentionally left empty
// (the server redacts it); an empty key in a later PUT /models preserves the
// daemon's stored key, so a view→edit→save round-trip never wipes it.
func (m ModelDTO) ToModelConfig() config.ModelConfig {
	return config.ModelConfig{
		Name:            m.Name,
		DisplayName:     m.DisplayName,
		APIType:         m.APIType,
		Endpoint:        m.Endpoint,
		Project:         m.Project,
		Location:        m.Location,
		Model:           m.Model,
		Temperature:     m.Temperature,
		TopP:            m.TopP,
		MaxTokens:       m.MaxTokens,
		ContextWindow:   m.ContextWindow,
		ReasoningEffort: m.ReasoningEffort,
		EffortOptions:   m.EffortOptions,
		Thinking:        m.Thinking,
		Free:            m.Free,
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
	SubAgents   config.SubAgentConfig `json:"sub_agents"`
	Timeouts    config.TimeoutConfig  `json:"timeouts"`
	Budget      config.BudgetConfig   `json:"budget"`
	ReviewEdits bool                  `json:"review_edits"`
}

// WatcherDTO mirrors the server's watcherView.
type WatcherDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Target     string `json:"target"`
	Task       string `json:"task"`
	Schedule   string `json:"schedule"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"`
	NextFire   string `json:"next_fire"`
	LastRun    string `json:"last_run"`
	LastResult string `json:"last_result"`
	LastError  string `json:"last_error"`
}

// CreateWatcherRequest mirrors the server's createWatcherRequest body.
type CreateWatcherRequest struct {
	Name            string                `json:"name"`
	Task            string                `json:"task"`
	Schedule        config.ScheduleConfig `json:"schedule"`
	Model           string                `json:"model,omitempty"`
	Enabled         *bool                 `json:"enabled,omitempty"`
	ReportToSession *string               `json:"report_to_session,omitempty"`
}

// --- health & sessions ------------------------------------------------------

// Health reports whether the daemon answers GET /api/health. It is used to
// confirm a live attachment before building the TUI.
func (c *APIClient) Health() error {
	return c.do(http.MethodGet, "/health", nil, nil)
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

// SendMessage runs a turn on the daemon and blocks until it completes. It is
// the remote equivalent of the embedded OnSend background goroutine: progress
// (thoughts, tool calls, the final answer) is delivered out-of-band over the
// global SSE stream, so the returned MessageDTO is only used to detect an
// outright failure. ctx (a background context) is NOT bounded by quickTimeout —
// a turn may legitimately run for minutes.
func (c *APIClient) SendMessage(ctx context.Context, id, message, modelName, effort string) (MessageDTO, error) {
	body := sendMessageBody{Message: message, Model: modelName, Effort: effort}
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

// ApprovePlan executes a session's pending plan on the daemon. Like SendMessage
// it runs a (potentially long) turn whose progress streams over SSE, so ctx is
// a background context and the response is used only to detect failure.
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

// GetStatistics returns the daemon's aggregate statistics report.
func (c *APIClient) GetStatistics() (stats.Report, error) {
	var out stats.Report
	if err := c.do(http.MethodGet, "/stats", nil, &out); err != nil {
		return stats.Report{}, err
	}
	return out, nil
}

// --- watchers ---------------------------------------------------------------

// ListWatchers returns the watchers visible to sessionID (free-running plus that
// session's attached watchers; "" yields the free-running ones only).
func (c *APIClient) ListWatchers(sessionID string) ([]WatcherDTO, error) {
	path := "/watchers"
	if sessionID != "" {
		path += "?session_id=" + url.QueryEscape(sessionID)
	}
	var out []WatcherDTO
	if err := c.do(http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateWatcher registers a new watcher on the daemon.
func (c *APIClient) CreateWatcher(req CreateWatcherRequest) (WatcherDTO, error) {
	var out WatcherDTO
	if err := c.do(http.MethodPost, "/watchers", req, &out); err != nil {
		return WatcherDTO{}, err
	}
	return out, nil
}

// SetWatcherEnabled enables or disables a watcher (by id or name).
func (c *APIClient) SetWatcherEnabled(idOrName string, enabled bool) error {
	return c.do(http.MethodPut, "/watchers/"+url.PathEscape(idOrName)+"/enabled",
		map[string]bool{"enabled": enabled}, nil)
}

// RunWatcher fires a watcher immediately (by id or name).
func (c *APIClient) RunWatcher(idOrName string) error {
	return c.do(http.MethodPost, "/watchers/"+url.PathEscape(idOrName)+"/run", nil, nil)
}

// StopWatcher cancels a watcher's in-flight fire (by id or name).
func (c *APIClient) StopWatcher(idOrName string) error {
	return c.do(http.MethodPost, "/watchers/"+url.PathEscape(idOrName)+"/stop", nil, nil)
}

// DeleteWatcher unregisters a watcher (by id or name).
func (c *APIClient) DeleteWatcher(idOrName string) error {
	return c.do(http.MethodDelete, "/watchers/"+url.PathEscape(idOrName), nil, nil)
}

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
// permission; "approve"/"approve_all"/"reject" for an edit review). A 404/409
// (already resolved or timed out) is surfaced as an error the caller can ignore.
func (c *APIClient) DecideApproval(aid, decision string) error {
	return c.do(http.MethodPost, "/approvals/"+url.PathEscape(aid)+"/decision",
		map[string]string{"decision": decision}, nil)
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
