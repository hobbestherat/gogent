package main

import (
	"testing"

	"gogent/internal/gogent"
)

func TestIssue358HostLabelDistinguishesLocalAndRemoteDaemon(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{"unix:///tmp/gogent/daemon.sock", "the local daemon"},
		{"/tmp/gogent/daemon.sock", "the local daemon"},
		{"http://127.0.0.1:8080", "127.0.0.1:8080"},
		{"https://daemon.example.test:8443/path", "daemon.example.test:8443"},
		{"ssh://host.example.test", "host.example.test"},
	}
	for _, tc := range tests {
		if got := hostLabel(tc.addr); got != tc.want {
			t.Errorf("hostLabel(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestIssue358FormatUptimeIsCompactAndStable(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{-1, "0s"},
		{0, "0s"},
		{1, "1s"},
		{62, "1m2s"},
		{3723, "1h2m3s"},
	}
	for _, tc := range tests {
		if got := formatUptime(tc.seconds); got != tc.want {
			t.Errorf("formatUptime(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestIssue358LiveUserSessionCountExcludesBackendOnlySessions(t *testing.T) {
	g := gogent.NewGogent(t.TempDir())
	g.NewSession("default")
	g.NewSession("user-a")
	g.NewSession("watcher:daily")
	g.NewSession("user-b")

	if got := liveUserSessionCount(g); got != 2 {
		t.Fatalf("liveUserSessionCount = %d, want 2 user-facing sessions", got)
	}
}
