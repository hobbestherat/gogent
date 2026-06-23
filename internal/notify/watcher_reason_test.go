package notify

import (
	"strings"
	"testing"

	"gogent/internal/config"
)

// TestReasonWatcherConstant locks in the wire token used by the watcher manager
// when it asks the host to notify (host passes "watcher").
func TestReasonWatcherConstant(t *testing.T) {
	if ReasonWatcher != "watcher" {
		t.Errorf("ReasonWatcher = %q, want \"watcher\"", ReasonWatcher)
	}
}

// TestDefaultConfigEnablesWatcher confirms watcher completion notifications are
// on by default, mirroring OnComplete/OnClarify.
func TestDefaultConfigEnablesWatcher(t *testing.T) {
	if !config.DefaultNotifyConfig().OnWatcher {
		t.Error("DefaultNotifyConfig().OnWatcher should default to true")
	}
}

// TestShouldNotifyWatcher exercises the ReasonWatcher branch of the per-event
// gate: it tracks the OnWatcher toggle and the master switch, and is independent
// of the other reasons' toggles.
func TestShouldNotifyWatcher(t *testing.T) {
	all := config.DefaultNotifyConfig()

	watcherOff := all
	watcherOff.OnWatcher = false

	masterOff := all
	masterOff.Enabled = false

	// Turning a *different* reason off must not affect watcher gating.
	completeOff := all
	completeOff.OnComplete = false

	for _, tc := range []struct {
		name string
		cfg  config.NotifyConfig
		want bool
	}{
		{"default on", all, true},
		{"watcher toggle off", watcherOff, false},
		{"master off", masterOff, false},
		{"unrelated toggle off", completeOff, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := New(tc.cfg, &strings.Builder{})
			if got := n.ShouldNotify(ReasonWatcher, false); got != tc.want {
				t.Errorf("ShouldNotify(ReasonWatcher)=%v, want %v", got, tc.want)
			}
			if got := n.ReasonEnabled(ReasonWatcher); got != tc.want {
				t.Errorf("ReasonEnabled(ReasonWatcher)=%v, want %v", got, tc.want)
			}
		})
	}
}
