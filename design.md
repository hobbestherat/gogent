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
| Auth gate: a request on a **unix-socket listener** OR a **loopback** RemoteAddr → local human, **no token needed** | `internal/server/auth.go:92` (`isLoopback \|\| isUnixRequest`), `:146-153`, `:160-165` | `isUnixRequest` keys on the *listener* (`la.Network()=="unix"`), not peer uid. **This is why streamlocal-over-SSH works token-free** — see §2.9. |
| `runAttached` cancelable `ctx` created at `:131`, **after** `NewAPIClient`/`Health`; signal handler installed at `:158`, **after** `rc.Start` | `cmd/attach.go:131`, `:158-164` | The initial connect has no ctx/signal to break it today — §2.3 moves both earlier. |
| `RetryNow` only honored while the reconnect goroutine sits in its backoff `select` | `ui/tui/remote_handlers.go:153-158`, `:300-305` | A blocking `Restart()` would make "Retry now" a no-op until it returns — §2.4 bounds + cancels it. |
| `readAddr` fallback when file absent = `"unix://" + p.Sock` | `internal/daemon/status.go:68-76` | Same fallback we use over SSH. |
| Home dir = `os.UserHomeDir()`; **no `--gogent-home` / `GOGENT_HOME`** | `internal/daemon/paths.go:52-58` | **Settles Q5: non-default homes are not a thing today → out of scope for v1.** |
| `--connect` help string lists only `unix/http/https` | `cmd/main.go:42` | Must be extended. |
| go.mod: `x/crypto` ABSENT; `x/sys` + `x/term` PRESENT (indirect); go 1.25.11 | `go.mod` | Adding `x/crypto` promotes existing indirects, small transitive surface. |
| Docs declare Tier 2 "planned" | `docs/usage-headless.md:310-314` | Replace with the shipped behavior. |

---

## 2. Design

### 2.1 New package `internal/sshtunnel`

```
const dialTimeout = 10 * time.Second   // bounds TCP connect + SSH handshake (fail-fast)

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
    mu     sync.Mutex         // guards client+target across Restart
    client *ssh.Client
    target ResolvedTarget
}

// All connect paths take a ctx so the caller (initial connect: a timeout+signal
// ctx; reconnect: rc.ctx) can abort a hung dial. The internal dial ALSO sets a
// dialTimeout on net.Dialer + ssh.ClientConfig.Timeout so a silently-dropped
// (firewalled) host fails in ~10s even if the caller's ctx has no deadline.
func New(ctx, Config) (*Tunnel, error)             // dial TCP to host:sshport (bounded), SSH handshake + auth, host-key verify
func (*Tunnel) Discover() (ResolvedTarget, error)  // exec `cat ~/.gogent/daemon.addr`; parse; fallback default sock
func (*Tunnel) DialContext(ctx, network, addr) (net.Conn, error)  // dispatch on resolved target (snapshots client under mu)
func (*Tunnel) Restart(ctx) (redialed bool, err error)            // PROBE-then-redial (see below); redialed=false on live-session skip
func (*Tunnel) Close() error
```

**`New` connect is bounded (fixes "fail fast", criterion #2 / §2.5).** `New` builds the
TCP conn with `net.Dialer{Timeout: dialTimeout}.DialContext(ctx, …)` and sets
`ssh.ClientConfig.Timeout = dialTimeout`. A firewalled/black-holed host therefore
errors in ~10s (not the ~75–130s OS TCP timeout), and a caller ctx cancel (signal /
shutdown) aborts immediately — never an indefinite hang before the UI.

**`Restart(ctx)` probes before it tears down (fixes "every reconnect kills a live
session", critique r1-#3).** Most stream drops (daemon graceful restart, transient blip,
the health monitor's 2-fail trip, server idle-close) leave the SSH session perfectly
healthy. So `Restart` first sends a cheap liveness probe on the existing client —
`client.SendRequest("keepalive@openssh.com", true, nil)`:
- probe **succeeds** → session is fine → return nil immediately (no redial, no
  re-auth, no re-Discover, `client`+`target` unchanged). "Retry now" then costs only an
  `openStream`, as fast as the `unix`/`http` path. `Restart` signals the caller *no
  redial happened* (returns `(redialed=false, nil)` or sets a flag the reconnect loop
  reads) so §2.4 can skip the now-pointless `CloseIdleConnections` (item r2-#4).
- probe **fails / no client** → the session is genuinely dead → close it and redial +
  re-auth + re-`Discover`, replacing `client`+`target`, and returns `redialed=true`.

Two tuning constants and a locking rule make this responsive (items r2-#2, r2-#3):
- `probeTimeout = 2 * time.Second` — the probe's **own** short deadline, distinct from
  `dialTimeout`. A half-open session (peer gone, no FIN) thus trips into redial in ~2s,
  not 10s, so "Retry now" never stalls on a wedged probe.
- **`mu` is held only for the probe and the final `client`/`target` swap, NOT across the
  ~10s network redial.** `Restart` snapshots the old client under `mu`, releases it,
  probes; on failure it dials/auths/Discovers *without* the lock, then re-takes `mu`
  only to publish the new `client`+`target` (and close the old). This keeps a concurrent
  `DialContext` (and `pollApprovals`, which dials every 750ms — `remote_handlers.go:53`)
  from blocking up to 10s behind a redial. The brief unlocked window is safe: a
  `DialContext` that grabs the soon-to-be-replaced client just fails and feeds the
  existing reconnect backoff.

`Restart` honors `ctx`: a `ctx.Done()` during the probe or redial returns promptly, so
shutdown and (via the reconnect loop, §2.4) "Retry now" interrupt it. The redial dial is
bounded by `dialTimeout` (as `New`).

**Re-Discover scope (item r2-#5, correcting a mis-statement).** Re-`Discover` runs only
on the *redial* path, and it is *not* required for correctness in the common case:
daemon paths are deterministic (`~/.gogent/daemon.sock`, fixed `--http-port`), so the
probe-skip path correctly reuses `t.target`. Known accepted edge: a mid-attachment
daemon restart that *changes* `--http-port` is only picked up once the SSH session itself
dies and forces a redial — rare, and out of scope for v1.

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

**Move the cancelable `ctx` + signal handler to the TOP of `runAttached`** (today they
are created at `:131`/`:158`, *after* `NewAPIClient`/`Health`/`rc.Start`). This is
required so the *initial* SSH connect — a synchronous, potentially slow operation — is
both bounded and interruptible by Ctrl+C, instead of relying on the shell's default
SIGINT disposition (critique r1-#4). The existing later-stage code that used `ctx` is
unaffected; we are only widening its scope.

**Critical: `sigChan` must keep exactly ONE consumer (item r2-#1).** Today (`:158-164`)
the final `select` is the *only* `sigChan` reader, and `httpShutdownCh` is the single
shutdown funnel fed by the TUI-loop goroutine (`:145-154`). The naive "add a goroutine
that does `<-sigChan; cancel()`" would create a *second* reader — and since a channel
value goes to exactly one receiver, a Ctrl+C after the TUI is up would nondeterministically
hit the goroutine (which cancels `rc.ctx` but never unblocks the final `select`, since
`wb.Run()` isn't driven by `rc.ctx`), forcing a **second** Ctrl+C to quit and losing the
"detaching…" line. That is a regression on the untouchable `unix`/`http`/`https` paths.

**Fix — the single signal consumer both cancels AND forwards into the existing funnel,
mirroring the TUI-loop goroutine at `:145-154`:**

```go
func runAttached(homeDir, addr, token string, noColorFlag bool) error {
    ctx, cancel := context.WithCancel(context.Background())   // MOVED UP from :131
    defer cancel()
    sigChan := make(chan os.Signal, 1)                        // MOVED UP from :158
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    var sigSeen atomic.Value                                  // carries the signal for the detach message
    go func() {                                               // the ONLY sigChan consumer
        sig := <-sigChan
        sigSeen.Store(sig)
        cancel()                                              // aborts a hung *initial* connect / SSE start
        select { case httpShutdownCh <- struct{}{}:           // SAME funnel as the TUI-loop goroutine (:145-154)
                 default: }                                   // ...so a single Ctrl+C always unblocks the final wait
    }()

    var apiOpts []tuipkg.APIClientOption
    var tunnel *sshtunnel.Tunnel
    if strings.HasPrefix(addr, "ssh://") {
        cfg, perr := parseSSHConfig(addr, token, *sshKey, *sshKnownHosts, *sshInsecure)
        if perr != nil { return fmt.Errorf("bad ssh connect %q: %w", addr, perr) }
        connectCtx, c := context.WithTimeout(ctx, sshtunnel.DialTimeout)  // bounded + signal-cancelable
        tunnel, err = sshtunnel.New(connectCtx, cfg)          // dial + auth + host-key verify (fail fast)
        c()
        if err != nil { return fmt.Errorf("ssh connect %s: %w", cfg.Host, err) }
        if _, derr := tunnel.Discover(); derr != nil {        // read daemon.addr (actionable msg)
            tunnel.Close(); return fmt.Errorf("resolve daemon at %s: %w", cfg.Host, derr)
        }
        apiOpts = append(apiOpts, tuipkg.WithDialContext("http://ssh", tunnel.DialContext))
    }
    client, err := tuipkg.NewAPIClient(addr, token, apiOpts...)
    ...
    // existing client.Health() (was :69) now probes over the tunnel; §2.5 wraps the
    // no-daemon case into an actionable error. The HTTP request carries its own
    // per-call timeout, so Health can't hang behind a dead tunnel either.
    ...
    rc := tuipkg.NewRemoteClient(client, wb.EmitSessionEvent, wb)
    if tunnel != nil { rc.SetTunnel(tunnel) }    // give reconnect a Restart handle (§2.4)
    ...
    // Final wait: BOTH a Ctrl+C and a normal TUI quit now arrive via httpShutdownCh —
    // the signal goroutine above forwards into it, the TUI-loop goroutine (:145-154)
    // already does. One consumer, one funnel, single-press exit on every scheme.
    <-httpShutdownCh
    if sig := sigSeen.Load(); sig != nil {
        fmt.Printf("\nReceived signal %v, detaching...\n", sig)
    }
    // teardown (was :166)
    rc.Close()
    if tunnel != nil { tunnel.Close() }          // session + listener gone; daemon keeps running
}
```

Why this is regression-free on the existing paths: the final `select { case sig :=
<-sigChan; case <-httpShutdownCh }` (`:160-164`) collapses to a single `<-httpShutdownCh`
wait. A Ctrl+C — whether it lands during the initial connect or after the TUI is up —
goes to the **one** `sigChan` reader, which cancels `ctx` *and* sends to `httpShutdownCh`,
so the wait unblocks on the **first** press and still prints the detach line (from
`sigSeen`). A normal TUI quit feeds `httpShutdownCh` exactly as today. No second reader,
no scheduler-dependent double-press. (`sigChan` stays buffered cap-1 so the `Notify`
delivery never blocks; the goroutine reads once, which is all we need — process exits
after teardown.)

`local := strings.HasPrefix(addr, "unix://")` (:114) is already correct: `ssh://`
→ `false` → `DaemonModeAttachedRemote` → daemon menu shows "Daemon status" only, no
local Start/Stop. Optional polish: label the remote with the SSH host.

### 2.4 `ui/tui/remote_handlers.go` — Restart on reconnect

Add a tiny interface + optional field; do **not** make RemoteClient own Close of the
tunnel (runAttached owns that — single owner):

`Restart` returns whether it actually redialed, so the caller only flushes the HTTP pool
when the underlying `*ssh.Client` changed (item r2-#4 — no pointless flush on probe-skip):

```go
// redialed=false on the probe-skip (live-session) path; true when client/target were replaced.
type TunnelRestarter interface{ Restart(context.Context) (redialed bool, err error) }
// new field on RemoteClient:  tunnel TunnelRestarter
func (rc *RemoteClient) SetTunnel(t TunnelRestarter) { rc.tunnel = t }
```

In `reconnect()` (:297), at the top of the loop *after* `notifyLost(attempt)` and the
backoff `select` (which is where `RetryNow` is honored, :300-305), *before*
`openStream()`:

```go
if rc.tunnel != nil {
    redialed, err := rc.tunnel.Restart(rc.ctx)   // PROBE-then-redial (§2.1), ctx-cancelable, dial-bounded
    if err != nil {
        continue   // SSH genuinely down → next attempt, longer backoff, modal stays up
    }
    if redialed {
        rc.client.CloseIdleConnections()  // only when the session was actually replaced (item r2-#5/#4, critique r1-#5)
    }
}
next, err := rc.openStream()
```

Three properties this gives us, each closing a critique item:

- **`Restart(rc.ctx)` is bounded and cancelable** (critique r1-#2). The dial inside is
  capped at `dialTimeout` (probe at `probeTimeout`), so a hung re-dial returns control to
  the backoff `select` in ~10s where `RetryNow` is honored again — "Retry now" is no
  longer a multi-minute no-op. And `Close()` (which cancels `rc.ctx`) interrupts an
  in-progress `Restart` immediately, so shutdown never blocks on a wedged session.
- **Live sessions are *not* torn down** (critique r1-#3). `Restart` probes first and
  returns `(false, nil)` without redialing when the SSH session is healthy (the common
  case — the stream dropped, not the tunnel). So the steady-state reconnect cost stays
  ~one `openStream`, matching the `unix`/`http` path, and `mu` is not held across any
  slow dial (§2.1) so `pollApprovals` is never stalled.
- **Stale pooled channels are dropped — only when actually stale** (critique r1-#5, item
  r2-#4). Unlike the `unix` dialer, the SSH tunnel can swap its underlying `*ssh.Client`
  on a redial, leaving the `http.Transport` pool holding channels bound to the dead
  session. A non-idempotent `POST` (e.g. `/sessions/{id}/stop`, `/approvals/{id}/decision`)
  landing on such a channel would error (net/http only auto-retries idempotent requests).
  It is benign for current callers (they log-and-ignore), but we call
  `APIClient.CloseIdleConnections()` (a thin wrapper over `c.http.CloseIdleConnections()`,
  added alongside `WithDialContext`) right after a `Restart` **that reports `redialed=true`**
  so the pool is rebuilt on the live session — the probe-skip path leaves the pool intact
  (no waste). Nil-tunnel paths skip the whole branch → no behavior change for
  `unix`/`http`/`https`.

On success, `openStream` → `notifyRestored` → `kickApprovals` (jump-to-present) fire
exactly as today. No second reconnect state machine.

### 2.5 Fail-fast errors (usability gate)

Every case below errors within ~`dialTimeout` (≤10s) — **bounded, not the OS TCP
timeout** — and before any UI is constructed → never an empty TUI, never a multi-minute
hang:

- **Unreachable / firewalled / refused host** → `net.Dialer{Timeout}` + `ClientConfig.Timeout`
  in `sshtunnel.New` → `"ssh connect <host>: dial tcp …: i/o timeout"` (or connection
  refused) in ≤10s. *This is the case the previous draft left unbounded.*
- **Auth failure** → from `New` → `"ssh connect <host>: ssh: handshake failed: …"` (with a passphrase/agent hint).
- **Host-key mismatch / unknown host** → from `New` → fingerprint + `"add it to known_hosts (ssh-keyscan <host>) or pass --ssh-insecure-skip-verify"`.
- **No daemon running** → `Discover` falls back to the default socket; the existing
  `Health()` (was `cmd/attach.go:69-71`) then fails over the tunnel. Wrap that case for
  the ssh path: `"no daemon found at ssh://<host> — start it with: gogent daemon start"`.
  (`Health` issues a normal HTTP request with its own per-call timeout, so it cannot
  hang behind a half-open tunnel.)
- **SIGINT during the initial connect** → the moved signal goroutine (§2.3) cancels the
  connect ctx → `New` returns promptly and `runAttached` exits cleanly.

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
auth/host-key behavior, the new flags, and full disconnect-recovery (#481 has landed).
Two specific notes to include:
- **Token is usually inert over SSH** (see §2.9): because the tunnel lands on the
  daemon's Unix socket (or its loopback `--tcp` listener), the daemon already treats the
  caller as the local human — `--token` only matters when attaching to a *non-loopback*
  `--tcp` daemon. Tell users they normally don't need a token for `ssh://`.
- **First connect to a new host fails on strict known_hosts** (open Q3): there is no
  interactive trust-on-first-use prompt; document the `ssh-keyscan <host> >> ~/.ssh/known_hosts`
  (or `--ssh-insecure-skip-verify`) step so the first-connect error is not surprising.

### 2.9 Auth model over the tunnel (criterion #1 nuance, from the critique)

`internal/server/auth.go:92` grants local-human scope when
`isLoopback(RemoteAddr) || isUnixRequest(r)`, and `isUnixRequest` (:160-165) keys on the
*listener* being a Unix socket (`la.Network()=="unix"`), **not** on peer credentials.
Consequences for the SSH tunnel, all verified:
- **Socket-only daemon** (no `--tcp`): a `direct-streamlocal` channel lands on the
  daemon's Unix listener → `isUnixRequest` true → local human, **token not required**.
  This is exactly what makes the "no `--tcp` needed" headline work, and it is *not* a
  security hole: reaching the socket at all already required authenticating to the host
  over SSH, and the socket is `0600`/`sshd`-user-owned — the same trust model as a local
  attach.
- **Loopback `--tcp` daemon** (`http://127.0.0.1:port`, e.g. Windows primary): the
  `direct-tcpip` channel arrives with a loopback `RemoteAddr` → `isLoopback` true →
  local human, token also inert.
- **Non-loopback `--tcp` daemon**: not loopback, not unix → the bearer token (or
  password cookie) is the real gate, carried as `Authorization: Bearer <token>` over the
  tunnel exactly as a manual TCP attach.

So the design's "token authenticates over the tunnel" is precise only for the
non-loopback `--tcp` case; for the common socket / loopback paths the token is accepted
but unnecessary. The flag still works everywhere (an unknown-but-present token on a
unix/loopback request is simply never consulted), so passing one is harmless.

---

## 3. The four gates

**(1) GOAL MATCH.** Exactly the issue's ask: one command attaches the TUI to a remote
daemon; auto-resolves a daemon started with plain `gogent daemon start` (socket-only,
no `--tcp`) by reading `daemon.addr` over SSH; `--tcp` daemon also supported (2nd
token preferred); `?port=`/`?socket=` overrides; no manual `ssh -L`. The socket-only
headline genuinely works because the daemon's auth gate scopes a unix-listener request
as local human (§2.9) — the token is the gate only for a non-loopback `--tcp` daemon,
where it is carried as `Bearer` over the tunnel as before. No scope creep — Phase-3
(watcher mgmt / archived sessions over the wire), remote daemon auto-start, and
daemon-side TLS are explicitly out.

**(2) USABILITY.** User drives input via a single `--connect ssh://…` URL + standard
`--token`/`GOGENT_HTTP_TOKEN` + optional `--ssh-*` flags. Every failure mode
(unreachable / auth / host-key / no daemon / SIGINT) fails fast with an actionable
message **in ≤`dialTimeout` (~10s), never the ~75s OS TCP timeout** — every connect is
bounded by `net.Dialer{Timeout}`+`ssh.ClientConfig.Timeout` and cancelable by the
moved signal/ctx (§2.3, §2.5) — and before the TUI, so never a blank screen. SSH drop
raises the **existing** disconnect modal; "Retry now" stays responsive because
`Restart(ctx)` (a) probes the live session and skips redial when it is healthy —
the common case — so the reconnect costs ~one `openStream`, and (b) is bounded +
ctx-cancelable so even a genuinely-dead session returns control to the backoff `select`
(where `RetryNow` is honored) in ~10s instead of blocking it indefinitely (§2.4).
After a successful restart the stale-channel pool is flushed (`CloseIdleConnections`)
so the next non-idempotent `POST` lands on the live session. Exit closes the SSH
session; the daemon keeps running (detach never stops it). Remote daemon menu correctly
hides local Start/Stop.

**(3) NO REGRESSIONS.** `unix`/`http`/`https` paths are untouched: `NewAPIClient`'s
variadic opts are ignored by those cases; `RemoteClient.tunnel` is nil for them so the
new `reconnect` branch is skipped; `resolveMode`, classification, teardown order all
unchanged. New behavior is purely additive (`case "ssh"`, a nil-guarded field, a new
package). Risks + mitigations:
- *`NewAPIClient` signature change* → variadic, so all ~27 existing 2-arg call sites
  (incl. `cmd/handoff.go:184`) compile unchanged; only the `ssh://` path passes opts.
- *Concurrent `DialContext` vs `Restart` swapping `*ssh.Client`* → guard `client`+`target`
  with `Tunnel.mu`; `DialContext` snapshots both under the lock. The `APIClient` holds
  `tunnel.DialContext` as a method value, so it transparently picks up the post-`Restart`
  client/target.
- *Stale pooled channels after a `Restart` redial* (the one semantic difference from
  `unix`, which never swaps its transport) → `CloseIdleConnections()` after a successful
  restart (§2.4) rebuilds the pool on the live session; benign for current callers
  regardless (they log-and-ignore the rare stale-conn `POST` error).
- *Per-request channel cost* → `http.Transport` pooling reuses channels, same as unix.
- *Moved signal handler (the round-2 catch)* → the relocated `ctx`+`sigChan` keep
  **exactly one** `sigChan` consumer, which both `cancel()`s and forwards to
  `httpShutdownCh` (the existing funnel). So a single Ctrl+C still exits on the first
  press and prints the detach line on every scheme — verified by the new test below.
  Without this, a second `sigChan` reader would race the final `select` and intermittently
  require a double Ctrl+C on `unix`/`http`/`https` (§2.3).
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
| `ui/tui/api_client.go` | `case "ssh"` in `NewAPIClient`; variadic `APIClientOption` + `WithDialContext`; `APIClient.CloseIdleConnections()` wrapper; update `default:` error string. |
| `cmd/attach.go` | `runAttached`: **move `ctx`+signal handler to the top with a single `sigChan` consumer that `cancel()`s AND forwards to `httpShutdownCh`** (preserves single-press exit on all schemes); parse `ssh://`, build (bounded `New`) + `Discover` tunnel before `NewAPIClient`, inject dialer, `SetTunnel`, close tunnel after `rc.Close()`, wrap no-daemon Health error. |
| `ui/tui/remote_handlers.go` | `TunnelRestarter` (`Restart(ctx) (bool, error)`) field + `SetTunnel`; `Restart(rc.ctx)` + conditional `CloseIdleConnections()` (only on `redialed`) at top of `reconnect` loop. |
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
- `reconnect` calls `tunnel.Restart(ctx)` before `openStream` (inject a fake `TunnelRestarter`),
  and calls `CloseIdleConnections` **only when `Restart` reports `redialed=true`** (assert
  it is NOT called on the probe-skip path).
- **Bounded dial**: `New`/`Restart` against a host that accepts the TCP conn but never
  completes the SSH handshake (or a `net.Pipe`/blackhole address) returns an error within
  `dialTimeout`, not the OS default — assert wall-clock < dialTimeout+ε.
- **Probe-skips-redial**: `Restart(ctx)` on a still-live test SSH session returns
  `(false, nil)` *without* opening a new client (assert the test server's accept-count is
  unchanged); on a closed session it returns `(true, nil)` and redials (accept-count
  increments).
- **Probe deadline**: `Restart(ctx)` against a half-open session (keepalive never
  answered) trips into redial within ~`probeTimeout` (~2s), well under `dialTimeout`.
- **`mu` not held across redial**: a `DialContext`/probe call concurrent with a slow
  `Restart` redial is not blocked for the full dial (assert it returns/errs promptly).
- **Cancelable Restart**: a `Restart(ctx)` whose `ctx` is cancelled mid-dial returns
  promptly with `ctx.Err()`.
- **Single-press exit (regression guard, item r2-#1)**: with the §2.3 signal wiring, one
  SIGINT both cancels `ctx` and unblocks the final `<-httpShutdownCh` wait on a
  non-ssh (`unix`) attach — assert `runAttached` returns after exactly one signal and the
  detach line is printed. (Guards against re-introducing a second `sigChan` consumer.)
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
