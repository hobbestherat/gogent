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
- **Malformed values are skipped, not propagated** (advisory-config posture):
  `Port` is `strconv.Atoi`'d — a non-numeric / out-of-range value leaves
  `rc.Port == 0` (unset) rather than erroring or storing a bogus `int`.
  `IdentitiesOnly` enables only on `yes`/`true`; any other value leaves it
  `false`. A single broken line never poisons the rest of the resolve.
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

> **Revised after critique.** An earlier draft appended the `id_*` defaults
> *unconditionally* (whenever `!IdentitiesOnly`), i.e. **even when `--ssh-key`
> was set**. That silently changed the established `--ssh-key` contract
> ("default: SSH agent + `~/.ssh/id_*`" ⇒ the flag *replaces* that default) and
> — because every existing integration test sets `KeyPath` and
> `disableRealSSHEnv` clears only `SSH_AUTH_SOCK`, **not `HOME`** — would make
> the whole suite `os.ReadFile` the developer's real `~/.ssh/id_*`, including a
> blocking passphrase prompt on an encrypted real key (`loadSigner` →
> `promptPassphrase` returns true on a TTY, and the Pi5 suite runs from a
> terminal). The corrected design below keeps `--ssh-key` authoritative so the
> existing tests' candidate list is **byte-for-byte unchanged**.

```go
func keyPaths(cfg Config) []string {
    // An explicit --ssh-key is authoritative: it (and any config IdentityFile)
    // REPLACES the id_* defaults — preserving the flag's established contract.
    if cfg.KeyPath != "" {
        return dedup(append([]string{cfg.KeyPath}, cfg.IdentityFiles...))
    }
    // No --ssh-key: config IdentityFile(s) first, then the conventional
    // ~/.ssh/id_* defaults UNLESS IdentitiesOnly suppresses them.
    paths := append([]string{}, cfg.IdentityFiles...)
    if !cfg.IdentitiesOnly {
        // existing ~/.ssh/id_ed25519, id_ecdsa, id_rsa (via os.UserHomeDir)
    }
    return dedup(paths)   // de-dup preserving order
}
```

Resulting candidate lists:

| inputs | candidate keys |
|---|---|
| `KeyPath` set (existing tests) | `[KeyPath]` — **identical to today** |
| `KeyPath` set + config `IdentityFile` | `[KeyPath, IdentityFile…]` (no `id_*`) |
| no `KeyPath`, config `IdentityFile`, `IdentitiesOnly=false` (the rpi5 case) | `[IdentityFile…, id_ed25519, id_ecdsa, id_rsa]` |
| no `KeyPath`, config `IdentityFile`, `IdentitiesOnly=true` | `[IdentityFile…]` only |
| nothing (no config, no flag) | `[id_ed25519, id_ecdsa, id_rsa]` — **identical to today** |

- **rpi5 parity preserved:** the headline no-`--ssh-key` case still offers the
  config `IdentityFile` *alongside* the `id_*` defaults (OpenSSH parity — issue
  required-change #3 is satisfied for the reported case).
- **`--ssh-key` authoritative (intentional, critic-endorsed):** when the user
  passes `--ssh-key` they get exactly that key (+ any config `IdentityFile`),
  **not** the `id_*` defaults. This (a) matches the flag's documented "default:
  … `~/.ssh/id_*`" contract — the flag overrides the default rather than
  augmenting it; (b) keeps the existing integration tests' candidate list
  unchanged so they neither read real keys nor hang; (c) avoids offering an
  unrelated personal key to an arbitrary host. This is a deliberate, narrow
  deviation from the most-literal "`--ssh-key` *and* `id_*`" reading; called out
  in the open questions.
- **`IdentitiesOnly true`** ⇒ never the `id_*` defaults (required). De-dup
  matters: `keyAuth` bundles all signers into one `ssh.PublicKeys(...)` method
  and each key is a separate attempt against the server's `MaxAuthTries`, so
  duplicate paths waste tries; the agent is tried first (its method is appended
  first in `authMethods`), so a fat agent can starve the config `IdentityFile` —
  de-dup limits the file-side count and keeping `--ssh-key` authoritative caps
  the worst case.
- **Agent under `IdentitiesOnly`:** the agent (`agentAuth`) is still offered.
  OpenSSH's `IdentitiesOnly=yes` also filters the agent to the listed
  identities; doing that precisely needs per-key agent filtering —
  **accepted minimal deviation** (explicitly permitted by the issue), enforcing
  only the required `id_*`-skip. Noted here and in code; a user expecting
  `IdentitiesOnly` to constrain the agent is the one quiet surprise.
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
  only, no contents**), agent presence + key count, and the config summary
  (`cfg.ConfigFound`, `cfg.Alias`, `cfg.User`, `cfg.Host`, `cfg.Port`,
  `cfg.IdentityFiles`). If `!cfg.ConfigFound`, it says
  `~/.ssh/config: no match for 'rpi5'`.
- **Bounded agent query:** the one-shot agent key count runs on the
  already-failing path, so it must not stall on a wedged `SSH_AUTH_SOCK`. The
  helper dials the agent socket with a short deadline (e.g. ~500ms,
  `net.DialTimeout` + a read deadline on the `agent.Signers` call) and on any
  error/timeout degrades to `agent=present (key count unavailable)` /
  `agent=none`. It never blocks the error from surfacing.
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
  `DialContext`, `Restart`, `Close` are unchanged. The only behavioral edit is
  `keyPaths`; `dialClient` changes the error-wrap *message* only.
- **`keyPaths` is provably non-regressing for the existing tests.** With
  `--ssh-key` kept authoritative (Component 3), any `Config` that sets `KeyPath`
  and leaves the new fields zero — which is *every* existing integration test
  (`insecureCfg()` at `tunnel_test.go:432-436`, `writeRSAKeyFile`-based cfgs,
  `TestNew_*`, `TestHostKey_*`, `TestRestart_*`, `TestDialContext_*`) — returns
  exactly `[KeyPath]`, byte-for-byte the old result. **No `os.UserHomeDir()` /
  `id_*` read is reached on these paths**, so the developer's real
  `~/.ssh/id_*` is never touched and the encrypted-key passphrase-prompt hang is
  impossible. (This corrects the earlier draft, which appended `id_*`
  unconditionally and *would* have read real keys + hung — see Component 3's
  note.)
- **Defense-in-depth: harden `disableRealSSHEnv`** (`tunnel_test.go:43-46`) to
  also point `HOME` (and `USER`) at a `t.TempDir()`, not just clear
  `SSH_AUTH_SOCK`. This makes the harness's stated invariant ("prevent
  interference from the developer's real ssh-agent / keys") robust against
  *future* changes to `keyPaths` as well as `id_*` defaults, and makes the new
  config tests hermetic by construction. It is purely additive test isolation,
  no production effect.
- **`TestParseConnectURL` determinism:** `ParseConnectURL` now consults the real
  `~/.ssh/config`. The table test sets `HOME` to an empty `t.TempDir()` (it does
  not use `disableRealSSHEnv`) so `ReadSSHConfig` finds nothing (`Found=false`)
  — same assertions, now hermetic. No production behavior change.
- Existing integration tests reach `New`/`dialClient` directly (not
  `ParseConnectURL`), so `ReadSSHConfig` is **not** invoked on those paths;
  config resolution is confined to the `ParseConnectURL` entry point.
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
- **MaxAuthTries:** merging config `IdentityFiles` + `id_*` defaults + the
  (agent-first) agent grows the signer list, which in a pathological config
  could exceed a server's `MaxAuthTries`. Mitigations: de-dup in `keyPaths`
  collapses overlap (config `IdentityFile` == an `id_*`); `--ssh-key` stays
  authoritative so the explicit-key path is capped; and the agent is offered as
  before. The structure is still 1 agent method + 1 multi-key method, just with
  a (de-duped) longer key list in the no-`--ssh-key` + config case — the same
  growth OpenSSH itself incurs when a `Host` block adds `IdentityFile`s.
- **Token expansion is intentionally partial** (`~`, `%h`, `%r`, `%%`, maybe
  `%d`). Other `%`-tokens (`%p`, `%L`, `%n`, …) are not expanded; they don't
  appear in the reported case and are left literal. Noted in code.

## Tests (concrete)

- **`sshconfig_test.go` (new, hermetic via `t.TempDir()` + explicit path or
  `HOME`):** parse a sample config and assert `HostName/User/Port/IdentityFile(s)/
  IdentitiesOnly`; `~` and `%h`/`%r` expansion; `Include`; glob `Host rpi*`;
  first-value-wins precedence across multiple matching blocks; **missing file
  ⇒ `Found=false`, `nil` error**; **malformed `Port`/`IdentitiesOnly` line is
  skipped, not fatal**.
- **`tunnel_test.go` (extend):**
  - Harden `disableRealSSHEnv` to isolate `HOME`/`USER` (see criterion 3) — this
    one change protects the whole existing suite.
  - New end-to-end: write a temp `~/.ssh/config` (`Host alias` →
    `HostName 127.0.0.1`, `Port <server>`, `User test`,
    `IdentityFile <clientKey>`), `ParseConnectURL("ssh://alias", …, insecure=true)`,
    then `New` connects against the in-process server — proving config-resolved
    `User` + `IdentityFile` auth succeeds with **no `--ssh-key`**.
  - `ParseConnectURL` cases: config **present** (fills User/Port/HostName/
    IdentityFile) vs **absent** (`Found=false`, unchanged); **override** —
    `ssh://bob@alias` keeps `bob`, `ssh://alias:2222` keeps `2222`, `--ssh-key`
    keeps the flag key and suppresses `id_*` (assert via `keyPaths`).
  - `keyPaths` unit cases for the five rows of the Component 3 table.

## Open questions

1. **`/etc/ssh/ssh_config` fallback** — include it (closer to OpenSSH) or limit
   to `~/.ssh/config`? Leaning *include it, best-effort* (read after the user
   file, user file wins via first-value-wins); it is cheap and harmless. Flag if
   the reviewer prefers user-file-only for the first cut.
2. **`--ssh-key` replaces vs augments `id_*`** — this design keeps `--ssh-key`
   **authoritative** (it replaces the `id_*` defaults, preserving the flag's
   contract and the test harness's real-key isolation). The most-literal reading
   of the issue (`[--ssh-key, IdentityFile…, id_*]`) would also append `id_*`
   when `--ssh-key` is set. Confirming the authoritative reading is preferred
   (it is less surprising and the critic endorsed it); easy to flip if the
   maintainer wants strict literalism — but that re-introduces the real-key test
   leak unless `HOME` isolation is in place (which it now is).
3. **`IdentitiesOnly` + agent** — keep the agent method (proposed minimal
   deviation) or also filter agent identities to the listed keys? Keeping it is
   the documented minimal choice; full agent filtering is a possible follow-up.
4. **Negated `Host !pattern`** — support the simple case or defer? Plan is to
   support simple negation; deferring (treating `!` literally) is acceptable if
   it risks the gate. Will not block on this.
