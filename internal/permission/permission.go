// Package permission provides a single, resource+action-aware authorization
// gate for every side-effecting tool (file ops, shell, sub-agents, network).
//
// A Service evaluates a (action, resource) pair to one of three effects:
// allow, deny or ask. On "ask" it consults a Prompter (implemented by the UI);
// the user's decision may be persisted ("always" / "always deny") to
// permissions.json so the question is not asked again.
package permission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Action identifies the kind of operation being gated.
type Action string

const (
	ActionRead        Action = "read"        // read a file inside the workspace
	ActionWrite       Action = "write"       // write/edit a file inside the workspace
	ActionShell       Action = "shell"       // run a shell command (session-wide gate)
	ActionExternal    Action = "external"    // touch a path outside the workspace
	ActionNetwork     Action = "network"     // network access
	ActionSubagent    Action = "subagent"    // spawn a sub-agent
	ActionMCP         Action = "mcp"         // launch/connect to an MCP server
	ActionDiagnostics Action = "diagnostics" // run the configured compiler/linter
	ActionVerify      Action = "verify"      // run the configured test command
)

// Effect is the resolved policy for a request.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
	EffectAsk   Effect = "ask"
)

// Decision is a user's answer to an "ask" prompt.
type Decision string

const (
	DecisionAllow      Decision = "allow"       // allow once
	DecisionDeny       Decision = "deny"        // deny once
	DecisionAlways     Decision = "always"      // allow and persist
	DecisionAlwaysDeny Decision = "always_deny" // deny and persist
)

// Rule is a static policy entry. Action "*" matches any action; Resource
// supports a trailing "*" wildcard or "*" for all.
type Rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// RequestContext identifies the session (and optionally the sub-agent) on whose
// behalf a decision is requested, so the UI can badge the requesting session's
// sidebar node, alert when it is unfocused and let the user jump straight to it.
// The zero value is valid: headless or CLI callers leave it empty and the prompt
// stays session-agnostic.
type RequestContext struct {
	SessionID string // requesting session id ("" if unknown)
	Agent     string // requesting sub-agent id ("" for the session's main agent)
}

// Request is handed to a Prompter when a decision is needed.
type Request struct {
	Action   Action
	Resource string
	Detail   string         // human context, e.g. the shell command being run
	Context  RequestContext // who is asking (for alerting/routing); optional
}

// Prompter asks the user for a decision. It blocks until the user answers and
// is always invoked off the UI thread, so implementations must marshal to their
// UI and wait for the reply.
type Prompter interface {
	AskPermission(Request) Decision
}

// DeniedError is returned by Check when an operation is not permitted.
type DeniedError struct {
	Action   Action
	Resource string
}

func (e *DeniedError) Error() string {
	if e.Resource == "" {
		return "permission denied: " + string(e.Action)
	}
	return "permission denied: " + string(e.Action) + " on " + e.Resource
}

// AuditSink records the outcome of a resolved permission check so it can be
// written to an append-only audit trail (issue #51). allowed reports whether the
// request was authorized. It must not block; it is called on the request path.
type AuditSink func(rc RequestContext, action Action, resource string, allowed bool)

// Service is the central permission gate. It is safe for concurrent use.
type Service struct {
	mu        sync.Mutex
	configDir string
	rules     []Rule
	saved     map[string]Decision
	prompter  Prompter
	audit     AuditSink
}

// New creates a Service whose persisted "always" decisions live under
// configDir/permissions.json. configDir may be empty to disable persistence.
func New(configDir string) *Service {
	s := &Service{
		configDir: configDir,
		saved:     make(map[string]Decision),
	}
	s.load()
	return s
}

// SetPrompter installs the interactive prompter. With no prompter, "ask"
// resolves to deny (safe default for headless runs).
func (s *Service) SetPrompter(p Prompter) {
	s.mu.Lock()
	s.prompter = p
	s.mu.Unlock()
}

// SetAuditSink installs the sink that records resolved permission decisions. A
// nil sink (the default) disables auditing.
func (s *Service) SetAuditSink(sink AuditSink) {
	s.mu.Lock()
	s.audit = sink
	s.mu.Unlock()
}

// AddRule appends a static policy rule.
func (s *Service) AddRule(r Rule) {
	s.mu.Lock()
	s.rules = append(s.rules, r)
	s.mu.Unlock()
}

func key(a Action, resource string) string { return string(a) + ":" + resource }

// effect resolves the policy for (action, resource) under the lock.
func (s *Service) effect(a Action, resource string) Effect {
	// Persisted decisions win. For path-style actions an allowed ancestor root
	// covers its descendants.
	for k, d := range s.saved {
		ka, kr := splitKey(k)
		if ka != a {
			continue
		}
		if kr == resource || (isPathAction(a) && pathUnder(resource, kr)) {
			switch d {
			case DecisionAlways:
				return EffectAllow
			case DecisionAlwaysDeny:
				return EffectDeny
			}
		}
	}
	for _, r := range s.rules {
		if r.Action != "*" && r.Action != string(a) {
			continue
		}
		if wildcardMatch(resource, r.Resource) {
			return Effect(r.Effect)
		}
	}
	return EffectAsk
}

// Check authorizes (action, resource), prompting if necessary. It returns nil
// when allowed and a *DeniedError when denied.
func (s *Service) Check(a Action, resource string) error {
	return s.CheckWithDetail(a, resource, "")
}

// CheckWithDetail is Check with extra human context for the prompt.
func (s *Service) CheckWithDetail(a Action, resource, detail string) error {
	return s.CheckWithContext(RequestContext{}, a, resource, detail)
}

// CheckWithContext is CheckWithDetail that additionally records which session
// (and sub-agent) is asking, so the prompter can alert and route the user to the
// requesting session. The context is carried only when the request reaches the
// prompter; persisted and rule-based decisions are session-agnostic.
func (s *Service) CheckWithContext(rc RequestContext, a Action, resource, detail string) (err error) {
	s.mu.Lock()
	eff := s.effect(a, resource)
	prompter := s.prompter
	sink := s.audit
	s.mu.Unlock()

	// Record the resolved decision on the audit trail, however it is reached
	// (rule, persisted, or interactive prompt). err==nil means allowed.
	if sink != nil {
		defer func() { sink(rc, a, resource, err == nil) }()
	}

	switch eff {
	case EffectAllow:
		return nil
	case EffectDeny:
		return &DeniedError{Action: a, Resource: resource}
	}

	if prompter == nil {
		return &DeniedError{Action: a, Resource: resource}
	}

	switch prompter.AskPermission(Request{Action: a, Resource: resource, Detail: detail, Context: rc}) {
	case DecisionAllow:
		return nil
	case DecisionAlways:
		s.persist(a, resource, DecisionAlways)
		return nil
	case DecisionAlwaysDeny:
		s.persist(a, resource, DecisionAlwaysDeny)
		return &DeniedError{Action: a, Resource: resource}
	default: // DecisionDeny and anything unexpected
		return &DeniedError{Action: a, Resource: resource}
	}
}

// persist records a sticky decision and flushes the snapshot to disk. The map
// mutation and its marshalling happen under a single lock so a concurrent
// persist cannot interleave between them; only the file I/O runs outside the
// lock, on the stable snapshot.
func (s *Service) persist(a Action, resource string, d Decision) {
	s.mu.Lock()
	s.saved[key(a, resource)] = d
	data, err := json.MarshalIndent(savedFile{Saved: s.saved}, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return
	}
	_ = s.write(data)
}

func (s *Service) configPath() string {
	if s.configDir == "" {
		return ""
	}
	return filepath.Join(s.configDir, "permissions.json")
}

type savedFile struct {
	Saved map[string]Decision `json:"saved"`
}

func (s *Service) load() {
	path := s.configPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path) //nolint:gosec // reads caller-controlled permission store path
	if err != nil {
		return
	}
	var f savedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	if f.Saved != nil {
		s.saved = f.Saved
	}
}

// write replaces the persisted snapshot on disk. The grant file records what
// the agent is permitted to do, so it is created owner-only: the directory with
// 0700 and the file with 0600, never readable by other local users (CWE-732).
func (s *Service) write(data []byte) error {
	path := s.configPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(s.configDir, 0700); err != nil {
		return fmt.Errorf("create permission dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write permission file: %w", err)
	}
	return nil
}

func splitKey(k string) (Action, string) {
	i := strings.IndexByte(k, ':')
	if i < 0 {
		return Action(k), ""
	}
	return Action(k[:i]), k[i+1:]
}

func isPathAction(a Action) bool {
	return a == ActionExternal || a == ActionRead || a == ActionWrite
}

// pathUnder reports whether child is equal to or nested under parent. Both are
// expected to be cleaned absolute paths.
func pathUnder(child, parent string) bool {
	if parent == "" || child == "" {
		return false
	}
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func wildcardMatch(value, pattern string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return value == pattern
}
