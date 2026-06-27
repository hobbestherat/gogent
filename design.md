# Design — issue #507: Attached TUI is a pure daemon frontend (default-model & notification split-brain)

## Summary of the bug

In attached/remote mode (`gogent --connect …`, `ssh://`, or local-socket auto-attach),
`cmd/attach.go::runAttached` builds a **full local** `*gogent.Gogent` via
`gogent.NewGogent(homeDir)` purely as a presentation-config source. Most UI state
correctly talks to the daemon over HTTP (`RemoteClient.Handlers()` in
`ui/tui/remote_handlers.go`), but two fields are still served from — and persisted
back to — the **client** machine's `~/.gogent/config.json`:

1. **default model** — `installPresentationHandlers` wires
   `GetDefaultModel → g.DefaultModelName()` and
   `SetDefaultModel → g.SetDefaultModel()` (which calls `g.SaveConfig()`, writing the
   client's `config.json`). But the model dropdown is built from the **daemon's**
   models (`remoteModelConfigs(client)`). So `Workbench` resolves the client's
   default-model name against the daemon's list (`modelIndexByName`, `ui/tui/tui.go:1506`)
   and silently falls back to index 0 when the name is absent; "Set as default"
   (`model_editor.go`, `model_catalog_dialog.go`) edits the **wrong machine**.
2. **notifications** — `GetNotifyConfig/SetNotifyConfig` and the
   `wb.SetNotifyConfig(g.Notifications())` seed in `runAttached` all use the client
   config, while the daemon keeps its **own** notify block at
   `/api/settings/notifications` used only for daemon-side fallback. The two drift
   silently and it's undocumented which side owns what.

Budget is the consistency target: it is already daemon-owned
(`runAttached` seeds `wb.SetBudgetConfig(s.Budget)` from `client.GetSettings()`;
`RemoteClient` GetBudget/SetBudget round-trip `/api/settings`).

## Chosen design

### 1) Default model becomes daemon-owned (PRIMARY FIX)

Add a `default_model` field to the **existing** `settingsView` (the `settingsView`
route already exists, mirrors budget exactly, and avoids a new endpoint/route/test
surface). Concretely:

- **`internal/server/wire.go`** — add `DefaultModel string \`json:"default_model"\``
  to `settingsView`.
- **`internal/server/resources.go`** — `settingsSvc.Get` returns
  `DefaultModel: svc.s.g.DefaultModelName()`. `settingsSvc.Set` applies the
  default-model change **first, before any other setter** (fail-fast), and **only
  when** `req.DefaultModel != ""` **and** it differs from the current value (so an
  unrelated PUT — e.g. a budget change, or an older client that omits the field —
  never re-validates or clears it):

  ```go
  if req.DefaultModel != "" && req.DefaultModel != svc.s.g.DefaultModelName() {
      if err := svc.s.g.SetDefaultModel(req.DefaultModel); err != nil {
          // g.SetDefaultModel returns a bare fmt.Errorf("model %q not found")
          // (gogent.go:3400). A bare error maps to HTTP 500 in webapi
          // (webapi.go: http.Error(..., StatusInternalServerError)); an invalid
          // model name is user-correctable input, so wrap it as a 400 to match the
          // repo-wide pattern (resources.go:40/63/81, approvals_handlers.go:33).
          return nil, webapi.NewHTTPError(http.StatusBadRequest, err.Error())
      }
  }
  // ... only AFTER default_model validated/applied do the other setters run:
  svc.s.g.SetSubAgentSettings(req.SubAgents)
  svc.s.g.SetTimeouts(req.Timeouts)
  svc.s.g.SetBudget(req.Budget)
  if req.ReviewEdits != svc.s.g.ReviewEdits() { svc.s.g.SetReviewEdits(req.ReviewEdits) }
  ```

  Applying `default_model` first means a bad name fails the whole PUT **before** any
  other field is persisted — correct even for a non-RMW caller (a browser/script doing
  a full PUT with a changed budget *and* an invalid model gets no partial write). This
  removes any reliance on the RMW "no-op" rationalization.
  `webapi` and `net/http` are already imported in `resources.go`.
- **`ui/tui/api_client.go`** — add `DefaultModel string \`json:"default_model"\``
  to `SettingsDTO` (keeps the read-modify-write round-trip lossless, mirroring the
  server view).
- **`ui/tui/remote_handlers.go`** — in `RemoteClient.Handlers()` add:
  - `GetDefaultModel: func() string { s,err:=c.GetSettings(); if err!=nil {return ""}; return s.DefaultModel }`
  - `SetDefaultModel: func(name string) error { … read-modify-write returning the error }`.
    Budget's setter uses `rc.mutateSettings` (which *logs* and swallows the write
    error). `SetDefaultModel` must **return** the error (its signature is
    `func(string) error`, and `model_editor.go:299` / `model_catalog_dialog.go:477`
    surface it), so it does its own GET → set `DefaultModel` → `c.SetSettings(cur)`
    and returns that result. `APIClient.SetSettings`/`do` already turns a daemon
    non-2xx response into a Go error carrying the body (`api_client.go:215-217`), so
    the daemon's **400 "model not found"** (from the wrapped `NewHTTPError` above)
    propagates to the editor dialog. (Minor DRY note: this inlines a GET→mutate→PUT
    rather than adding an error-returning sibling of `mutateSettings`; that is
    deliberate, because the two have opposite error semantics — swallow vs return.)
- **`cmd/attach.go::installPresentationHandlers`** — **remove** the
  `h.GetDefaultModel`/`h.SetDefaultModel` local-`g` assignments. They now come from
  `rc.Handlers()` (installed before `installPresentationHandlers`, which no longer
  overwrites them). Update the function's doc comment to drop "default-model
  dropdown selection" from the list of client-owned concerns.
- **`docs/api.md`** — update the `settingsView` description to
  `{sub_agents, timeouts, budget, review_edits, default_model}`.

Why settingsView over a dedicated `/api/settings/default-model`: the issue lists the
settingsView field as PREFERRED; it reuses the existing route, mirrors budget 1:1
(budget likewise has **no** dedicated APIClient method — it rides `GetSettings`/
`SetSettings`), and needs no new api.go route wiring. The only nuances (HTTP status
for an invalid name; partial-write ordering) are handled by the wrapped
`NewHTTPError(400)` and the validate-first ordering above, and by
`RemoteClient.SetDefaultModel` doing an explicit error-returning RMW rather than the
silent `mutateSettings`.

### 2) Notification ownership — CLIENT-LOCAL (documented), the simplest defensible policy

The desktop bell / OSC / native notifier physically fires on the **client** machine.
The attached client already raises notifications for daemon-side completions via the
`notification` SSE frame (`client.SetNotificationHandler → wb.NotifyFromWire`), and
whether *this terminal* bells/sounds is legitimately the client user's choice. So:

- **Keep** `GetNotifyConfig/SetNotifyConfig` wired to the client config in
  `installPresentationHandlers`, and **keep** the `wb.SetNotifyConfig(g.Notifications())`
  seed in `runAttached`. **No behavioural change.**
- **Document** (comment block in `cmd/attach.go` + docs) that:
  - the **client** config's `notifications` block governs the client-side live
    notifier (this terminal's bell/sound/enabled), and
  - the **daemon's** `/api/settings/notifications` governs **only** daemon-side
    fallback (watcher completions / `NotifyLocalFallback` when no client is attached),
  - the attached client deliberately does **not** read or write the daemon's notify
    config, so it never *pretends* to control daemon notifications.

This resolves the "silent drift" by making the split explicit and intentional rather
than accidental. Notifications are therefore **not** a daemon-owned field for the
regression test (default_model is).

Rejected alternative (SYNCED): route the client's notifier through the daemon's
`/api/settings/notifications`. More moving parts, makes one machine's bell depend on
another's config, and contradicts the fact that the notifier hardware is local. Not
worth the risk for this fix.

### 3) Client no longer persists daemon-owned state

After the fix, an attached settings-only session that changes the default model
issues **only** an HTTP PUT to the daemon; the local `g.SetDefaultModel`/`SaveConfig`
path is gone, so the client's `config.json` is untouched for `default_model`.
`NewGogent` loads config but does **not** save on construction (verified:
`internal/gogent/gogent.go:189` loads; no SaveConfig in the constructor), so merely
attaching writes nothing. Covered by the regression test below.

### 4) (NICE-TO-HAVE) wasteful local loads — DOCUMENT ONLY

`NewGogent` also loads client `skills/`, `rules.json`, workspace/AGENTS.md/repo map,
none of which are used while attached (those handlers come from `RemoteClient`).
Carving out a "frontend-only" `NewGogent` mode is **out of scope / too risky** for
this fix (it would touch the constructor used by every entry point). I will instead
add a comment in `runAttached` stating these are loaded only for presentation config
(theme/keybindings/layout/welcome/notifications) and that skills/rules/workspace are
**ignored** in remote mode. No code change to the constructor.

### 5) Docs / comment block

- A comment block at the top of `runAttached` (and on `installPresentationHandlers`)
  enumerating, for attached mode, **client-owned** vs **daemon-owned**:
  - **Client** (`~/.gogent/config.json` on the TUI machine): theme + saved themes,
    keybindings, window layout, welcome/onboarding, notifications (client-side
    notifier). Ignored in remote mode: skills/, rules.json, workspace.
  - **Daemon** (over HTTP): sessions/messages/models/tools/skills/stats/watchers,
    **default model**, budget, timeouts, sub-agents, review-edits, daemon-side
    notification fallback.
- A short subsection in `docs/api.md` (Settings) and/or `docs/architecture.md`
  capturing the same ownership table and the `default_model` field.

## Embedded mode (must not regress)

`cmd/embedded_handlers.go::embeddedHandlersFor` keeps `GetDefaultModel →
g.DefaultModelName()`, `SetDefaultModel → g.SetDefaultModel()`, and notifications →
local `g` — unchanged. The embedded path never goes through the server, so the
`settingsSvc.Set` change does not affect it. Verified the embedded handlers wire these
from `g` (`embedded_handlers.go:106-114, 181-186`).

## Files touched

gogent only (turbotui untouched, no go.mod bump, stdlib-only):

- `internal/server/wire.go` — `default_model` field on `settingsView`.
- `internal/server/resources.go` — Get returns it; Set applies + validates it.
- `ui/tui/api_client.go` — `DefaultModel` on `SettingsDTO`.
- `ui/tui/remote_handlers.go` — wire `GetDefaultModel`/`SetDefaultModel` to the daemon.
- `cmd/attach.go` — remove local default-model wiring; add ownership comment block;
  document ignored client loads + notification policy.
- `docs/api.md` (+ optionally `docs/architecture.md`) — settingsView field + ownership.
- Tests (below).

No change to `internal/config/config.go` is actually required (the `default_model`
field already exists; getters/setters exist). I keep it on the allowed-touch list only
if a doc comment there helps; default is **no change**.

## Tests

- **`internal/server`** (new test file, e.g. `default_model_issue507_test.go`),
  driving the real handler via `server.NewServer(...)` + `srv.Handler()`
  (`api.go:88,267`) with a loopback (human) request:
  GET `/api/settings` reflects `g.DefaultModelName()`; PUT with a valid
  `default_model` updates `g.DefaultModelName()`; PUT with an **unknown** name returns
  exactly **HTTP 400** (`rec.Code == http.StatusBadRequest`, not merely "non-2xx" —
  this is what catches the bare-error→500 trap) and leaves the default **and the other
  fields** unchanged (proves validate-first ordering: send a changed budget + invalid
  model in one PUT, assert budget did **not** persist); a PUT that omits
  `default_model` (`""`) leaves the daemon default unchanged. Assert the existing
  `/settings/notifications` round-trip is independent of `default_model`.
- **`ui/tui`** (new test, e.g. `default_model_issue507_test.go`): copy the
  **stub-server + recorded-request** style of `TestRemoteHandlersMapCallsToHTTPRequests`
  (`remote_client_phase2_test.go:417`) — note there is no existing budget/settings
  handler-mapping test to mirror, only the session-handler one, so reuse its harness,
  not a budget precedent. Build `NewRemoteClient(client,…).Handlers()` against the stub
  and assert: `GetDefaultModel()` issues `GET /api/settings` and returns the stub's
  `default_model`; `SetDefaultModel("x")` issues `GET` then `PUT /api/settings` with
  `default_model:"x"`; and a stub `PUT` returning 400 makes `SetDefaultModel` return a
  non-nil error.
- **REGRESSION** (`cmd` test — both seams are package-callable; `runAttached` itself is
  not unit-callable because it inlines signal/SSH/TUI setup, so the test composes the
  handler set the same way `runAttached` does, minus the loop): temp `HOME`; seed a
  client `~/.gogent/config.json` with `default_model:"client-model"`; stand up an
  in-process daemon (`server.NewServer` + `httptest`/unix socket) whose core's default
  is `"daemon-model"`; build `handlers := rc.Handlers()` then
  `installPresentationHandlers(&handlers, g, wb, false)`. Assert:
  (a) `installPresentationHandlers` does **not** overwrite the daemon-backed
  `GetDefaultModel`/`SetDefaultModel` — set a sentinel via `rc.Handlers()` first and
  confirm it survives (guards against a future re-introduction of the local wiring);
  (b) `handlers.GetDefaultModel()` → `"daemon-model"` (NOT the client's);
  (c) after `handlers.SetDefaultModel("daemon-model-2")`, the **client** `config.json`
  is **byte-unchanged** AND its `default_model` field is still `"client-model"` (the
  field check is robust even if a future load normalizes unrelated bytes), while the
  daemon core's `DefaultModelName()` is now `"daemon-model-2"`. Because notifications
  are client-local by policy, the test does **not** assert config.json unchanged after
  a notify change.
- Keep existing suites green: `attach_phase2_test`, `handoff_issue358_test`,
  `ssh_attach_issue482_test`, model-catalog (#486), `server_test`,
  `notifications_issue358_test`, etc. (adding a field to `settingsView` is
  backward-compatible JSON.)

## The four design gates

**(1) Goal match.** Exactly the issue's ask: default_model becomes daemon-owned
(read+write over HTTP, validated daemon-side); notification ownership is decided
(client-local) and documented; no attached action silently mutates the client's
config.json for a daemon-owned field. It is a *fix*, not a feature/refactor — no new
UI, no scope creep (frontend-only NewGogent deferred to a comment).

**(2) Usability.** "Set as default" while attached updates the **daemon**, so new
daemon sessions honour it. The model selector resolves the daemon's default against
the daemon's own list — eliminating the silent index-0 fallback for the user's real
choice (`tui.go:1506`). An invalid name surfaces the daemon's "model not found" error
in the editor dialog as a **400** (error propagated, not swallowed). Default-model now
follows the same daemon-owned HTTP pattern as **budget** (both ride `/api/settings`).
Notifications deliberately stay **client-owned** — the user controls *their* terminal's
bell locally — so the model is "two daemon-owned settings (default-model, budget) +
one explicitly client-owned (notifications)", each consistent within its tier and each
documented. (This is an intentional split, not an inconsistency.)

**(3) No regressions.** Embedded path untouched (local `g` still backs these
handlers; server change doesn't touch embedded). Adding a JSON field to
`settingsView`/`SettingsDTO` is backward-compatible (old clients ignore it; the
server defaults it). `settingsSvc.Set` only acts on `default_model` when it changed,
so existing budget/timeouts/sub-agents/review-edits PUTs are unaffected. Notification
behaviour is byte-for-byte the same (only comments/docs added). Session/transcript
invariants are not in this path. gofmt/build/vet/golangci-lint/`go test ./...`
expected green (only the pre-existing environmental `TestUserSessionSendMessage` 404
in `internal/agent` may fail, per the brief).

**(4) Holistic / cross-repo.** The fix lives entirely in gogent at the correct seams:
the daemon data-plane in `internal/server`, the HTTP transport in `ui/tui/api_client`,
the Handlers mapping in `ui/tui/remote_handlers`, and the attach wiring in
`cmd/attach`. The `ui/tui` ↔ `internal/daemon` decoupling is preserved (ui/tui only
speaks HTTP via `APIClient`; it gains no daemon import). **turbotui is not involved**:
the Handlers seam and the config types live in gogent's `ui/tui` + `internal/config`;
turbotui (read-only clone at `$HOME/work/turbotui`) consumes the published Handlers
struct shape, and this change adds **no** new Handlers field and changes **no**
existing field signature (`GetDefaultModel func() string`, `SetDefaultModel
func(string) error` are unchanged — only *which closure* fills them in the attached
build). No go.mod bump, no new deps.

## Regression risks & mitigations

- **`NewGogent` rewriting `config.json` on load** would break the byte-unchanged
  assertion independent of this fix. Verified the constructor does not SaveConfig
  (`gogent.go:176-236`). Mitigation if a future migration changes this: the
  regression test also asserts the `default_model` **field value** is unchanged
  (robust even if unrelated bytes are normalized), in addition to byte-identity.
- **Partial write on a bad default name** — resolved by ordering: `default_model` is
  validated/applied **first**, so a 400 aborts the PUT before any other setter runs.
  No reliance on RMW. Tested by the "changed budget + invalid model" case.
- **Wrong HTTP status** — `g.SetDefaultModel`'s bare error would map to **500**;
  wrapped as `NewHTTPError(http.StatusBadRequest, …)` to match the repo-wide pattern.
  The server test asserts `http.StatusBadRequest` specifically (not "non-2xx").
- **Empty `default_model` in a PUT** (e.g. an older client that doesn't send the
  field) must not clear the daemon's default: Set ignores `req.DefaultModel == ""`.
  Covered by test.

## Open questions

1. **Endpoint shape** — settingsView field (chosen) vs a dedicated
   `/api/settings/default-model`. I chose the field per the issue's stated preference;
   if the maintainer wants symmetry with `/settings/review-edits` (which has both a
   field *and* a dedicated endpoint), a dedicated GET/PUT could be added later without
   breaking the field. Flagging, not blocking.
2. **Notification policy** — I chose CLIENT-LOCAL (documented). If the maintainer
   prefers SYNCED (single daemon source of truth, seeded like budget), the change is
   localized: re-point `GetNotifyConfig/SetNotifyConfig` at `/api/settings/notifications`
   and seed from `client.GetSettings`/`GetNotifyConfig` in `runAttached`, and the
   regression test would then also assert the client config.json unchanged after a
   notify change. The rest of the design is unaffected.
