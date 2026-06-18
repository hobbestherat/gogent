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
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Request is handed to a Prompter when a decision is needed.
type Request struct {
	Action   Action
	Resource string
	Detail   string // human context, e.g. the shell command being run
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

// Service is the central permission gate. It is safe for concurrent use.
type Service struct {
	mu        sync.Mutex
	configDir string
	rules     []Rule
	saved     map[string]Decision
	prompter  Prompter
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
	s.mu.Lock()
	eff := s.effect(a, resource)
	prompter := s.prompter
	s.mu.Unlock()

	switch eff {
	case EffectAllow:
		return nil
	case EffectDeny:
		return &DeniedError{Action: a, Resource: resource}
	}

	if prompter == nil {
		return &DeniedError{Action: a, Resource: resource}
	}

	switch prompter.AskPermission(Request{Action: a, Resource: resource, Detail: detail}) {
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

func (s *Service) persist(a Action, resource string, d Decision) {
	s.mu.Lock()
	s.saved[key(a, resource)] = d
	s.mu.Unlock()
	s.save()
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
	data, err := os.ReadFile(path)
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

func (s *Service) save() error {
	path := s.configPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(s.configDir, 0755); err != nil {
		return err
	}
	s.mu.Lock()
	data, err := json.MarshalIndent(savedFile{Saved: s.saved}, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
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
