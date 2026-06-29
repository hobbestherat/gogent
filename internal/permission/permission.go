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
	"regexp"
	"strings"
	"sync"

	"gogent/internal/diag"
)

// Action identifies the kind of operation being gated.
type Action string

const (
	ActionRead     Action = "read"     // read a file inside the workspace
	ActionWrite    Action = "write"    // write/edit a file inside the workspace
	ActionShell    Action = "shell"    // run a shell command (session-wide gate)
	ActionExternal Action = "external" // touch a path outside the workspace
	ActionNetwork  Action = "network"  // network access
	ActionSubagent Action = "subagent" // spawn a sub-agent
	ActionMCP      Action = "mcp"      // launch/connect to an MCP server
	ActionWatcher  Action = "watcher"  // start/manage a scheduled watcher (issue #329)
)

// knownActions is the set of Action constants a rule may target. A rule's action
// must be one of these or the "*" wildcard; anything else (e.g. a typo like
// "shel") is rejected by AddRule so a guardrail that could never match is not
// silently loaded (issue #355).
var knownActions = map[Action]bool{
	ActionRead: true, ActionWrite: true, ActionShell: true, ActionExternal: true,
	ActionNetwork: true, ActionSubagent: true, ActionMCP: true, ActionWatcher: true,
}

// validRuleAction reports whether a is a legal rule action: a known Action
// constant or the "*" wildcard.
func validRuleAction(a string) bool {
	return a == "*" || knownActions[Action(a)]
}

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
// supports a trailing "*" wildcard or "*" for all. For path-style actions
// (read/write/external) a literal Resource also matches any path nested under it.
//
// DetailPattern, when non-empty, is a Go regular expression that must match the
// request's Detail (human context, e.g. the shell command text) for the rule to
// apply. ActionShell gates with Resource "" and carries the command in Detail,
// so DetailPattern is what enables command-level guardrails such as denying
// "rm -rf /". A rule with an invalid DetailPattern regex is rejected by AddRule.
type Rule struct {
	Action        string `json:"action"`
	Resource      string `json:"resource"`
	Effect        string `json:"effect"`
	DetailPattern string `json:"detail_pattern,omitempty"`
}

// compiledRule is a Rule with its DetailPattern pre-compiled so effect() does no
// per-request regex compilation. detailRE is nil when DetailPattern is empty.
type compiledRule struct {
	Rule
	detailRE *regexp.Regexp
}

// matches reports whether the rule applies to (action, resource, detail).
func (r compiledRule) matches(a Action, resource, detail string) bool {
	if r.Action != "*" && r.Action != string(a) {
		return false
	}
	resourceMatch := wildcardMatch(resource, r.Resource) ||
		(isPathAction(a) && pathUnder(resource, r.Resource))
	if !resourceMatch {
		return false
	}
	if r.detailRE != nil {
		return r.detailRE.MatchString(detail)
	}
	return true
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
	rules     []compiledRule
	saved     map[string]Decision
	prompter  Prompter
	audit     AuditSink
	logger    *diag.Logger

	// yolo is the global auto-approve default (issue #356): when set, an "ask"
	// resolves to allow instead of prompting/denying. sessionYolo holds per-session
	// overrides set at runtime by the TUI toggle. Neither can punch through a
	// rules.json hard-deny guardrail (#355) — deny is resolved before the bypass.
	yolo        bool
	sessionYolo map[string]bool
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

// SetLogger installs the diagnostics logger used to report persistence failures
// (a failed permissions.json write/read) so a dropped grant is diagnosable in
// gogent.log instead of silently swallowed (issue #560). A nil *diag.Logger is a
// safe no-op.
func (s *Service) SetLogger(l *diag.Logger) {
	s.mu.Lock()
	s.logger = l
	s.mu.Unlock()
}

// AddRule appends a static policy rule. A rule whose Action is not a known
// Action constant or "*", whose Effect is not "allow" or "deny", or whose
// DetailPattern is not a valid Go regex, is rejected and an error returned (the
// rule is not registered) so callers can log and skip it. Rejecting unknown
// actions keeps a typo'd guardrail (e.g. "shel") from silently loading as a rule
// that could never match (issue #355).
func (s *Service) AddRule(r Rule) error {
	cr := compiledRule{Rule: r}
	if !validRuleAction(r.Action) {
		return fmt.Errorf("invalid action %q (want an Action constant or \"*\")", r.Action)
	}
	switch Effect(r.Effect) {
	case EffectAllow, EffectDeny:
	default:
		return fmt.Errorf("invalid effect %q (want allow or deny)", r.Effect)
	}
	if r.DetailPattern != "" {
		re, err := regexp.Compile(r.DetailPattern)
		if err != nil {
			return fmt.Errorf("invalid detail_pattern %q: %w", r.DetailPattern, err)
		}
		cr.detailRE = re
	}
	s.mu.Lock()
	s.rules = append(s.rules, cr)
	s.mu.Unlock()
	return nil
}

// rulesFile is the on-disk shape of ~/.gogent/rules.json (issue #355).
type rulesFile struct {
	Rules []Rule `json:"rules"`
}

// LoadRules reads dir/rules.json and registers each valid rule via AddRule.
//
// It is deliberately lenient (mirroring config loading): a missing file adds
// nothing and returns no error; a corrupt/unreadable file adds nothing and
// returns a single error; individual rules that AddRule rejects — an unknown
// action (not an Action constant or "*"), an unknown effect, or an invalid
// detail_pattern regex — are skipped and reported. It never panics. The caller
// (gogent) logs the returned errors. dir "" is a no-op.
//
// Because deny rules are hard guardrails resolved before everything else in
// effect(), loading them after the default allow-alls is fine — order between
// rules does not affect which deny wins.
func (s *Service) LoadRules(dir string) []error {
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "rules.json")) //nolint:gosec // caller-controlled config dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("read rules.json: %w", err)}
	}
	var f rulesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return []error{fmt.Errorf("parse rules.json: %w", err)}
	}
	var errs []error
	for i, r := range f.Rules {
		if err := s.AddRule(r); err != nil {
			errs = append(errs, fmt.Errorf("rules.json rule %d (%s %s): %w", i, r.Action, r.Resource, err))
		}
	}
	return errs
}

// SetYolo sets the global auto-approve default (issue #356). When on, requests
// that would otherwise ask resolve to allow for every session that has no
// explicit per-session override. It never bypasses rules.json deny guardrails.
func (s *Service) SetYolo(on bool) {
	s.mu.Lock()
	s.yolo = on
	s.mu.Unlock()
}

// SetSessionYolo sets a per-session auto-approve override (issue #356), used by
// the TUI runtime toggle. An empty sessionID is ignored.
func (s *Service) SetSessionYolo(sessionID string, on bool) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	if s.sessionYolo == nil {
		s.sessionYolo = make(map[string]bool)
	}
	s.sessionYolo[sessionID] = on
	s.mu.Unlock()
}

// EffectiveYolo reports the resolved auto-approve state for a session: the global
// default unless the session has an explicit override. It is the seam gogent uses
// to decide a session's step cap (unlimited under yolo).
func (s *Service) EffectiveYolo(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveYoloLocked(sessionID)
}

// effectiveYoloLocked computes the effective yolo state; the caller holds s.mu.
func (s *Service) effectiveYoloLocked(sessionID string) bool {
	if on, ok := s.sessionYolo[sessionID]; ok {
		return on
	}
	return s.yolo
}

func key(a Action, resource string) string { return string(a) + ":" + resource }

// effect resolves the policy for (action, resource, detail) under the lock as a
// fixed priority cascade (issues #355/#356):
//
//  1. rules.json DENY guardrails — if any matches, EffectDeny. Hard stop:
//     nothing below (persisted allows, allow rules, the yolo bypass) overrides it.
//  2. Persisted decisions (permissions.json) — always→allow, always_deny→deny.
//  3. ALLOW rules (the default workspace allow-alls and any rules.json allow).
//  4. Fall through → EffectAsk (the yolo bypass, if active, is applied by the
//     caller — CheckWithContext — only on this ask result, after deny is ruled out).
//
// detail is threaded through so detail_pattern rules can gate on the request's
// human context (e.g. the shell command text).
func (s *Service) effect(a Action, resource, detail string) Effect {
	// 1. Hard-deny guardrails first — they win over everything.
	for _, r := range s.rules {
		if Effect(r.Effect) == EffectDeny && r.matches(a, resource, detail) {
			return EffectDeny
		}
	}
	// 2. Persisted decisions. For path-style actions an allowed ancestor root
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
	// 3. Allow rules.
	for _, r := range s.rules {
		if Effect(r.Effect) == EffectAllow && r.matches(a, resource, detail) {
			return EffectAllow
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
	eff := s.effect(a, resource, detail)
	// Yolo bypass (issue #356): an otherwise-ask resolves to allow when yolo is
	// active for this request. Deny guardrails (#355) already returned EffectDeny
	// above, so they are never reached by this conversion — they always hold.
	if eff == EffectAsk && s.effectiveYoloLocked(rc.SessionID) {
		eff = EffectAllow
	}
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
	logger := s.logger
	s.mu.Unlock()
	if err != nil {
		// nil-safe: a Logger with a nil receiver/handler discards.
		logger.Errorf("permission: marshal saved decisions: %v", err)
		return
	}
	if werr := s.write(data); werr != nil {
		// Surface a failed grant write so it is diagnosable in gogent.log rather
		// than silently dropped (issue #560). The decision is already applied
		// in-memory for this process; only durability across restart is lost.
		logger.Errorf("permission: persist decision to disk: %v", werr)
	}
}

// Persist records a sticky decision out-of-band — outside the normal
// CheckWithContext cascade — and records it on the audit trail. It exists so the
// daemon's remote-approval bridge can make a late "always"/"always_deny" answer
// stick even after the originating prompt was already resolved or timed out
// (issue #560): the in-time path persists from CheckWithContext, but a decision
// that arrives after the pending approval was removed must be applied here.
//
// Only DecisionAlways/DecisionAlwaysDeny carry sticky state; any other decision
// is per-call and is ignored. The audit entry mirrors the in-time path
// (allowed == DecisionAlways) so a late grant is never off-record (issue #51).
func (s *Service) Persist(rc RequestContext, a Action, resource string, d Decision) {
	if d != DecisionAlways && d != DecisionAlwaysDeny {
		return
	}
	s.persist(a, resource, d)
	s.mu.Lock()
	sink := s.audit
	s.mu.Unlock()
	if sink != nil {
		sink(rc, a, resource, d == DecisionAlways)
	}
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
		// A corrupt store is left untouched (the in-memory default holds); surface
		// it so it is diagnosable rather than a silent re-prompt (issue #560).
		s.logger.Errorf("permission: parse %s: %v", path, err)
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
