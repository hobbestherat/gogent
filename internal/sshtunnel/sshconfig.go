package sshtunnel

// A small, zero-dependency reader for ~/.ssh/config (issue #498). gogent's
// ssh:// transport previously built its ssh.ClientConfig purely from the
// --connect URL + CLI flags, so `gogent --connect ssh://rpi5` ignored a
// `Host rpi5` block and failed to authenticate even when `ssh rpi5` worked. This
// file resolves the directives gogent honors — HostName / User / Port /
// IdentityFile / IdentitiesOnly — the way OpenSSH does (first-value-wins, glob
// Host patterns, Include, ~ and %h/%r/%% expansion), with NO external
// dependency (a hand-rolled line parser, not github.com/kevinburke/ssh_config).
//
// Scope is deliberately narrow: ProxyJump/ProxyCommand/Match/HostKeyAlias/
// UserKnownHostsFile and password/keyboard-interactive auth are out of scope
// (separate follow-ups). The config is advisory: a missing file or a malformed
// line is never fatal — it yields Found=false / skips the line, mirroring
// OpenSSH's tolerance, so a broken config can never block a connect.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// includeDepthLimit bounds Include recursion so a cyclic `Include` cannot loop
// forever. OpenSSH uses a similar small cap.
const includeDepthLimit = 16

// ResolvedSSHConfig is the subset of ~/.ssh/config gogent applies to a dial. All
// fields are zero/empty when the corresponding directive is absent; Found
// reports whether any Host block matched the queried host at all (so the caller
// can say "ssh config found/applied for <host>" in diagnostics).
type ResolvedSSHConfig struct {
	HostName       string   // HostName directive: the real address to dial
	User           string   // User directive
	Port           int      // Port directive; 0 when absent or unparseable
	IdentityFiles  []string // IdentityFile directives, in order, ~/%h/%r-expanded
	IdentitiesOnly bool     // IdentitiesOnly yes/true
	Found          bool     // a Host block matched the queried host
}

// ReadSSHConfig resolves the directives gogent honors for host from the user's
// ~/.ssh/config (and, best-effort, the system /etc/ssh/ssh_config as a lower-
// priority fallback). It is advisory and never hard-fails on a missing OR
// unreadable file: such files are skipped, yielding ResolvedSSHConfig{Found:
// false} and a nil error. A non-nil error is possible only when a file that
// opened successfully fails mid-read (a scanner error); callers treat even that
// as non-fatal.
func ReadSSHConfig(host string) (ResolvedSSHConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// No home → no user config to read; the system file may still apply.
		home = ""
	}
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, ".ssh", "config"))
	}
	// System-wide config is consulted after the user file: first-value-wins means
	// the user file's values take precedence, matching OpenSSH ordering.
	paths = append(paths, filepath.Join("/etc", "ssh", "ssh_config"))
	return readSSHConfigFiles(paths, host, home)
}

// readSSHConfigFiles folds the named files (each best-effort: a missing file is
// skipped) into a single ResolvedSSHConfig using first-value-wins semantics.
func readSSHConfigFiles(paths []string, host, home string) (ResolvedSSHConfig, error) {
	acc := &sshConfigAccumulator{host: host, home: home}
	for _, p := range paths {
		if err := acc.parseFile(p, 0); err != nil {
			return ResolvedSSHConfig{}, err
		}
	}
	return acc.finish(), nil
}

// sshConfigAccumulator folds directives across one or more files. Single-valued
// directives keep their FIRST seen value (OpenSSH first-value-wins);
// IdentityFile accumulates every value. raw* fields hold pre-expansion strings;
// expansion to %h/%r/~ happens once in finish(), after HostName/User resolve.
type sshConfigAccumulator struct {
	host string // the queried host (alias), used for %h fallback and matching
	home string // user home dir for ~ expansion (may be "")

	hostName       string
	user           string
	port           int
	identityRaw    []string
	identitiesOnly bool

	haveHostName       bool
	haveUser           bool
	havePort           bool
	haveIdentitiesOnly bool

	matched bool // any Host block matched the queried host
}

// parseFile opens path (best-effort: a non-existent / unreadable file is
// skipped) and folds its directives. depth guards Include recursion.
func (a *sshConfigAccumulator) parseFile(path string, depth int) error {
	if depth > includeDepthLimit {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // path is a user/system ssh config file, not attacker-controlled
	if err != nil {
		// Advisory posture: a missing config — or one we cannot open (e.g.
		// permission denied) — is skipped, never fatal. A broken/unreadable
		// ssh-config must not block a connect.
		return nil
	}
	defer func() { _ = f.Close() }()
	return a.parseReader(f, filepath.Dir(path), depth)
}

// parseReader folds one config stream. baseDir is the directory of the enclosing
// file, used to resolve relative Include paths (OpenSSH-style).
func (a *sshConfigAccumulator) parseReader(r io.Reader, baseDir string, depth int) error {
	active := true // directives before the first Host line are global
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		key, val, ok := splitDirective(sc.Text())
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			active = matchHost(val, a.host)
			if active {
				a.matched = true
			}
		case "include":
			if active {
				a.parseIncludes(val, baseDir, depth)
			}
		case "hostname":
			if active && !a.haveHostName && val != "" {
				a.hostName, a.haveHostName = val, true
			}
		case "user":
			if active && !a.haveUser && val != "" {
				a.user, a.haveUser = val, true
			}
		case "port":
			if active && !a.havePort {
				if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 65535 {
					a.port, a.havePort = n, true
				}
				// A non-numeric / out-of-range Port is skipped, not propagated:
				// a broken line must not poison the resolve.
			}
		case "identityfile":
			if active && val != "" {
				a.identityRaw = append(a.identityRaw, val)
			}
		case "identitiesonly":
			if active && !a.haveIdentitiesOnly {
				a.identitiesOnly = parseBool(val)
				a.haveIdentitiesOnly = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan ssh config: %w", err)
	}
	return nil
}

// parseIncludes expands an Include value (which may list several whitespace-
// separated patterns, each a glob, possibly ~-rooted) and folds each matched
// file inline at this point so first-value-wins ordering is preserved.
func (a *sshConfigAccumulator) parseIncludes(val, baseDir string, depth int) {
	for _, pat := range strings.Fields(val) {
		pat = expandTilde(pat, a.home)
		if !filepath.IsAbs(pat) && baseDir != "" {
			pat = filepath.Join(baseDir, pat)
		}
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue
		}
		for _, m := range matches {
			_ = a.parseFile(m, depth+1)
		}
	}
}

// finish materialises the accumulated directives into a ResolvedSSHConfig,
// applying token/path expansion to IdentityFile entries now that HostName/User
// are known.
func (a *sshConfigAccumulator) finish() ResolvedSSHConfig {
	rc := ResolvedSSHConfig{
		// In HostName, %h is the original alias.
		HostName:       expandTokens(a.hostName, a.host, a.user),
		User:           a.user,
		Port:           a.port,
		IdentitiesOnly: a.identitiesOnly,
		Found:          a.matched,
	}
	// In IdentityFile, %h is the resolved HostName (fallback: the alias).
	hForKeys := rc.HostName
	if hForKeys == "" {
		hForKeys = a.host
	}
	for _, raw := range a.identityRaw {
		p := expandTokens(raw, hForKeys, a.user)
		p = expandTilde(p, a.home)
		rc.IdentityFiles = append(rc.IdentityFiles, p)
	}
	return rc
}

// --- line / token helpers --------------------------------------------------

// splitDirective parses one config line into a (keyword, value) pair. It strips
// comments (a line whose first non-space rune is '#') and blank lines (ok=false),
// accepts both `Key Value` and `Key=Value`, and unquotes a single layer of
// surrounding double quotes on the value.
func splitDirective(line string) (key, val string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || s[0] == '#' {
		return "", "", false
	}
	// Separate the keyword from the rest on the first run of separators
	// (whitespace and/or a single '='), matching OpenSSH's tolerant parsing.
	i := strings.IndexAny(s, " \t=")
	if i < 0 {
		return "", "", false // a bare keyword with no value is ignored
	}
	key = s[:i]
	rest := strings.TrimLeft(s[i:], " \t=")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", "", false
	}
	if len(rest) >= 2 && rest[0] == '"' {
		if j := strings.IndexByte(rest[1:], '"'); j >= 0 {
			return key, rest[1 : 1+j], true
		}
	}
	return key, rest, true
}

// matchHost reports whether any whitespace-separated pattern in patterns matches
// host. Patterns support OpenSSH globbing (* and ?); a leading '!' negates, and a
// matched negation disqualifies the whole line (returns false) as in OpenSSH.
func matchHost(patterns, host string) bool {
	var positive bool
	for _, pat := range strings.Fields(patterns) {
		neg := false
		if strings.HasPrefix(pat, "!") {
			neg, pat = true, pat[1:]
		}
		if globMatch(pat, host) {
			if neg {
				return false // an explicit negative match wins: block excluded
			}
			positive = true
		}
	}
	return positive
}

// globMatch implements OpenSSH Host-pattern matching: '*' matches any run
// (including empty), '?' matches exactly one character, everything else is
// literal. It is anchored (the whole host must match).
func globMatch(pattern, s string) bool {
	// Iterative backtracking matcher (handles multiple '*' without recursion).
	var (
		px, sx       int
		star, starSx = -1, 0
	)
	for sx < len(s) {
		if px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]) {
			px++
			sx++
			continue
		}
		if px < len(pattern) && pattern[px] == '*' {
			star = px
			starSx = sx
			px++
			continue
		}
		if star >= 0 {
			px = star + 1
			starSx++
			sx = starSx
			continue
		}
		return false
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

// parseBool reads an OpenSSH yes/no flag value; only "yes"/"true" enable it.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true":
		return true
	default:
		return false
	}
}

// expandTokens expands the OpenSSH percent tokens gogent needs: %h (host), %r
// (remote user), %% (literal %). The caller supplies the value %h resolves to
// (the alias for HostName, the resolved HostName for IdentityFile). Unknown
// tokens are left untouched.
func expandTokens(s, hostForH, user string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'h':
			b.WriteString(hostForH)
		case 'r':
			b.WriteString(user)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// expandTilde expands a leading ~ or ~/ to the home dir. A "~user" form is left
// untouched (out of scope); home=="" leaves the path unchanged.
func expandTilde(p, home string) string {
	if home == "" || p == "" || p[0] != '~' {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p // ~user — not expanded
}
