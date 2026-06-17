package fileops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PermissionDecision represents a user's permission decision
type PermissionDecision string

const (
	DecisionAllow  PermissionDecision = "allow"
	DecisionDeny   PermissionDecision = "deny"
	DecisionAsk    PermissionDecision = "ask"
	DecisionAlways PermissionDecision = "always"
)

// PermissionRule represents a permission rule
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// PermissionRequest represents a permission request
type PermissionRequest struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Project  string `json:"project"`
	Source   string `json:"source"`
}

// PermissionResult represents the result of a permission request
type PermissionResult struct {
	ID     string `json:"id"`
	Effect string `json:"effect"`
}

// PermissionService manages file operation permissions
type PermissionService struct {
	mu        sync.RWMutex
	decisions map[string][]PermissionRule
	saved     map[string]PermissionDecision
}

// NewPermissionService creates a new permission service
func NewPermissionService(homeDir string) *PermissionService {
	projectDir := filepath.Join(homeDir, ".gogent")

	ps := &PermissionService{
		decisions: make(map[string][]PermissionRule),
		saved:     make(map[string]PermissionDecision),
	}

	// Default: allow all actions on workspace
	ps.AddRule(PermissionRule{
		Action:   "*",
		Resource: filepath.Join(projectDir, "workspace"),
		Effect:   "allow",
	})

	ps.loadSavedPermissions(projectDir)
	return ps
}

func (ps *PermissionService) loadSavedPermissions(projectDir string) {
	savedPath := filepath.Join(projectDir, "permissions.json")

	data, err := os.ReadFile(savedPath)
	if err != nil {
		return
	}

	var saved map[string]PermissionDecision
	if err := json.Unmarshal(data, &saved); err != nil {
		return
	}

	ps.mu.Lock()
	ps.saved = saved
	ps.mu.Unlock()
}

func (ps *PermissionService) savePermissions(projectDir string) error {
	os.MkdirAll(projectDir, 0755)

	savedPath := filepath.Join(projectDir, "permissions.json")

	ps.mu.RLock()
	data, err := json.MarshalIndent(ps.saved, "", "  ")
	ps.mu.RUnlock()

	if err != nil {
		return err
	}

	return os.WriteFile(savedPath, data, 0644)
}

func (ps *PermissionService) evaluate(action, resource string) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	key := action + ":" + resource
	if effect, ok := ps.saved[key]; ok {
		return string(effect)
	}

	for _, rule := range ps.decisions["*"] {
		if wildcardMatch(action, rule.Action) && wildcardMatch(resource, rule.Resource) {
			return rule.Effect
		}
	}

	return "ask"
}

func wildcardMatch(value, pattern string) bool {
	if pattern == "*" {
		return true
	}

	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(value, prefix)
	}

	return value == pattern
}

func (ps *PermissionService) Assert(action, resource string) error {
	effect := ps.evaluate(action, resource)

	switch effect {
	case "allow":
		return nil
	case "deny":
		return &PermissionDeniedError{
			Action:   action,
			Resource: resource,
		}
	case "ask":
		return &PermissionRequiredError{
			Action:   action,
			Resource: resource,
		}
	default:
		return &PermissionDeniedError{
			Action:   action,
			Resource: resource,
		}
	}
}

func (ps *PermissionService) Ask(action, resource string) *PermissionRequest {
	return &PermissionRequest{
		ID:       generatePermissionID(),
		Action:   action,
		Resource: resource,
	}
}

func (ps *PermissionService) Reply(requestID string, decision PermissionDecision) error {
	if decision == DecisionAlways {
		return ps.savePermissions("")
	}
	return nil
}

func (ps *PermissionService) AddRule(rule PermissionRule) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.decisions["*"] == nil {
		ps.decisions["*"] = []PermissionRule{}
	}
	ps.decisions["*"] = append(ps.decisions["*"], rule)
}

type PermissionDeniedError struct {
	Action   string
	Resource string
}

func (e *PermissionDeniedError) Error() string {
	return "permission denied: " + e.Action + " on " + e.Resource
}

type PermissionRequiredError struct {
	Action   string
	Resource string
}

func (e *PermissionRequiredError) Error() string {
	return "permission required: " + e.Action + " on " + e.Resource
}

func generatePermissionID() string {
	return "per_0000000000000000"
}
