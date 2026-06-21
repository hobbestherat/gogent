package gogent

import (
	"testing"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
)

// TestCreateUserSessionWiresStreamThinking verifies the config flag
// Experimental.StreamThinking is applied to each new user session (issue #217):
// on when the flag is set, off otherwise (the default).
func TestCreateUserSessionWiresStreamThinking(t *testing.T) {
	newSession := func(g *Gogent, id string) *agent.UserSession {
		m := model.NewModelConnection()
		s := model.NewModelSession(id, m)
		ag := agent.NewAgent(id, s)
		return g.CreateUserSession(id, ag)
	}

	// Flag on → session streaming enabled.
	g := NewGogent(t.TempDir())
	if g.config == nil {
		g.config = &config.Config{}
	}
	g.config.Experimental.StreamThinking = true
	if us := newSession(g, "on"); !us.StreamThinking() {
		t.Error("CreateUserSession must propagate Experimental.StreamThinking=true")
	}

	// Flag off (default) → session streaming disabled.
	g2 := NewGogent(t.TempDir())
	if g2.config == nil {
		g2.config = &config.Config{}
	}
	g2.config.Experimental.StreamThinking = false
	if us := newSession(g2, "off"); us.StreamThinking() {
		t.Error("CreateUserSession must leave streaming off when the flag is off")
	}
}
