package main

import "testing"

func TestResolveModeAttachSelection(t *testing.T) {
	cases := []struct {
		name       string
		embedded   bool
		connect    string
		probe      func() bool
		wantAttach bool
		wantAddr   string
	}{
		{
			name:       "embedded flag forces embedded even with explicit connect",
			embedded:   true,
			connect:    "http://daemon:8080",
			probe:      func() bool { return true },
			wantAttach: false,
		},
		{
			name:       "explicit connect wins without probing local socket",
			connect:    "http://daemon:8080",
			probe:      func() bool { panic("probe must not run for explicit connect") },
			wantAttach: true,
			wantAddr:   "http://daemon:8080",
		},
		{
			name:       "default attaches to live local socket",
			probe:      func() bool { return true },
			wantAttach: true,
			wantAddr:   "unix:///tmp/gogent.sock",
		},
		{
			name:       "default falls back to embedded when local socket absent",
			probe:      func() bool { return false },
			wantAttach: false,
		},
		{
			name:       "nil probe falls back to embedded",
			probe:      nil,
			wantAttach: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAttach, gotAddr := resolveMode(tc.embedded, tc.connect, "unix:///tmp/gogent.sock", tc.probe)
			if gotAttach != tc.wantAttach || gotAddr != tc.wantAddr {
				t.Fatalf("resolveMode = (%v, %q), want (%v, %q)", gotAttach, gotAddr, tc.wantAttach, tc.wantAddr)
			}
		})
	}
}

func TestResolveConnectTokenPrecedence(t *testing.T) {
	t.Setenv("GOGENT_HTTP_TOKEN", "from-env")
	if got := resolveConnectToken("from-flag"); got != "from-flag" {
		t.Fatalf("flag token = %q, want from-flag", got)
	}
	if got := resolveConnectToken(""); got != "from-env" {
		t.Fatalf("env token = %q, want from-env", got)
	}

	t.Setenv("GOGENT_HTTP_TOKEN", "")
	if got := resolveConnectToken(""); got != "" {
		t.Fatalf("empty token = %q, want empty", got)
	}
}
