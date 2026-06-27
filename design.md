# Design — Issue #498: honor `~/.ssh/config` in gogent's native ssh:// transport

## Problem (recap)

`gogent --connect ssh://rpi5` builds its `ssh.ClientConfig` purely from the
`--connect` URL + CLI flags + hard-coded defaults and never reads
`~/.ssh/config`. So it dials the *alias* literally, authenticates as the local
OS user, and offers only `~/.ssh/id_*` — even when `ssh rpi5` works because
`~/.ssh/config` maps `rpi5` → `HostName 192.168.1.5`, `User pi`,
`IdentityFile ~/.ssh/rpi5_key`. The result is the misleading
`ssh: unable to authenticate, attempted methods [none publickey]`.

**Goal:** principle of least surprise — if `ssh <host>` works via a `Host`
block, `gogent --connect ssh://<host>` works too with no extra flags, honoring
at minimum `HostName` / `User` / `Port` / `IdentityFile` / `IdentitiesOnly`,
with explicit URL fields and CLI flags overriding config (OpenSSH `-o` vs config
precedence).

## Scope / constraints

- **gogent only.** turbotui is not touched (it only consumes the injected
  `DialContext`); no `go.mod` change; no new external dependency. Reuse the
  `golang.org/x/crypto/ssh` already in `go.mod`. Hand-rolled, zero-dependency
  ssh_config parser (option (a) — **not** `kevinburke/ssh_config`).
- **Out of scope** (explicit non-goals, separate follow-ups): ProxyJump /
  ProxyCommand / Match blocks / HostKeyAlias / UserKnownHostsFile; password /
  keyboard-interactive auth; daemon-side changes. Host-key verification
  (`hostKeyCallback`) is left unchanged.

---

## Files touched (gogent)

| File | Change |
|---|---|
| `internal/sshtunnel/sshconfig.go` | **NEW.** Hand-rolled ssh_config parser: `ResolvedSSHConfig` + `ReadSSHConfig(host)` (+ unexported helpers for testability). |
| `internal/sshtunnel/tunnel.go` | `Config`: add `IdentityFiles []string`, `IdentitiesOnly bool`, `Alias string`, `ConfigFound bool`. `ParseConnectURL`: call `ReadSSHConfig` and fill *missing* fields. `keyPaths`: merge `IdentityFiles`, honor `IdentitiesOnly`. `dialClient`: enrich the auth-failure error. |
| `cmd/attach.go` | Use the original alias (not the resolved `HostName`) in the user-facing `ssh <host> gogent daemon start` hint, so the hint matches what the user typed. |
| `internal/sshtunnel/sshconfig_test.go` | **NEW.** Parser unit tests (hermetic via `t.TempDir()` + explicit path / `HOME`). |
| `internal/sshtunnel/tunnel_test.go` | Extend the in-process ssh.Server harness: config-resolved `IdentityFile`+`User` connects; new `ParseConnectURL` cases (config present/absent, URL/flag override). Isolate `HOME` in the existing table test so it stays deterministic. |

turbotui: **no files.** `cmd/main.go`: no change to the flag set (the four ssh
flags already exist and keep their override semantics).

---

## Component 1 — `internal/sshtunnel/sshconfig.go`

```go
type ResolvedSSHConfig struct {
    HostName       string
    User           string
    Port           int
    IdentityFiles  []string
    IdentitiesOnly bool
    Found          bool   // a Host block matched (vs file-missing / no-match)
}

func ReadSSHConfig(host string) (ResolvedSSHConfig, error)
```

### Read order & best-effort

- `ReadSSHConfig(host)` resolves `~/.ssh/config` via `os.UserHomeDir()` (honors
  `$HOME`, so tests isolate with `t.Setenv("HOME", …)`), then optionally falls
  back to the system `/etc/ssh/ssh_config`. A **missing** file is **not** an
  error: skip it. Only a genuinely unreadable/parse-broken file would surface an
  error — and even that is swallowed by the caller (see Component 2), matching
  OpenSSH's "config is advisory" posture.
- `Found=false` + `nil` error when no file exists or no `Host` pattern matches.
- Internals factored so tests don't depend on the real `~/.ssh/config`:
  - `parseSSHConfig(r io.Reader, host string, baseDir string, depth int) (ResolvedSSHConfig, error)` — pure, stream-based.
  - `readSSHConfigFiles(paths []string, host string) (ResolvedSSHConfig, error)` — opens each path best-effort and folds results with first-value-wins.

### Parsing semantics (OpenSSH-faithful, minimal)

- **Line model:** trim, skip blanks and lines whose first non-space rune is `#`.
  Split each line into `keyword` + `value`, accepting both `Key Value` and
  `Key=Value` (OpenSSH allows `=`). Keep the remainder of the line as the value
  (so paths with spaces inside quotes survive — strip one layer of surrounding
  `"`). **Directive keys are case-insensitive** (`strings.EqualFold`).
- **Host blocks:** a `Host pat1 pat2 …` line sets whether the following
  directives are "active" — active iff any pattern matches `host`. Directives
  appearing *before* the first `Host` line are global (always active), as in
  OpenSSH.
- **First-value-wins across all matching blocks** (this is real OpenSSH
  behavior, not "first block wins"): single-valued directives (`HostName`,
  `User`, `Port`, `IdentitiesOnly`) take the **first** value seen while active
  and ignore later ones. `IdentityFile` is **multi-valued**: accumulate every
  value seen while active, in order.
- **Glob matching** for `Host` patterns: support `*` and `?` (translate to a
  segment match — implement a tiny matcher, no regexp dependency needed but
  `path.Match`-style is acceptable since `ssh_config` globs map cleanly).
  Exact-match is the floor; globbing covers the common `Host rpi*`. Negated
  patterns (`!pat`) are handled if trivial (a matched negation disqualifies the
  block); if a negation edge proves fiddly it is acceptable to treat `!` as a
  literal and note it — but the plan is to support the simple negation case.
- **`Include` directive:** expand `~` and globs in the include path (relative
  paths resolve against the including file's dir, OpenSSH-style), then parse each
  matched file **inline at that point** in processing order so first-value-wins
  ordering is preserved. Guard with a recursion `depth` cap (e.g. 16) to avoid
  cyclic includes.
- **Token / path expansion** (kept minimal but correct for the reported case):
  - `HostName`: expand `%h` → original `host`, `%%` → `%`.
  - `IdentityFile`: expand leading `~` / `~user`-less `~/` → home dir; expand
    `%h` → resolved `HostName` (fallback original `host`), `%r` → resolved
    `User`, `%%` → `%`. `%d` → home dir is a cheap bonus if trivial.
  - Expansion happens after the whole file is folded (so `%h`/`%r` see the
    resolved `HostName`/`User`), matching OpenSSH's "resolve then expand".

### Output

`ResolvedSSHConfig` carries the resolved `HostName`, `User`, `Port`,
ordered+expanded `IdentityFiles`, `IdentitiesOnly`, and `Found`.

---

## Component 2 — wire into `ParseConnectURL` (`tunnel.go`)

New `Config` fields:

```go
Alias          string   // the original ssh:// host (alias), for known_hosts hints + diagnostics
IdentityFiles  []string // resolved from ~/.ssh/config IdentityFile (in order)
IdentitiesOnly bool
ConfigFound    bool     // a ~/.ssh/config Host block matched (for diagnostics)
```

Revised flow (URL/flags win; config is the fallback only):

1. Parse URL; capture `urlUser := u.User.Username()` (`""` ⇒ no `user@`) and the
   URL ssh port (existing block) into `cfg.Port`.
2. `cfg.Alias = host`, `cfg.Host = host`, `cfg.User = urlUser`. Fold in
   `Token/KeyPath/KnownHosts/Insecure` exactly as today.
3. Parse `?port=` / `?socket=` (existing).
4. **NEW — best-effort config:** `rc, _ := ReadSSHConfig(host)` (error swallowed;
   advisory). If `rc.Found`:
   - `cfg.ConfigFound = true`
   - `if cfg.User == "" && rc.User != "" { cfg.User = rc.User }`  *(URL `user@` wins)*
   - `if cfg.Port == 0 && rc.Port > 0 { cfg.Port = rc.Port }`     *(URL `:port` wins)*
   - `if rc.HostName != "" { cfg.Host = rc.HostName }`            *(real dial address; `Alias` retains the typed name)*
   - `cfg.IdentityFiles = rc.IdentityFiles`
   - `cfg.IdentitiesOnly = rc.IdentitiesOnly`
5. OS-user fallback (existing): `if cfg.User == "" { user.Current() }`.
6. Empty-user error (existing).

**Precedence guaranteed:** URL `user@`/`:port` and the `--ssh-key` /
`--ssh-known-hosts` / `--ssh-insecure-skip-verify` flags override config. Config
only fills holes. `--ssh-key` (i.e. `cfg.KeyPath`) is untouched by config and is
tried first (see Component 3). `--ssh-known-hosts` and `--ssh-insecure-skip-verify`
flow into `hostKeyCallback` unchanged.

---

## Component 3 — `keyPaths` merge + `IdentitiesOnly` (`tunnel.go`)

```go
func keyPaths(cfg Config) []string {
    var paths []string
    if cfg.KeyPath != "" {            // explicit --ssh-key first
        paths = append(paths, cfg.KeyPath)
    }
    paths = append(paths, cfg.IdentityFiles...)   // config IdentityFile(s), in order
    if !cfg.IdentitiesOnly {                       // append id_* defaults only when not IdentitiesOnly
        // existing ~/.ssh/id_ed25519, id_ecdsa, id_rsa
    }
    return dedup(paths)   // de-dup preserving order
}
```

- **`IdentitiesOnly true`** ⇒ offer only `--ssh-key` + config `IdentityFile`s,
  **skip the `id_*` defaults** (required). De-dup matters: `keyAuth` bundles all
  signers into one `ssh.PublicKeys(...)` method and each key is a separate
  attempt against the server's `MaxAuthTries`, so duplicate paths waste tries.
- **Agent under `IdentitiesOnly`:** the agent (`agentAuth`) is still offered.
  OpenSSH's `IdentitiesOnly=yes` also filters the agent down to the listed
  identities, but doing that precisely needs per-key agent filtering;
  **accepted minimal deviation** — we keep the agent method and only enforce the
  required `id_*`-skip. Noted here and in code.
- `keyAuth`/`agentAuth`/`authMethods` shapes are otherwise unchanged; `keyPaths`
  is the only behavioral edit.

---

## Component 4 — auth-failure diagnostics (`dialClient`, `tunnel.go`)

When `ssh.NewClientConn` returns an **auth** error (message contains
`unable to authenticate` / `no supported methods` — fall back to enriching any
handshake error), wrap with a diagnostic that names *what was attempted*, still
`%w`-wrapping the underlying error and **never** leaking key contents:

```
ssh handshake 192.168.1.5:22: ssh: unable to authenticate …:
  attempted user=pi; keys=[~/.ssh/rpi5_key (loaded), ~/.ssh/id_ed25519 (absent)];
  agent=SSH_AUTH_SOCK present (2 keys);
  ~/.ssh/config: matched 'rpi5' → User=pi HostName=192.168.1.5 Port=22 IdentityFile=~/.ssh/rpi5_key
```

- A small helper `authDiagnostic(cfg) string` builds this from `cfg.User`,
  `keyPaths(cfg)` (annotating each path `loaded`/`absent`/`encrypted` — **paths
  only, no contents**), agent presence + key count (a one-shot agent query on the
  failure path), and the config summary (`cfg.ConfigFound`, `cfg.Alias`,
  `cfg.User`, `cfg.Host`, `cfg.Port`, `cfg.IdentityFiles`). If
  `!cfg.ConfigFound`, it says `~/.ssh/config: no match for 'rpi5'`.
- This makes the headline failure self-diagnosing: the user sees *which* user,
  *which* keys, whether the agent was consulted, and whether/what config applied.

`cmd/attach.go`: the existing `sshTarget`/"no daemon found … `ssh %s gogent
daemon start`" hint switches to the **alias** (`cfg.Alias`) rather than the
resolved `HostName`, so the suggested command matches what the user typed and
what `ssh` itself accepts.

---

## User-facing behavior

- `gogent --connect ssh://rpi5` with a `Host rpi5` block (`User pi`,
  `HostName 192.168.1.5`, `IdentityFile ~/.ssh/rpi5_key`) connects with **no
  extra flags** — `ssh rpi5` parity.
- `ssh://bob@rpi5`, `ssh://rpi5:2222`, `--ssh-key …`, `--ssh-known-hosts …`,
  `--ssh-insecure-skip-verify` continue to **override** config.
- On auth failure the error is actionable (user/keys/agent/config), instead of
  the bare `attempted methods [none publickey]`.
- No config / no match: behavior is exactly as today (OS user, `id_*` defaults).

---

## Design criteria

### (1) Goal match
Implements exactly the ask — read `~/.ssh/config` for the matching `Host` and
apply `HostName/User/Port/IdentityFile/IdentitiesOnly` to the `ssh.ClientConfig`,
config as fallback, URL/flags win. It is a **fix**, not a feature: no new flags,
no new transport, no refactor of the dial/discover/restart machinery. Explicit
non-goals (ProxyJump/Match/known-hosts files/etc.) are excluded.

### (2) Usability
URL fields + CLI flags override config (least surprise for power users); the
config fallback removes the need for flags in the common case; the auth-failure
error surfaces *exactly* what was attempted and whether config applied — nothing
silent. The `ssh <alias> …` hint matches the user's own vocabulary.

### (3) No regressions
- `hostKeyCallback`, `authMethods`, `agentAuth`, `keyAuth`, `New`, `Discover`,
  `DialContext`, `Restart`, `Close` are unchanged except `keyPaths` (additive,
  guarded by new empty-by-default fields) and the `dialClient` error-wrap
  (message only).
- A `Config{}` with no config fields behaves as before; all existing
  integration tests (`insecureCfg`, host-key, restart, concurrency) are
  unaffected — they construct `Config` literals that leave the new fields zero.
- **`TestParseConnectURL` determinism:** `ParseConnectURL` now consults the real
  `~/.ssh/config`. To keep the existing table test asserting unchanged behavior
  on any machine, the test sets `HOME` to an empty `t.TempDir()` so
  `ReadSSHConfig` finds nothing (`Found=false`) — same assertions, now hermetic.
  No production behavior change; this is a test-isolation fix.
- gofmt/build/vet clean; golangci-lint v2 whole-repo 0 **new** issues; `go test
  ./...` green (no `-race` on Pi5). Pre-existing environmental
  `TestUserSessionSendMessage` 404 remains the only acceptable failure.

### (4) Holistic across both repos
- The change lives entirely in `internal/sshtunnel/` (+ a one-line diagnostic
  tweak in `cmd/attach.go`). The seam to turbotui — the injected
  `DialContext("http://ssh", tunnel.DialContext)` — is **unchanged**; turbotui
  still consumes an opaque `net.Conn`-yielding dialer and needs no edit. No
  `go.mod` bump, no new dependency. Config resolution happens at
  `ParseConnectURL` time (cmd side), so by the time the tunnel/DialContext seam
  is reached the target is already fully resolved — turbotui sees no difference.

---

## Regression risks / notes

- **known_hosts now keyed by `HostName`.** Dialing the resolved `HostName` means
  strict `hostKeyCallback` verifies against `HostName` (e.g. `192.168.1.5`), not
  the alias. This *matches* OpenSSH's common case (`ssh rpi5` stores/verifies the
  key under the `HostName`), so for a host that already works via `ssh` it is
  consistent. `hostKeyCallback` stays unchanged per the issue; the keyscan hint
  uses `cfg.Host` (the real host), which is correct. Documented, not a code
  change.
- **MaxAuthTries:** merging IdentityFiles + id_* defaults + agent could, in
  pathological configs, exceed a server's `MaxAuthTries`. De-dup in `keyPaths`
  mitigates the common overlap; the count is otherwise the same order as before
  (1 agent method + 1 multi-key method).
- **Token expansion is intentionally partial** (`~`, `%h`, `%r`, `%%`, maybe
  `%d`). Other `%`-tokens (`%p`, `%L`, `%n`, …) are not expanded; they don't
  appear in the reported case and are left literal. Noted in code.

## Open questions

1. **`/etc/ssh/ssh_config` fallback** — include it (closer to OpenSSH) or limit
   to `~/.ssh/config`? Leaning *include it, best-effort* (read after the user
   file, user file wins via first-value-wins); it is cheap and harmless. Flag if
   the reviewer prefers user-file-only for the first cut.
2. **`IdentitiesOnly` + agent** — keep the agent method (proposed minimal
   deviation) or also filter agent identities to the listed keys? Keeping it is
   the documented minimal choice; full agent filtering is a possible follow-up.
3. **Negated `Host !pattern`** — support the simple case or defer? Plan is to
   support simple negation; deferring (treating `!` literally) is acceptable if
   it risks the gate. Will not block on this.
