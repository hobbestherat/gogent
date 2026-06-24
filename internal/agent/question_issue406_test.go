package agent

import "testing"

func TestIssue406PlanModeKeepsAskUser(t *testing.T) {
	for _, name := range planKeptTools {
		if name == "ask_user" {
			return
		}
	}
	t.Fatalf("planKeptTools = %v, want ask_user retained in plan mode", planKeptTools)
}
