# Design: Native `--connect ssh://` transport (Tier 2 remote attach) — Issue #482

Status: DESIGN. No code written yet. Line numbers below were verified against the
current branch (`pair2/issue-482-...`, on top of #481/#485).

## 0. One-paragraph summary

Add an `ssh://[user@]host[:sshport]` scheme to `--connect`. When given, gogent opens
an in-process SSH session (golang.org/x/crypto/ssh), reads `~/.gogent/daemon.addr`
over the session to auto-resolve the *running* daemon's transport (Unix socket OR
TCP — `--tcp` not required), and builds an `APIClient` whose
`http.Transport.DialContext` opens an SSH channel per request (mirroring the proven
`unix://` placeholder-base pattern). `Health()`, the SSE stream, the approvals
poller, auth (bearer token), the disconnect modal, and exponential backoff all reuse
the existing machinery unchanged. Teardown closes the SSH session; the remote daemon
keeps running.

---

## 1. Verified facts (current source, not assumptions)

| Fact | Location | Note |
|---|---|---|
| `resolveMode` is scheme-agnostic, passes `connect` through verbatim | `cmd/attach.go:18-39` | `ssh://` already flows through. No change. |
| `runAttached(homeDir, addr, token string, noColorFlag bool)` builds client → Health → RemoteClient → classify local | `cmd/attach.go:51-170` | Seam for tunnel construction + teardown. |
| Client build | `cmd/attach.go:63` `tuipkg.NewAPIClient(addr, token)` | |
| Synchronous probe | `cmd/attach.go:69-71` `client.Health()` | Runs over the tunnel; tunnel must be up first. |
| RemoteClient build | `cmd/attach.go:92` `NewRemoteClient(client, …)` | |
| Local/remote classification | `cmd/attach.go:114` `local := strings.HasPrefix(addr, "unix://")` | `ssh://` → `false` → `DaemonModeAttachedRemote`. Correct already. |
| Teardown | `cmd/attach.go:166` `rc.Close()` | tunnel.Close() goes right after. |
| `NewAPIClient(addr, token string) (*APIClient, error)` — single scheme switch | `ui/tui/api_client.go:81-114` | `unix`/`http`/`https`; `default:` → `"unsupported connect scheme %q (want unix:// \| http:// \| https://)"`. |
| `unix` case builds `base:"http://unix"` + `DialContext` closure dialing the socket | `ui/tui/api_client.go:86-103` | The exact pattern `ssh` will mirror. |
| `APIClient{ http, base, token, … }` | `ui/tui/api_client.go:36-45` | No stored dialer field; the dialer lives in the `http.Transport` closure. |
| `Health()` = `c.do(GET, "/health", …)` | `ui/tui/api_client.go:359-363` | `/api/health` is `pub`/AuthNone (`internal/server/api.go:250`). Works pre-auth. |
| `RemoteClient` struct (ctx/cancel, client, reconnector, streamCancel, …) | `ui/tui/remote_handlers.go:69-117` | No tunnel handle today — we add one. |
| `Start` launches `consume`/`monitorHealth`/`pollApprovals` after a synchronous `openStream()` | `ui/tui/remote_handlers.go:174-202` | |
| `reconnect()` backoff loop; top of loop = after `notifyLost`, before `openStream` | `ui/tui/remote_handlers.go:297-320` | **Restart seam.** Backoff 0.5→1→2→5→10s cap (`backoffFor`, :354-367). |
| `Close()` = `rc.cancel()` only | `ui/tui/remote_handlers.go:166` | One context bounds every goroutine + in-flight turn. |
| Send handlers use `context.Background()` | `ui/tui/remote_handlers.go:558,588,818` | Detach does not cancel daemon turns (#481 already landed). |
| `daemon.addr` written `0600`: `baseAddr [+ " http://host:port" if --tcp]` | `cmd/daemon.go:272-275`, `internal/daemon/status.go:59-66` | `baseAddr` = `unix:///path` (Unix) or `http://127.0.0.1:port` (Windows). |
| `readAddr` fallback when file absent = `"unix://" + p.Sock` | `internal/daemon/status.go:68-76` | Same fallback we use over SSH. |
| Home dir = `os.UserHomeDir()`; **no `--gogent-home` / `GOGENT_HOME`** | `internal/daemon/paths.go:52-58` | **Settles Q5: non-default homes are not a thing today → out of scope for v1.** |
| `--connect` help string lists only `unix/http/https` | `cmd/main.go:42` | Must be extended. |
| go.mod: `x/crypto` ABSENT; `x/sys` + `x/term` PRESENT (indirect); go 1.25.11 | `go.mod` | Adding `x/crypto` promotes existing indirects, small transitive surface. |
| Docs declare Tier 2 "planned" | `docs/usage-headless.md:310-314` | Replace with the shipped behavior. |

---

## 2. Design

### 2.1 New package `internal/sshtunnel`

```
type Config struct {
    User, Host string
    Port       int            // default 22
    Token      string         // daemon bearer token (passed through to APIClient, not SSH)
    KeyPath    string         // --ssh-key; "" → agent + ~/.ssh/id_* defaults
    KnownHosts string         // --ssh-known-hosts; "" → ~/.ssh/known_hosts
    Insecure   bool           // --ssh-insecure-skip-verify
    DaemonPort int            // ?port= override (TCP); 0 → auto
    DaemonSock string         // ?socket= override (Unix); "" → auto
}

type ResolvedTarget struct {  // exactly one of these is set
    UnixSocket string
    TCPAddr    string         // "host:port"
}

type Tunnel struct {          // owns one *ssh.Client; safe for concurrent Dial
    cfg    Config
    mu     sync.Mutex         // guards client across Restart
    client *ssh.Client
    target ResolvedTarget
}

func New(ctx, Config) (*Tunnel, error)             // dial TCP to host:sshport, SSH handshake + auth, host-key verify
func (*Tunnel) Discover() (ResolvedTarget, error)  // exec `cat ~/.gogent/daemon.addr`; parse; fallback default sock
func (*Tunnel) DialContext(ctx, network, addr) (net.Conn, error)  // dispatch on resolved target
func (*Tunnel) Restart() error                     // re-dial+re-auth+re-Discover under mu; replaces dead client
func (*Tunnel) Close() error
```

**Auth order (v1):** SSH agent (`SSH_AUTH_SOCK`) first → explicit `--ssh-key` →
default `~/.ssh/id_ed25519`, `~/.ssh/id_rsa` (passphrase prompt via
`golang.org/x/term` on a TTY). Password auth: deferred follow-up (documented). All
SSH auth failures surface as Go errors from `New()` before any UI is built.

**Host-key verification (v1, settles Q6):** `knownhosts.New(~/.ssh/known_hosts)` by
default; mismatch → hard fail with the offending key fingerprint. `--ssh-insecure-skip-verify`
swaps in `ssh.InsecureIgnoreHostKey()` for trusted/lab use, and we `log`/banner that
verification is disabled (not silent).

**Discover() parsing** (mirrors `readAddr` exactly):
1. `session.Output("cat ~/.gogent/daemon.addr")` (or `Run`). Note: `~` is expanded by
   the remote login shell, and gogent always uses `os.UserHomeDir()`, so this is the
   canonical path with no `--gogent-home` complication.
2. `strings.Fields(trim(out))` → tokens.
   - 1st token `unix:///path` → `ResolvedTarget{UnixSocket: path}`.
   - 1st token `http://127.0.0.1:port` (Windows daemon) → `ResolvedTarget{TCPAddr: "127.0.0.1:port"}`.
   - optional 2nd token `http://host:port` (present only with `--tcp`) → **prefer** it → `TCPAddr`.
   - `?port=`/`?socket=` query overrides win over discovery when set.
3. exec error / empty / file-not-found → fall back to default socket
   `~/.gogent/daemon.sock` as a Unix target (same as local `readAddr`). If the
   subsequent `Health()` then fails, we emit the fail-fast "no daemon found" error
   (§2.5) — so an absent daemon never yields an empty TUI.

**DialContext dispatch** (net/http calls it per request; ignores the passed
network/addr exactly like the unix case):
- `UnixSocket != ""` → `t.client.Dial("unix", socket)` → `direct-streamlocal@openssh.com`
  channel. **Works with a daemon bound only to the socket — no `--tcp`, no TCP listener.**
- `TCPAddr != ""` → `t.client.Dial("tcp", TCPAddr)` → `direct-tcpip`.
Both return a `net.Conn` backed by an `ssh.Channel`; `http.Transport` pools/reuses them.

### 2.2 `ui/tui/api_client.go` — add `case "ssh":`

Keep `NewAPIClient` as the single scheme switch, but let `runAttached` own the
tunnel's lifecycle. Make the constructor variadic (backward compatible):

```go
type APIClientOption func(*apiClientOpts)
func WithDialContext(base string, dc func(ctx, network, addr) (net.Conn, error)) APIClientOption

func NewAPIClient(addr, token string, opts ...APIClientOption) (*APIClient, error)
```

- `unix`/`http`/`https`: unchanged behavior, ignore opts.
- new `case "ssh":` — requires an injected dialer:
  - parse `user@host[:sshport]` and `?port=`/`?socket=` (validated here so a bad URL
    fails before the tunnel is dialed),
  - if no `WithDialContext` was injected → return an error
    (`"internal: ssh:// requires an injected tunnel"`); this never happens in the
    real path because `runAttached` always injects.
  - with injection → `APIClient{ base: "http://ssh", token: token, http: &http.Client{Transport: &http.Transport{DialContext: dc}} }`.
- `default:` error string updated to include `ssh://`.

Rationale for variadic injection over building the tunnel inside `NewAPIClient`
(settles Q3): the tunnel is stateful infra (its own goroutines, Close, Restart) and
`runAttached` already owns connection lifecycle (it owns `rc.Close()`); building it
inside the pure transport-selection function would split ownership and make the
synchronous "is SSH reachable?" failure happen in the wrong layer.

### 2.3 `cmd/attach.go` — `runAttached`

Before line 63 (`NewAPIClient`), branch on scheme:

```go
var apiOpts []tuipkg.APIClientOption
var tunnel *sshtunnel.Tunnel
if strings.HasPrefix(addr, "ssh://") {
    cfg := parseSSHConfig(addr, token, *sshKey, *sshKnownHosts, *sshInsecure) // flags from main.go
    tunnel, err = sshtunnel.New(ctx, cfg)        // dial + auth + host-key verify (fail fast)
    if err != nil { return fmt.Errorf("ssh connect %s: %w", cfg.Host, err) }
    tgt, derr := tunnel.Discover()               // read daemon.addr (fail fast w/ actionable msg)
    if derr != nil { tunnel.Close(); return fmt.Errorf("resolve daemon at %s: %w", cfg.Host, derr) }
    apiOpts = append(apiOpts, tuipkg.WithDialContext("http://ssh", tunnel.DialContext))
}
client, err := tuipkg.NewAPIClient(addr, token, apiOpts...)
...
// existing client.Health() at :69 now probes over the tunnel (see §2.5 for the fail-fast wrap)
...
rc := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb)
if tunnel != nil { rc.SetTunnel(tunnel) }        // give reconnect a Restart handle (§2.4)
...
// teardown at :166
rc.Close()
if tunnel != nil { tunnel.Close() }              // session + listener gone; daemon untouched
```

`local := strings.HasPrefix(addr, "unix://")` (:114) is already correct: `ssh://`
→ `false` → `DaemonModeAttachedRemote` → daemon menu shows "Daemon status" only, no
local Start/Stop. Optional polish: label the remote with the SSH host.

### 2.4 `ui/tui/remote_handlers.go` — Restart on reconnect

Add a tiny interface + optional field; do **not** make RemoteClient own Close of the
tunnel (runAttached owns that — single owner):

```go
type TunnelRestarter interface{ Restart() error }   // *sshtunnel.Tunnel satisfies it; nil for unix/http
// new field on RemoteClient:  tunnel TunnelRestarter
func (rc *RemoteClient) SetTunnel(t TunnelRestarter) { rc.tunnel = t }
```

In `reconnect()` (:297), at the top of the loop *after* `notifyLost(attempt)` and the
backoff wait, *before* `openStream()`:

```go
if rc.tunnel != nil {
    if err := rc.tunnel.Restart(); err != nil {
        continue   // SSH still down → next attempt, longer backoff, modal stays up
    }
}
next, err := rc.openStream()
```

A dead SSH session is the *likely* cause of a stream drop, so re-establishing the
tunnel before re-subscribing is correct. A `Restart` failure feeds the existing
backoff; on success, `openStream` → `notifyRestored` → `kickApprovals` (jump-to-present)
fire exactly as today. No second reconnect state machine.

### 2.5 Fail-fast errors (usability gate)

- **Unreachable host / SSH refused** → from `sshtunnel.New` → `"ssh connect <host>: …"`.
- **Auth failure** → from `New` → `"ssh connect <host>: ssh: handshake failed: …"` (or passphrase/agent hint).
- **Host-key mismatch** → from `New` → fingerprint + `"add it to known_hosts or pass --ssh-insecure-skip-verify"`.
- **No daemon running** → `Discover` falls back to default socket; the existing
  `Health()` at `cmd/attach.go:69-71` then fails. Wrap that specific case for the
  ssh path: `"no daemon found at ssh://<host> — start it with: gogent daemon start"`.

All occur before any UI is constructed → never an empty TUI.

### 2.6 `cmd/main.go` — flags + help

- Extend `--connect` help (:42) to add `| ssh://[user@]host[:sshport]` and a note that
  it auto-resolves the running daemon (no `--tcp` needed).
- New flags: `--ssh-key`, `--ssh-known-hosts`, `--ssh-insecure-skip-verify` (Q2/Q6).
  All default to empty/false → `~/.ssh` defaults + strict known_hosts.

### 2.7 Dependency

`go get golang.org/x/crypto`, then `go mod tidy`. Promotes `x/sys`/`x/term` from
indirect to direct (already vendored). Sanctioned for this feature: stdlib has no SSH,
in-process SSH genuinely requires it (Design §A, karma confirmed by maintainer). Keep
surface minimal — only `golang.org/x/crypto/ssh`, `.../ssh/agent`,
`.../ssh/knownhosts`.

### 2.8 Docs

Replace `docs/usage-headless.md:310-314` ("Tier 2 … planned") with the shipped flow:
single-command `gogent --connect ssh://user@machineB`, auto-resolution, no `--tcp`,
auth/host-key behavior, the new flags, and the known limitation note (below) if #481
were absent — but #481 has landed, so document full disconnect-recovery.

---

## 3. The four gates

**(1) GOAL MATCH.** Exactly the issue's ask: one command attaches the TUI to a remote
daemon; auto-resolves a daemon started with plain `gogent daemon start` (socket-only,
no `--tcp`) by reading `daemon.addr` over SSH; `--tcp` daemon also supported (2nd
token preferred); `?port=`/`?socket=` overrides; token authenticates over the tunnel;
no manual `ssh -L`. No scope creep — Phase-3 (watcher mgmt / archived sessions over the
wire), remote daemon auto-start, and daemon-side TLS are explicitly out.

**(2) USABILITY.** User drives input via a single `--connect ssh://…` URL + standard
`--token`/`GOGENT_HTTP_TOKEN` + optional `--ssh-*` flags. Every failure mode
(unreachable / auth / host-key / no daemon) fails fast with an actionable message
*before* the TUI — never a blank screen. SSH drop raises the **existing** disconnect
modal; "Retry now" collapses backoff and `tunnel.Restart()`+`openStream` re-establish
tunnel+SSE+jump-to-present. Exit closes the SSH session; the daemon keeps running
(detach never stops it). Remote daemon menu correctly hides local Start/Stop.

**(3) NO REGRESSIONS.** `unix`/`http`/`https` paths are untouched: `NewAPIClient`'s
variadic opts are ignored by those cases; `RemoteClient.tunnel` is nil for them so the
new `reconnect` branch is skipped; `resolveMode`, classification, teardown order all
unchanged. New behavior is purely additive (`case "ssh"`, a nil-guarded field, a new
package). Risks + mitigations:
- *`NewAPIClient` signature change* → variadic, so all existing call sites compile unchanged.
- *Concurrent `DialContext` vs `Restart` swapping `*ssh.Client`* → guard `client` with
  `Tunnel.mu`; `DialContext` snapshots the client under the lock.
- *Per-request channel cost* → `http.Transport` pooling reuses channels, same as unix.
- Gates to run: `gofmt`, `go build ./...`, `go vet ./...`, `golangci-lint` (0 NEW),
  `go test ./...` (no `-race` on Pi5).

**(4) HOLISTIC across both repos.** **turbotui is NOT touched.** It is consumed as a
versioned module (`github.com/hobbestherat/turbotui v0.3.1-…` in go.mod) and provides
the TUI widget/runtime framework; this feature lives entirely *below* the UI layer in
gogent's transport (`internal/sshtunnel`, `ui/tui/api_client.go`, `cmd/`,
`remote_handlers.go`). The disconnect modal and reconnect signals are gogent's own
`Reconnector` interface — no turbotui API surface changes. The repo seam (gogent
depends on turbotui, never the reverse) is respected; no downstream turbotui effect.
The new dep (`x/crypto`) lands only in gogent's go.mod. Built on top of #481 (async
turn dispatch) which has merged, so a tunnel drop during an in-flight turn no longer
orphans it — daemon, session, pending approvals, and completion notifications all
survive the reconnect.

---

## 4. Files touched (gogent only)

| File | Change |
|---|---|
| `internal/sshtunnel/` (new) | `Tunnel`: SSH session, `New`/`Discover`/`DialContext`/`Restart`/`Close`; auth + known_hosts. |
| `ui/tui/api_client.go` | `case "ssh"` in `NewAPIClient`; variadic `APIClientOption` + `WithDialContext`; update `default:` error string. |
| `cmd/attach.go` | `runAttached`: parse `ssh://`, build+Discover tunnel before `NewAPIClient`, inject dialer, `SetTunnel`, close tunnel after `rc.Close()`, wrap no-daemon Health error. |
| `ui/tui/remote_handlers.go` | `TunnelRestarter` field + `SetTunnel`; `Restart()` at top of `reconnect` loop. |
| `cmd/main.go` | extend `--connect` help; add `--ssh-key`/`--ssh-known-hosts`/`--ssh-insecure-skip-verify`. |
| `docs/usage-headless.md` | replace lines 310-314 "Tier 2 planned" with shipped behavior. |
| `go.mod` / `go.sum` | add `golang.org/x/crypto`; `go mod tidy`. |

No turbotui files.

---

## 5. Tests to add

- `ssh://` URL parsing: user / host / sshport / `?port=` / `?socket=` (table test).
- `Discover` parsing: `unix:///…` token; `http://127.0.0.1:port` token; `--tcp` second
  `http://host:port` token preferred; absent file → default-socket fallback.
- `DialContext` returns a working `net.Conn` against a loopback in-process test SSH
  server (`x/crypto/ssh` server side) serving both a Unix socket and a TCP target.
- `reconnect` calls `tunnel.Restart` before `openStream` (inject a fake `TunnelRestarter`).
- `Close`/teardown closes the SSH session (assert the test server sees the channel/conn close).
- Fail-fast: unreachable host, auth failure, no daemon (Health fails → actionable error).

---

## 6. Open questions

1. **Passphrase/password prompting in attach flow.** Passphrase-protected keys and
   password auth need a TTY prompt (`x/term`) *before* the TUI takes the screen.
   Recommendation: agent + unencrypted default keys cover v1 non-interactively; prompt
   for a key passphrase on a raw TTY pre-TUI; defer interactive *password* auth to a
   follow-up. Confirm scope for v1.
2. **`?socket=`/`?port=` ergonomics.** Confirm query-param overrides (vs new
   `--ssh-daemon-socket` flags) are acceptable; they keep the single-URL surface but
   are slightly obscure. Default path needs neither.
3. **known_hosts UX on first connect.** Strict known_hosts fails on an unknown host
   (no interactive "yes/no" trust-on-first-use). v1: fail with a copy-pasteable hint to
   `ssh-keyscan`/add the key, or use `--ssh-insecure-skip-verify`. Confirm we don't want
   a TOFU prompt in v1.
4. **Daemon on Windows host (TCP-only `http://127.0.0.1:port` primary).** Covered by the
   TCP dispatch path; confirm no additional handling needed (loopback bind is reachable
   via `direct-tcpip`).
