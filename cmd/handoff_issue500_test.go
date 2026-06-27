package main

import (
	"strings"
	"testing"

	tuipkg "gogent/ui/tui"
)

// Tests for issue #500's cmd-side seam: remoteTargetLabel renders a --connect address
// as the terse target shown in the menu-bar status indicator, and
// daemonController.Label surfaces it (cheaply, synchronously) only in attached-remote
// mode. All SSH/tcp parsing stays here in cmd/ — ui/tui never imports
// internal/sshtunnel/daemon/server.

func TestRemoteTargetLabel(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"ssh with user and port drops port", "ssh://alice@example.com:2222", "ssh:alice@example.com"},
		{"ssh with user no port", "ssh://alice@example.com", "ssh:alice@example.com"},
		{"ssh host only no userinfo", "ssh://example.com", "ssh:example.com"},
		{"ssh default port 22 dropped", "ssh://alice@example.com:22", "ssh:alice@example.com"},
		{"ssh ipv4 host", "ssh://alice@10.0.0.5:2222", "ssh:alice@10.0.0.5"},
		{"http tcp host port", "http://example.com:8080", "example.com:8080"},
		{"https tcp host port", "https://example.com:8443", "example.com:8443"},
		{"schemeless host port falls back raw", "example.com:8080", "example.com:8080"},
		{"empty address", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remoteTargetLabel(c.addr); got != c.want {
				t.Errorf("remoteTargetLabel(%q) = %q, want %q", c.addr, got, c.want)
			}
		})
	}
}

// TestRemoteTargetLabelTerse verifies the indicator label stays short enough for
// turbotui to keep on a narrow bar (gate 1 / interactions): an ssh target never
// includes the scheme prefix (ssh://) or the SSH port, only "ssh:user@host".
func TestRemoteTargetLabelTerse(t *testing.T) {
	got := remoteTargetLabel("ssh://alice@example.com:2222")
	if strings.Contains(got, "://") {
		t.Errorf("label %q still contains a scheme separator", got)
	}
	if strings.Contains(got, ":2222") || strings.HasSuffix(got, ":22") {
		t.Errorf("label %q still contains the SSH port", got)
	}
	if !strings.HasPrefix(got, "ssh:") {
		t.Errorf("label %q lost the ssh: marker", got)
	}
}

// TestDaemonControllerLabelByMode covers the mode-aware seam: Label returns "" for
// embedded and attached-local (the indicator derives those from the mode alone) and
// the parsed target only for attached-remote. It touches only dc.mu/mode/connect, so a
// minimal zero-value controller exercises it without the full attach plumbing.
func TestDaemonControllerLabelByMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    tuipkg.DaemonMode
		connect string
		want    string
	}{
		{"embedded empty connect", tuipkg.DaemonModeEmbedded, "", ""},
		{"attached-local unix socket is blank", tuipkg.DaemonModeAttachedLocal, "unix:///tmp/gogent.sock", ""},
		{"attached-remote ssh", tuipkg.DaemonModeAttachedRemote, "ssh://alice@example.com:2222", "ssh:alice@example.com"},
		{"attached-remote tcp", tuipkg.DaemonModeAttachedRemote, "example.com:8080", "example.com:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dc := &daemonController{mode: c.mode, connect: c.connect}
			if got := dc.Label(); got != c.want {
				t.Errorf("Label() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDaemonControllerLabelIsSynchronousAndCheap is a light contract check: Label
// must not require the daemon client/socket to be live (it is a field read), so it is
// safe to call on the UI thread on every rebuild/resize — unlike DaemonStatusInfo.
func TestDaemonControllerLabelIsSynchronousAndCheap(t *testing.T) {
	// A controller with no client/socket wired still resolves a label.
	dc := &daemonController{
		mode:    tuipkg.DaemonModeAttachedRemote,
		connect: "ssh://alice@example.com",
	}
	if got := dc.Label(); got != "ssh:alice@example.com" {
		t.Fatalf("Label() with no client wired = %q, want ssh:alice@example.com", got)
	}
}
