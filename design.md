# Design — gogent #502: SSH attach fails with "no supported methods remain" when ssh-agent holds 0 keys but a valid IdentityFile key is loaded

## Summary of the bug

`internal/sshtunnel/tunnel.go::authMethods` (L529) builds **two** separate
`ssh.AuthMethod`s that both report `method() == "publickey"`:

- `agentAuth()` (L667) → `ssh.PublicKeysCallback(agent.NewClient(conn).Signers)`
- `keyAuth(cfg)` (L683) → `ssh.PublicKeys(signers...)`

`golang.org/x/crypto/ssh`'s `clientAuthenticate` de-dupes *candidate* auth
methods **by name**: once a method named `publickey` has been tried it appends
`"publickey"` to `tried` and skips any later method with the same name
(`slices.Contains(tried, candidateMethod)`). So when the agent method runs first
and the agent is **present but empty (0 keys)**, the `publickey` slot is marked
tried and the **second** `publickey` method — the loaded `--ssh-key` /
`IdentityFile` key — is **silently dropped**. The accepted key is never offered
and the handshake dies with:

```
ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain
```

…even though the key is loaded and plain `ssh <host>` works.

Reordering (files-before-agent) is **not** a fix: it only mirrors the bug — an
unaccepted file key would then block a valid agent key. The robust fix is to
offer agent signers **and** file-key signers as candidates **within a single
`publickey` auth method**, so x/crypto/ssh's by-name de-dupe can never drop one.

## The fix (gogent only — `internal/sshtunnel/tunnel.go`)

Replace the two-method `agentAuth()` + `keyAuth()` split with **one** merged
`publickey` `AuthMethod` whose signer callback returns the de-duped concatenation
of agent signers and loaded file-key signers.

### Functions touched / added in `tunnel.go`

1. **`authMethods(cfg Config) ([]ssh.AuthMethod, error)`** — rewritten to return a
   single `ssh.PublicKeysCallback`:

   ```go
   func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
       // File-key signers are loaded eagerly here (as keyAuth did before): this
       // is where loadSigner's TTY passphrase prompt fires, and it lets us decide
       // the "nothing to try" gate below without prompting twice. authMethods is
       // re-run on every dialClient (incl. Restart redials), so this is not a
       // one-time snapshot — a redial re-loads the files just as before.
       fileSigners := fileSigners(cfg)

       // Agent conn: dialed ONCE here, captured by the callback, and queried
       // LAZILY (agent.NewClient(conn).Signers()) INSIDE the callback during the
       // handshake — so a Restart redial (which re-runs authMethods and re-dials
       // the agent) picks up keys added to the agent after the tunnel opened.
       agentConn := dialAgent() // net.Conn, or nil if no reachable SSH_AUTH_SOCK

       // Preserve the existing gate: an unreachable agent AND zero loadable file
       // keys is the only "nothing to try" case. A reachable-but-empty agent is
       // NOT an error here (same as today: agentAuth returned non-nil regardless
       // of key count) — the handshake proceeds and, if nothing is accepted, fails
       // with the server's message enriched by authDiagnostic.
       if agentConn == nil && len(fileSigners) == 0 {
           return nil, errors.New("no ssh auth available: start an ssh-agent (SSH_AUTH_SOCK) or pass --ssh-key")
       }

       return []ssh.AuthMethod{
           ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
               var signers []ssh.Signer
               // Agent first (queried lazily, per handshake), then file keys —
               // preserving the pre-fix order (agentAuth was appended before
               // keyAuth) and OpenSSH's "agent identities before on-disk keys"
               // spirit. De-dupe by marshaled public-key blob so a key present in
               // both the agent and a file is offered once; agent-first means the
               // agent's (already-unlocked) copy wins the dup.
               if agentConn != nil {
                   if as, err := agent.NewClient(agentConn).Signers(); err == nil {
                       signers = append(signers, as...)
                   }
               }
               signers = append(signers, fileSigners...)
               return dedupeSigners(signers), nil
           }),
       }, nil
   }
   ```

2. **`dialAgent() net.Conn`** — extracted from the old `agentAuth()`: dials
   `SSH_AUTH_SOCK` (keeping the existing `//nolint:gosec` rationale) and returns
   the `net.Conn`, or `nil` if `SSH_AUTH_SOCK` is unset / undialable. (The old
   `agentAuth` wrapped this in `PublicKeysCallback`; that wrapping moves into the
   merged callback.)

3. **`fileSigners(cfg Config) []ssh.Signer`** — the body of the old `keyAuth()`
   minus the `ssh.PublicKeys(...)` wrap: iterate `keyPaths(cfg)`, `loadSigner(p)`,
   collect non-nil. `keyPaths` and `loadSigner` (incl. the passphrase prompt) are
   **unchanged**.

4. **`dedupeSigners(in []ssh.Signer) []ssh.Signer`** — new helper; de-dupe by
   `string(s.PublicKey().Marshal())`, preserving first-seen (agent-first) order.
   Mirrors the existing `dedupPaths` helper. Each duplicate offer otherwise costs
   a separate attempt against the server's `MaxAuthTries`.

5. **Remove** `agentAuth()` and `keyAuth()` (fully replaced — no dead code, no
   lint failures). The doc comment on `authMethods` (L526–528) is updated to
   describe the single merged method.

`keyPaths` (and its `--ssh-key` / `IdentityFile` / `IdentitiesOnly` precedence
from #498), `loadSigner`, `promptPassphrase`, `classifyKey`, `agentSummary`,
`authDiagnostic`, `dialClient`, host-key verification, `Config`, and everything
in `Discover`/`DialContext`/`Restart`/`Close` are **untouched**.

### The agent-connection trade-off (RESOLVED — long-lived conn is REQUIRED)

The lazy query must use a connection that **stays open until the handshake's auth
exchange completes**, because the signers `agent.Signers()` returns
(`agentKeyringSigner{client, pubkey}`) sign by calling **back over that same
connection** (`Sign → s.agent.Sign(...)`), and that signing happens **after** the
`PublicKeysCallback` returns — x/crypto/ssh first lists candidate public keys,
then asks the chosen signer to sign. Therefore:

- **(A) Dial once in `authMethods`, capture the conn in the closure** — CHOSEN,
  and required. Matches the pre-fix `agentAuth` ("conn stays open for the tunnel's
  lifetime"). The conn is captured by the auth-method closure, which lives through
  `ssh.NewClientConn`; after the handshake it becomes unreferenced and the fd
  finalizer reclaims it. One agent conn per `dialClient`/redial. Every redial
  re-runs `authMethods` → re-dials → re-queries live agent keys.

- (B) Re-dial the agent socket *inside* the callback and close it right after
  `Signers()` — **REJECTED as incorrect.** Closing the conn after listing breaks
  agent-key signing (the signer can no longer reach the agent), so agent
  authentication would fail. This is not a viable follow-up cleanup; it is a bug.

The required invariant holds either way it is sliced: **keys added to the agent
after the tunnel opened are picked up on reconnect**, because
`Restart → dialClient → authMethods` re-dials the agent and the callback re-lists.

> Implementation note for the tester: the laziness mechanism is "fresh conn +
> fresh `agent.NewClient` per handshake/redial." Do **not** unit-test it by
> calling `agent.NewClient(conn)` twice on the *same* `net.Conn` — each
> `agent.NewClient` spawns its own reader goroutine, and two readers racing on one
> socket desync the agent wire protocol and deadlock. Exercise reconnect-requery
> through `New` + `Restart` (fresh conn per dial), which is how production runs it.

## User-facing behavior

- `gogent --connect ssh://<host>` now authenticates whenever **any** candidate
  key (agent **or** file) is accepted — matching `ssh <host>`.
- An empty ssh-agent never blocks a valid `IdentityFile` / `--ssh-key`.
- An unaccepted file key never blocks a valid agent key.
- The "no ssh auth available: start an ssh-agent (SSH_AUTH_SOCK) or pass
  --ssh-key" error is unchanged (same message, same trigger: no agent **and** no
  loadable file key, before any network I/O).
- The enriched auth-failure diagnostic (`authDiagnostic`) is unchanged — it reads
  `keyPaths(cfg)` + `agentSummary()`, never the method list, and leaks no key
  contents. (Optional, low-risk polish, not required by acceptance: append
  "offered N candidate keys across agent+files". Deferred to stay surgical; the
  current diagnostic is already accurate.)

## The 4 design gates

**(1) Goal match.** Exactly the issue's ask: a *fix*, not a feature/refactor.
The single behavioral change is packaging agent + file signers into one
`publickey` method so x/crypto/ssh's by-name de-dupe can't drop the loaded key.
No scope creep (no CLI flags, no new config, no transport changes).

**(2) Usability.** The right thing is surfaced, not silent: a loaded
`IdentityFile`/`--ssh-key` is always offered even with an empty agent; auth
either succeeds or fails with the existing enriched, contents-safe diagnostic.
Agent laziness / reconnect-requery preserved. The "nothing to try" error
preserved verbatim so the actionable hint still appears. The user drives input
exactly as before (`--ssh-key`, `~/.ssh/config`, `SSH_AUTH_SOCK`).

**(3) No regressions.** `keyPaths` precedence (#498), `loadSigner` passphrase
prompt, host-key verification, Discover/Dial/Restart/Close all untouched.
Existing tests that must keep passing: `TestNew_AuthFailure`,
`TestNew_AuthFailureDiagnostic`, `TestNew_AuthFailureDiagnosticFieldsAlwaysPresent`,
`TestKeyPaths`, all `TestParseConnectURL_*` / `TestReadSSHConfig*`,
`TestEndToEnd_*`, `TestRestart_*`, host-key tests. The merged method offers the
same signer set the two methods did (agent signers ++ file signers), just in one
`publickey` conversation, so accepted-key cases that worked still work; the only
*newly*-working cases are the two the issue describes. gofmt/build/vet,
golangci-lint (0 new), `go test ./...` green per the dev gate (no `-race` on Pi5;
pre-existing `TestUserSessionSendMessage` 404 is the only acceptable failure).

**(4) Holistic across both repos.** Change is in the correct place — the SSH
client auth construction in `internal/sshtunnel/tunnel.go`. **turbotui is not
touched**: it only consumes the tunnel's `DialContext` (a `net.Conn` factory) and
is blind to how SSH auth is assembled; the repo seam (gogent owns the SSH
transport, turbotui owns the TUI consuming `DialContext`) is respected. **No
`go.mod` bump, no new dependency** — reuses `golang.org/x/crypto/ssh` and
`.../ssh/agent` already imported.

## Tests — `internal/sshtunnel/tunnel_test.go` (partner writes these)

New helper (needs a new import: `golang.org/x/crypto/ssh/agent`):

- **`startTestAgent(t, keys ...*rsa.PrivateKey) sock`** — `agent.NewKeyring()`,
  `keyring.Add(agent.AddedKey{PrivateKey: k})` per key, listen on a `t.TempDir()`
  unix socket, `go agent.ServeAgent(keyring, conn)` per accept, `t.Cleanup` to
  close, `t.Setenv("SSH_AUTH_SOCK", sock)`. Empty `keys` → reachable-but-empty
  agent (the bug's trigger). Returning the `keyring` lets a reconnect test add a
  key mid-run.
- Expose the env's accepted client private key: add field `clientPriv *rsa.PrivateKey`
  to `sshTestEnv` (it already generates `clientPriv`), so a test can load *the
  accepted key* into the fake agent.

Required regression cases (use `t.TempDir()` / in-process fakes — never touch real
`~/.ssh` or a real agent):

1. **empty agent + accepted file key → SUCCEEDS** (the reported bug; fails pre-fix).
2. **unaccepted file key + accepted agent key → SUCCEEDS** (mirror invariant).
3. **control: accepted file key alone (no agent) → succeeds.**
4. **zero candidates (no agent, no file key) → `"no ssh auth available"` error**
   (unit-test `authMethods` directly; `IdentitiesOnly:true` + bogus `KeyPath`).

Recommended extras: agent-first offer-order assertion (both keys offered in one
handshake), same-key-in-agent-and-file de-dupes, reconnect picks up a
newly-added agent key (`New` → add key to agent → kill client → `Restart` →
`healthOver`), `dedupeSigners` unit test, `authMethods` returns exactly one
method. **Caveat (see trade-off note):** test reconnect-requery via `Restart`
(fresh conn), never by double `agent.NewClient` on one conn.

## Regression risks & mitigations

- **MaxAuthTries pressure / offer order.** One `publickey` method offering N
  signers issues a pubkey *query* per candidate; agent-first means many junk agent
  keys can exhaust the server's `MaxAuthTries` before a good file key is reached.
  This is an inherent SSH ceiling (pre-fix this also failed), not a new
  regression; `IdentitiesOnly` (preserved) is the escape hatch. The merged method
  removes the redundant second-method "none" overhead, so try-count does not
  increase for the cases that mattered.
- **Passphrase prompt timing.** `loadSigner` prompts during `fileSigners(cfg)`,
  i.e. at `authMethods` time — exactly as `keyAuth` did before. Re-prompt-per-redial
  behavior is identical to today.
- **Agent conn lifetime.** Unchanged from pre-fix `agentAuth`; bounded (fd
  finalizer reclaims it after the handshake), and required for agent signing — see
  the resolved trade-off above. Not worsened.
- **Dedup correctness.** Keying on `PublicKey().Marshal()` is the canonical SSH
  identity for a key; agent-first ordering makes the unlocked agent copy win a
  tie, the preferable signer.
- **Swallowed agent `Signers()` error.** If the agent errors, the callback skips
  agent signers and falls through to file keys (more resilient than pre-fix, which
  aborted). The failure path remains covered by `authDiagnostic`.

## Open questions

1. **Diagnostic polish.** Append "offered N candidate keys across agent+files" to
   `authDiagnostic`? Not in the acceptance criteria; deferred to keep the change
   surgical. Easy to add if reviewers want it.

(The earlier "agent conn cleanup" open question is now resolved — the long-lived
conn is required for agent signing, so trade-off (A) is the only correct choice;
there is no leak-free follow-up. Both this and the remaining question are confined
to `internal/sshtunnel/tunnel.go`, no new deps, turbotui untouched.)
