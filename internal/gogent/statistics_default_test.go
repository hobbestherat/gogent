package gogent

import "testing"

// TestStatistics_IncludesDefaultSessionForStats pins the issue #278 backend
// constraint: the fix for the phantom "default" session must NOT be applied inside
// Gogent.Statistics(), because Statistics() also backs the headless GET /stats
// endpoint (internal/server/resources.go systemSvc.Stats), where "default" is the
// real shared HTTP/API session the caller talks to. The phantom is filtered on the
// TUI side only (ui/tui: filterPhantomSessions); the backend report must still count
// and list "default" exactly like any other session.
//
// cmd/main.go unconditionally creates this session at startup:
//
//	_ = g.CreateUserSession("default", rootAgent)
//
// so this test models that real shape: a "default" session plus a user session.
func TestStatistics_IncludesDefaultSessionForStats(t *testing.T) {
	g := NewGogent("/tmp/test-stats-default")

	def := makeStatsSession(g, "default")
	def.SetPrimaryModel("opus")
	def.AddTokenUsage(400, 80) // headless HTTP traffic on the shared default session
	def.IncrementToolCall()

	usr := makeStatsSession(g, "user-1")
	usr.SetPrimaryModel("haiku")
	usr.AddTokenUsage(100, 20)

	rep := g.Statistics()

	// The grand totals must count BOTH sessions — the /stats consumer sees default.
	if rep.Totals.Sessions != 2 {
		t.Errorf("Totals.Sessions = %d, want 2 (Statistics must NOT drop default; /stats relies on it)", rep.Totals.Sessions)
	}
	if rep.Totals.TokensIn != 500 || rep.Totals.TokensOut != 100 {
		t.Errorf("Totals tokens = %d/%d, want 500/100 (default's 400/80 must be included)",
			rep.Totals.TokensIn, rep.Totals.TokensOut)
	}

	// The default session must appear as a per-session row, carrying its own traffic.
	var defRow bool
	for _, s := range rep.Sessions {
		if s.ID == "default" {
			defRow = true
			if s.TokensIn != 400 || s.TokensOut != 80 {
				t.Errorf("default row tokens = %d/%d, want 400/80", s.TokensIn, s.TokensOut)
			}
			if s.ToolCalls != 1 {
				t.Errorf("default row ToolCalls = %d, want 1", s.ToolCalls)
			}
		}
	}
	if !defRow {
		t.Errorf("default session missing from Statistics().Sessions; /stats would lose its real session: %+v", rep.Sessions)
	}

	// And its model attribution survives in the per-model breakdown.
	var opusSeen bool
	for _, m := range rep.Models {
		if m.Name == "opus" {
			opusSeen = true
			if m.TokensIn != 400 {
				t.Errorf("opus model TokensIn = %d, want 400 (default's attribution)", m.TokensIn)
			}
		}
	}
	if !opusSeen {
		t.Errorf("default session's model 'opus' missing from per-model breakdown: %+v", rep.Models)
	}
}
