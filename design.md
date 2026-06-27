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
  `DefaultModel: svc.s.g.DefaultModelName()`. `settingsSvc.Set` applies it via
  `svc.s.g.SetDefaultModel(req.DefaultModel)` **only when** `req.DefaultModel != ""`
  **and** differs from the current value (so an unrelated PUT — e.g. a budget change —
  never re-validates/clears it), and **returns that error** when `SetDefaultModel`
  reports "model not found", so a bad name surfaces to the caller instead of being
  swallowed. The other field setters are unchanged; because every realistic caller
  does a read-modify-write, the other fields equal their current values and remain
  effective no-ops even if Set returns early on the default-model error.
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
    and returns that result. `APIClient.SetSettings` already converts a daemon
    non-2xx (the 400 "model not found") into a Go error, so the daemon's validation
    propagates to the dialog.
- **`cmd/attach.go::installPresentationHandlers`** — **remove** the
  `h.GetDefaultModel`/`h.SetDefaultModel` local-`g` assignments. They now come from
  `rc.Handlers()` (installed before `installPresentationHandlers`, which no longer
  overwrites them). Update the function's doc comment to drop "default-model
  dropdown selection" from the list of client-owned concerns.
- **`docs/api.md`** — update the `settingsView` description to
  `{sub_agents, timeouts, budget, review_edits, default_model}`.

Why settingsView over a dedicated `/api/settings/default-model`: the issue lists the
settingsView field as PREFERRED; it reuses the existing route, mirrors budget 1:1,
and needs no new api.go route wiring. The only nuance (error propagation on an
invalid name) is handled by returning the error from `settingsSvc.Set` and by
`RemoteClient.SetDefaultModel` doing an explicit RMW rather than the silent
`mutateSettings`.

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

- **`internal/server`** (new test file, e.g. `default_model_issue507_test.go`):
  GET `/api/settings` reflects `g.DefaultModelName()`; PUT `/api/settings` with a
  valid `default_model` updates `g.DefaultModelName()`; PUT with an unknown name
  returns a non-2xx error and leaves the default unchanged; existing
  `/settings`/`/settings/notifications` round-trips unchanged (assert the notify
  endpoint is independent of default_model).
- **`ui/tui`** (extend the remote-client test set, mirroring the budget test pattern
  in `remote_client_phase2_test.go`): a stub daemon server; `GetDefaultModel` hits
  GET `/api/settings`; `SetDefaultModel` issues GET+PUT and returns the daemon's
  error on a 400.
- **REGRESSION** (`cmd` or `ui/tui` with a temp HOME + in-process daemon server):
  seed a client `~/.gogent/config.json` with `default_model:"client-model"`; stand up
  a daemon whose default is `"daemon-model"`; build the attached handlers; call
  `GetDefaultModel()` → expect `"daemon-model"` (NOT the client's), and
  `SetDefaultModel("daemon-model-2")` → expect the **client** `config.json`
  byte-unchanged (and its `default_model` still `"client-model"`), while the daemon's
  `DefaultModelName()` is now `"daemon-model-2"`. Because notifications are
  client-local by policy, the test does **not** assert config.json unchanged after a
  notify change.
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
in the editor dialog (error is propagated, not swallowed). Default-model now follows
the same daemon-owned pattern as budget, so the three settings behave consistently.
The user still controls *their* terminal's notifications locally, which is the
expected behaviour for a desktop bell.

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
- **`settingsSvc.Set` returning an error mid-apply** could in principle leave a
  partial write. Because the only realistic caller (RemoteClient) does a
  read-modify-write, the non-default fields equal current values, so the SetXxx calls
  are no-ops and an early return on the default-model error is harmless. Documented in
  the handler comment.
- **Empty `default_model` in a PUT** (e.g. an older client) must not clear the
  daemon's default: Set ignores `req.DefaultModel == ""`. Covered by test.

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
