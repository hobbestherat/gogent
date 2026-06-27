package sshtunnel

// Hermetic unit tests for the hand-rolled ~/.ssh/config reader (issue #498).
// All tests drive the unexported parser/helpers directly with explicit file
// paths / fixed strings, so they never touch the developer's real ~/.ssh/config
// or /etc/ssh/ssh_config (which on stock Debian/Ubuntu carries `Host *` and
// would otherwise pollute results on every machine).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes content to path (creating the parent dir) and fails the
// test on any I/O error. Used to materialise hermetic config files.
func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// resolve is a thin wrapper over readSSHConfigFiles for a single temp config
// file, with a deterministic home for ~ expansion.
func resolve(t *testing.T, content, host string) ResolvedSSHConfig {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	writeConfig(t, p, content)
	rc, err := readSSHConfigFiles([]string{p}, host, "/home/testuser")
	if err != nil {
		t.Fatalf("readSSHConfigFiles: %v", err)
	}
	return rc
}

// --------------------------------------------------------------------------------
// Resolution of the honored directives
// --------------------------------------------------------------------------------

func TestReadSSHConfig_BasicResolution(t *testing.T) {
	rc := resolve(t, strings.Join([]string{
		"Host rpi5",
		"    HostName 192.168.1.5",
		"    User pi",
		"    Port 2222",
		"    IdentityFile ~/.ssh/rpi5_key",
		"    IdentitiesOnly yes",
	}, "\n"), "rpi5")

	if !rc.Found {
		t.Fatalf("Found = false, want true (Host rpi5 matched)")
	}
	if rc.HostName != "192.168.1.5" {
		t.Errorf("HostName = %q, want 192.168.1.5", rc.HostName)
	}
	if rc.User != "pi" {
		t.Errorf("User = %q, want pi", rc.User)
	}
	if rc.Port != 2222 {
		t.Errorf("Port = %d, want 2222", rc.Port)
	}
	if !rc.IdentitiesOnly {
		t.Errorf("IdentitiesOnly = false, want true")
	}
	wantKey := filepath.Join("/home/testuser", ".ssh", "rpi5_key")
	if len(rc.IdentityFiles) != 1 || rc.IdentityFiles[0] != wantKey {
		t.Errorf("IdentityFiles = %v, want [%s]", rc.IdentityFiles, wantKey)
	}
}

func TestReadSSHConfig_NoMatch(t *testing.T) {
	rc := resolve(t, "Host rpi5\n    User pi\n", "other")
	if rc.Found {
		t.Fatalf("Found = true, want false for a non-matching host")
	}
	if rc.User != "" || rc.HostName != "" || rc.Port != 0 || len(rc.IdentityFiles) != 0 {
		t.Errorf("non-matching host should resolve empty, got %+v", rc)
	}
}

func TestReadSSHConfig_MissingFileIsNotFatal(t *testing.T) {
	// A path that does not exist yields Found=false and a nil error — the config
	// is advisory and a missing file must never block a connect.
	rc, err := readSSHConfigFiles([]string{filepath.Join(t.TempDir(), "does-not-exist")}, "rpi5", "/h")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if rc.Found {
		t.Fatalf("Found = true on a missing file, want false")
	}
}

// TestReadSSHConfig_GlobalOnlyYieldsFoundFalse documents a deliberate,
// OpenSSH-divergent behaviour: directives that appear BEFORE the first `Host`
// line are parsed while active, but do not set Found. A config with no Host
// block at all is therefore treated as "no match" by ParseConnectURL (which
// gates application on rc.Found) — OpenSSH would apply such globals to every
// host. Flagged in the test report; asserted here to lock the current contract.
func TestReadSSHConfig_GlobalOnlyYieldsFoundFalse(t *testing.T) {
	rc := resolve(t, strings.Join([]string{
		"User globaluser",
		"HostName 1.2.3.4",
		"Port 2222",
	}, "\n"), "anything")
	if rc.Found {
		t.Fatalf("Found = true for a global-only config, want false (current contract: globals alone don't count as a match)")
	}
	// The values ARE parsed internally; only Found gating suppresses application.
	if rc.User != "globaluser" {
		t.Errorf("internal User = %q, want globaluser", rc.User)
	}
}

// --------------------------------------------------------------------------------
// Host-pattern matching (glob, multi-pattern, negation)
// --------------------------------------------------------------------------------

func TestReadSSHConfig_GlobPatterns(t *testing.T) {
	content := "Host rpi*\n    HostName 10.0.0.1\n    User wildcard\n"
	for _, tc := range []struct {
		host      string
		wantFound bool
	}{
		{"rpi5", true},
		{"rpi", true}, // '*' matches empty
		{"rpi5123", true},
		{"other", false},
		{"xrpi5", false}, // anchored: no leading match
	} {
		rc := resolve(t, content, tc.host)
		if rc.Found != tc.wantFound {
			t.Errorf("host %q: Found = %v, want %v", tc.host, rc.Found, tc.wantFound)
		}
	}

	// '?' matches exactly one character.
	q := resolve(t, "Host rp?5\n    User q\n", "rpi5")
	if !q.Found || q.User != "q" {
		t.Fatalf("'?' should match one char: %+v", q)
	}
	q2 := resolve(t, "Host rp?5\n    User q\n", "rp5")
	if q2.Found {
		t.Fatalf("'?' must match exactly one char; 'rp5' should not match 'rp?5'")
	}

	// Multiple patterns on one Host line: any match wins.
	multi := resolve(t, "Host rpi4 rpi5 rpi6\n    User multi\n", "rpi5")
	if !multi.Found || multi.User != "multi" {
		t.Fatalf("multi-pattern Host should match rpi5: %+v", multi)
	}
}

func TestReadSSHConfig_Negation(t *testing.T) {
	content := strings.Join([]string{
		"Host rpi* !rpi5",
		"    User wildcard",
		"Host rpi5",
		"    User specific",
	}, "\n")
	if got := resolve(t, content, "rpi5").User; got != "specific" {
		t.Errorf("rpi5 should be excluded from rpi* !rpi5 then matched specifically; User = %q, want specific", got)
	}
	if got := resolve(t, content, "rpi4").User; got != "wildcard" {
		t.Errorf("rpi4 should match rpi* (negation does not apply); User = %q, want wildcard", got)
	}
}

// --------------------------------------------------------------------------------
// First-value-wins (single-valued) vs accumulate (IdentityFile)
// --------------------------------------------------------------------------------

func TestReadSSHConfig_FirstValueWinsAcrossBlocks(t *testing.T) {
	content := strings.Join([]string{
		"Host rpi*",
		"    User wildcard",
		"    Port 2222",
		"Host rpi5",
		"    User specific",
		"    Port 3333",
	}, "\n")
	rc := resolve(t, content, "rpi5")
	if rc.User != "wildcard" {
		t.Errorf("User = %q, want first-value-wins 'wildcard'", rc.User)
	}
	if rc.Port != 2222 {
		t.Errorf("Port = %d, want first-value-wins 2222", rc.Port)
	}
}

func TestReadSSHConfig_GlobalBeforeHostWins(t *testing.T) {
	// A global (pre-Host) value is seen first, so first-value-wins keeps it over a
	// later Host-specific value — matching OpenSSH's top-level-is-implicit-Host-*
	// semantics.
	content := strings.Join([]string{
		"User globaluser",
		"Host rpi5",
		"    User pi",
	}, "\n")
	rc := resolve(t, content, "rpi5")
	if rc.User != "globaluser" {
		t.Errorf("User = %q, want globaluser (global seen first wins)", rc.User)
	}
}

func TestReadSSHConfig_IdentityFileAccumulatesAcrossBlocks(t *testing.T) {
	content := strings.Join([]string{
		"Host rpi*",
		"    IdentityFile ~/.ssh/wild",
		"Host rpi5",
		"    IdentityFile ~/.ssh/specific",
	}, "\n")
	rc := resolve(t, content, "rpi5")
	want := []string{
		filepath.Join("/home/testuser", ".ssh", "wild"),
		filepath.Join("/home/testuser", ".ssh", "specific"),
	}
	if len(rc.IdentityFiles) != len(want) {
		t.Fatalf("IdentityFiles = %v, want %v", rc.IdentityFiles, want)
	}
	for i := range want {
		if rc.IdentityFiles[i] != want[i] {
			t.Errorf("IdentityFiles[%d] = %q, want %q", i, rc.IdentityFiles[i], want[i])
		}
	}
}

// --------------------------------------------------------------------------------
// Token / path expansion
// --------------------------------------------------------------------------------

func TestReadSSHConfig_IdentityFilePercentExpansion(t *testing.T) {
	// In IdentityFile, %h is the RESOLVED HostName and %r is the resolved User.
	content := strings.Join([]string{
		"Host rpi5",
		"    HostName 192.168.1.5",
		"    User pi",
		"    IdentityFile ~/.ssh/%h/%r_key",
	}, "\n")
	rc := resolve(t, content, "rpi5")
	want := filepath.Join("/home/testuser", ".ssh", "192.168.1.5", "pi_key")
	if len(rc.IdentityFiles) != 1 || rc.IdentityFiles[0] != want {
		t.Errorf("IdentityFiles = %v, want [%s]", rc.IdentityFiles, want)
	}
}

func TestReadSSHConfig_HostNamePercentHIsAlias(t *testing.T) {
	// In HostName, %h is the ORIGINAL alias (not the resolved HostName).
	content := "Host rpi5\n    HostName gw-%h.example.com\n"
	rc := resolve(t, content, "rpi5")
	if rc.HostName != "gw-rpi5.example.com" {
		t.Errorf("HostName = %q, want gw-rpi5.example.com", rc.HostName)
	}
}

func TestReadSSHConfig_PercentPercentIsLiteral(t *testing.T) {
	content := "Host rpi5\n    IdentityFile /keys/100%%pct\n"
	rc := resolve(t, content, "rpi5")
	if len(rc.IdentityFiles) != 1 || rc.IdentityFiles[0] != "/keys/100%pct" {
		t.Errorf("IdentityFiles = %v, want [/keys/100%%pct]", rc.IdentityFiles)
	}
}

// --------------------------------------------------------------------------------
// Include
// --------------------------------------------------------------------------------

func TestReadSSHConfig_IncludeRelativeGlob(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "config")
	writeConfig(t, main, strings.Join([]string{
		"Include conf.d/*.conf",
		"Host rpi5",
		"    User pi",
	}, "\n"))
	writeConfig(t, filepath.Join(dir, "conf.d", "rpi.conf"), strings.Join([]string{
		"Host rpi5",
		"    HostName 192.168.1.5",
		"    IdentityFile ~/.ssh/rpi5_key",
	}, "\n"))

	rc, err := readSSHConfigFiles([]string{main}, "rpi5", "/home/testuser")
	if err != nil {
		t.Fatalf("readSSHConfigFiles: %v", err)
	}
	if rc.HostName != "192.168.1.5" {
		t.Errorf("Include should bring in HostName; got %q", rc.HostName)
	}
	if rc.User != "pi" {
		t.Errorf("User = %q, want pi (from main, after include)", rc.User)
	}
	wantKey := filepath.Join("/home/testuser", ".ssh", "rpi5_key")
	if len(rc.IdentityFiles) != 1 || rc.IdentityFiles[0] != wantKey {
		t.Errorf("IdentityFiles = %v, want [%s]", rc.IdentityFiles, wantKey)
	}
}

// TestReadSSHConfig_CyclicIncludeTerminates guards against an Include cycle
// looping forever; the depth cap must bound it. If this test hangs, the guard is
// broken.
func TestReadSSHConfig_CyclicIncludeTerminates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.conf")
	b := filepath.Join(dir, "b.conf")
	writeConfig(t, a, "Include b.conf\nHost rpi5\n    User froma\n")
	writeConfig(t, b, "Include a.conf\nHost rpi5\n    HostName 1.2.3.4\n")

	done := make(chan struct{})
	var rc ResolvedSSHConfig
	var err error
	go func() {
		defer close(done)
		rc, err = readSSHConfigFiles([]string{a}, "rpi5", "/home/testuser")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cyclic Include did not terminate within 5s — depth cap broken")
	}
	if err != nil {
		t.Fatalf("cyclic include errored: %v", err)
	}
	// The cycle resolves Host rpi5 at some point; Found must be true.
	if !rc.Found {
		t.Fatalf("Found = false, want true (Host rpi5 should still match)")
	}
}

// --------------------------------------------------------------------------------
// Malformed values are skipped, not fatal (advisory-config posture)
// --------------------------------------------------------------------------------

func TestReadSSHConfig_MalformedPortSkipped(t *testing.T) {
	rc := resolve(t, strings.Join([]string{
		"Host rpi5",
		"    Port abc",   // non-numeric → skipped
		"    Port 99999", // out of range → skipped
		"    Port 0",     // non-positive → skipped
		"    Port 2222",  // valid → kept
		"    User pi",
	}, "\n"), "rpi5")
	if rc.Port != 2222 {
		t.Errorf("Port = %d, want 2222 (the one valid value; malformed ones skipped)", rc.Port)
	}
	if rc.User != "pi" {
		t.Errorf("a broken Port line must not abort parsing; User = %q, want pi", rc.User)
	}
}

func TestReadSSHConfig_AllMalformedPortLeavesZero(t *testing.T) {
	rc := resolve(t, "Host rpi5\n    Port nope\n    User pi\n", "rpi5")
	if rc.Port != 0 {
		t.Errorf("Port = %d, want 0 when every Port line is malformed", rc.Port)
	}
}

func TestReadSSHConfig_IdentitiesOnlyBoolValues(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"yes", true}, {"YES", true}, {"Yes", true}, {"true", true},
		{"no", false}, {"false", false}, {"", false}, {"garbage", false},
	} {
		rc := resolve(t, "Host rpi5\n    IdentitiesOnly "+tc.val+"\n", "rpi5")
		if rc.IdentitiesOnly != tc.want {
			t.Errorf("IdentitiesOnly %q = %v, want %v", tc.val, rc.IdentitiesOnly, tc.want)
		}
	}
}

// --------------------------------------------------------------------------------
// Lexical tolerance: case-insensitivity, Key=Value, quotes, comments
// --------------------------------------------------------------------------------

func TestReadSSHConfig_CaseInsensitiveKeys(t *testing.T) {
	rc := resolve(t, strings.Join([]string{
		"host rpi5",
		"    hostname 192.168.1.5",
		"    USER pi",
		"    PORT 2222",
		"    IDENTITIESONLY yes",
	}, "\n"), "rpi5")
	if rc.HostName != "192.168.1.5" || rc.User != "pi" || rc.Port != 2222 || !rc.IdentitiesOnly {
		t.Errorf("case-insensitive keys not honored: %+v", rc)
	}
}

func TestReadSSHConfig_KeyEqualsValueForm(t *testing.T) {
	rc := resolve(t, strings.Join([]string{
		"Host rpi5",
		"    HostName=192.168.1.5",
		"    User=pi",
		"    Port=2222",
		"    IdentitiesOnly=yes",
	}, "\n"), "rpi5")
	if rc.HostName != "192.168.1.5" || rc.User != "pi" || rc.Port != 2222 || !rc.IdentitiesOnly {
		t.Errorf("Key=Value form not parsed: %+v", rc)
	}
}

func TestReadSSHConfig_QuotedValueWithSpaces(t *testing.T) {
	rc := resolve(t, "Host rpi5\n    IdentityFile \"/path with spaces/key\"\n", "rpi5")
	if len(rc.IdentityFiles) != 1 || rc.IdentityFiles[0] != "/path with spaces/key" {
		t.Errorf("quoted value = %v, want [/path with spaces/key]", rc.IdentityFiles)
	}
}

func TestReadSSHConfig_CommentsAndBlanksIgnored(t *testing.T) {
	rc := resolve(t, strings.Join([]string{
		"# a leading comment",
		"",
		"   ",
		"Host rpi5",
		"    # not a block separator; inline text is part of no directive here",
		"    User pi",
		"",
		"# tail comment",
	}, "\n"), "rpi5")
	if !rc.Found || rc.User != "pi" {
		t.Errorf("comments/blanks should not affect parsing: %+v", rc)
	}
}

// --------------------------------------------------------------------------------
// Pure helper unit tests
// --------------------------------------------------------------------------------

func TestSplitDirective(t *testing.T) {
	for _, tc := range []struct {
		line    string
		wantKey string
		wantVal string
		wantOK  bool
	}{
		{"HostName 192.168.1.5", "HostName", "192.168.1.5", true},
		{"HostName=192.168.1.5", "HostName", "192.168.1.5", true},
		{"   HostName    192.168.1.5   ", "HostName", "192.168.1.5", true},
		{"HostName = 192.168.1.5", "HostName", "192.168.1.5", true},
		{"Host = rpi5", "Host", "rpi5", true},
		{"# comment", "", "", false},
		{"   # indented comment", "", "", false},
		{"", "", "", false},
		{"   ", "", "", false},
		{"HostName", "", "", false}, // bare keyword, no value → ignored
		{"IdentityFile \"/a b\"", "IdentityFile", "/a b", true},
	} {
		k, v, ok := splitDirective(tc.line)
		if k != tc.wantKey || v != tc.wantVal || ok != tc.wantOK {
			t.Errorf("splitDirective(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.line, k, v, ok, tc.wantKey, tc.wantVal, tc.wantOK)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pat, s string
		want   bool
	}{
		{"rpi5", "rpi5", true},
		{"rpi5", "rpi4", false},
		{"*", "anything", true},
		{"*", "", true},
		{"rpi*", "rpi5", true},
		{"rpi*", "rpi", true},
		{"rpi*", "xrpi5", false},
		{"rp?5", "rpi5", true},
		{"rp?5", "rp5", false},
		{"*5", "rpi5", true},
		{"r*i*5", "rpiX5", true},
		{"a*c", "abc", true},
		{"a*c", "abxc", true},
		{"a*c", "abx", false},
	} {
		if got := globMatch(tc.pat, tc.s); got != tc.want {
			t.Errorf("globMatch(%q,%q) = %v, want %v", tc.pat, tc.s, got, tc.want)
		}
	}
}

func TestMatchHost(t *testing.T) {
	for _, tc := range []struct {
		patterns, host string
		want           bool
	}{
		{"rpi5", "rpi5", true},
		{"rpi4 rpi5", "rpi5", true},
		{"rpi4 rpi5", "rpi6", false},
		{"* !bad", "bad", false}, // negation disqualifies
		{"* !bad", "good", true},
		{"!bad *", "bad", false}, // negation first still disqualifies
		{"", "rpi5", false},      // no patterns
	} {
		if got := matchHost(tc.patterns, tc.host); got != tc.want {
			t.Errorf("matchHost(%q,%q) = %v, want %v", tc.patterns, tc.host, got, tc.want)
		}
	}
}

func TestExpandTokens(t *testing.T) {
	for _, tc := range []struct {
		in         string
		host, user string
		want       string
	}{
		{"a%hb", "HOST", "u", "aHOSTb"},
		{"a%rb", "h", "USER", "aUSERb"},
		{"100%%", "h", "u", "100%"},
		{"%h-%r-%%", "H", "U", "H-U-%"},
		{"no-tokens", "h", "u", "no-tokens"},
		{"%x", "h", "u", "%x"}, // unknown token left intact
		{"trailing%", "h", "u", "trailing%"},
	} {
		if got := expandTokens(tc.in, tc.host, tc.user); got != tc.want {
			t.Errorf("expandTokens(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExpandTilde(t *testing.T) {
	for _, tc := range []struct {
		p, home, want string
	}{
		{"~", "/home/x", "/home/x"},
		{"~/dir", "/home/x", filepath.Join("/home/x", "dir")},
		{"~user/dir", "/home/x", "~user/dir"}, // ~user not expanded (out of scope)
		{"/abs/path", "/home/x", "/abs/path"},
		{"rel", "/home/x", "rel"},
		{"", "/home/x", ""},
		{"~", "", "~"}, // empty home → unchanged
	} {
		if got := expandTilde(tc.p, tc.home); got != tc.want {
			t.Errorf("expandTilde(%q,%q) = %q, want %q", tc.p, tc.home, got, tc.want)
		}
	}
}

func TestParseBool(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"yes", true}, {"YES", true}, {"Yes", true}, {"  yes  ", true},
		{"true", true}, {"TRUE", true},
		{"no", false}, {"false", false}, {"", false}, {"1", false}, {"garbage", false},
	} {
		if got := parseBool(tc.in); got != tc.want {
			t.Errorf("parseBool(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
