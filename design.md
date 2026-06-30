# Design — Timeouts UX / Discoverability (+ optional per-model override)

Issue #590 "[UX] Timeouts" (maintainer hobbestherat). Closes #590.

## Problem (faithful to the issue)

Model / tool / sub-agent timeouts are **hard to find**: they are only editable from
inside the **Sub-agent Settings** dialog, which is not where users look for them. The
maintainer asks to either make timeouts an explicit config or move them to a dedicated
**"Limits…"** page, and *optionally* let a model override the timeout for slow local
models.

## Key finding from reading the code (this shapes the whole design)

The "buried under Sub-agents" problem is **UI-only**. In the persisted config the
timeouts already live in their **own top-level `timeouts` JSON block**, completely
separate from `sub_agents`:

- `internal/config/config.go:275` — `type TimeoutConfig struct{ ModelSeconds; ToolSeconds; SubAgentSeconds }`
  with JSON tags `model_seconds` / `tool_seconds` / `subagent_seconds`.
- `internal/config/config.go:610-611` — the `Config` struct has **separate** fields
  `SubAgents SubAgentConfig \`json:"sub_agents"\`` and `Timeouts TimeoutConfig \`json:"timeouts"\``.
- `SubAgentConfig` (config.go:172) holds **no** timeout fields.

So there is **no config-schema migration to do**: the keys do not move. The fix is a
**UI relocation** — pull the three timeout fields out of `showSettingsDialog` and give
them their own discoverable **Timeouts** dialog reachable from the Config menu and the
command palette. Backward compatibility is therefore automatic (the on-disk schema is
unchanged), but I will still add tests that prove old configs load to identical
effective timeouts.

The three timeouts are consumed at (unchanged by this work):
- Model: `gogent.go:1556` `conn.SetTimeout(... ModelSecondsOrDefault())` in `buildConnection`.
- Tool/shell: `gogent.go:803/806` `toolRegistry.ShellTimeout = ... ToolSecondsOrDefault()`.
- Sub-agent: `gogent.go:1490` `SetSubAgentTimeout(... SubAgentSecondsOrDefault())`.
Defaults are all 300s (`config.go:285-289`) via the `*OrDefault()` accessors — **unchanged**.

## Scope

- **Part A (ship): UI relocation.** New dedicated **Timeouts** dialog; remove the three
  timeout fields from the Sub-agent Settings dialog; surface the new dialog in the
  Config menu + command palette. No config-schema change. No default change.
- **Part B (optional, recommended-if-clean): per-model model-timeout override.** A new
  optional `ModelConfig.ModelTimeoutSeconds`; when >0 it overrides the global
  `model_seconds` for that model, else falls back to global exactly as today. Applied at
  the single existing resolution point (`buildConnection`). Part B is self-contained and
  can be dropped without affecting Part A.

---

## Part A — files & functions to touch (gogent only)

1. **`ui/tui/settings_dialog.go`**
   - **New `showTimeoutsDialog()`** (a sibling of `showSettingsDialog`): a small
     fixed-form dialog titled **"Timeouts"** with three labelled numeric fields built
     from the existing `newNumField` helper — "Model timeout (s):", "Tool timeout (s):",
     "Sub-agent timeout (s):" — seeded from `GetTimeouts()` (falling back to
     `config.DefaultTimeoutConfig()`), plus a one-line help label
     ("Seconds. 0 = use the built-in default (300s).") and an OK/Cancel row. On OK it
     reuses the exact apply logic that exists today (`atoiOr(...)` → `SetTimeouts(t)`),
     guarded by `GetTimeouts != nil && SetTimeouts != nil`. Reuses `numField`, `atoiOr`,
     `dialogLabel`, `newButton`, Escape-to-cancel, `applyWindowShadow` — no new widgets.
   - **Edit `showSettingsDialog()`**: remove the `timeoutsLabel`, `modelTO`, `toolTO`,
     `subTO` fields (lines 145-148, 157, the `AddContent` loop at 158, and the
     `SetTimeouts` block at 187-193). Re-layout: move the review-edits / show-welcome
     toggles up to fill the freed rows and **shrink the pinned height** (the `DialogSpec`
     at line 76 `MinH/MaxH: 20` → ~`16`; update the height-rationale comment). The
     Sub-agent dialog keeps Max-sub-agents and Max-recursion-depth (these are
     sub-agent-specific fan-out/recursion bounds, not the cross-cutting timeouts the
     issue is about).

2. **`ui/tui/tui.go` — `settingsItems()`** (≈line 1270): add a plain
   `tv.NewMenuItem("&Timeouts…", func(){ w.showTimeoutsDialog() })`, gated on
   `w.handlers.GetTimeouts != nil && w.handlers.SetTimeouts != nil`, placed right after
   the "&Sub-agents…" / "&Models…" cluster so it reads as a peer settings page. (Mirrors
   how Notifications/Theme are gated on their own handlers.)

3. **`ui/tui/command_palette.go`** (Config category, ≈line 249): add
   `{category: "Config", name: "Timeout settings", run: w.showTimeoutsDialog,
   enabled: avail(h.GetTimeouts != nil && h.SetTimeouts != nil)}`.

   **Decision: no new rebindable ActionID / default chord.** Like Models, Resources and
   Notifications, the Timeouts entry is a plain menu+palette command. This keeps it
   discoverable without touching the keybinding action registry or the action-enumeration
   tests (`keybindings_issue401_test.go`, `keybindings_issue463_test.go`), which reduces
   regression surface. (Open question below if a chord is wanted.)

No new handlers needed: `GetTimeouts`/`SetTimeouts` already exist on the `Handlers`
struct (`tui.go:67-68`) and are wired in `remote_handlers.go:1265-1276`.

### Naming

I recommend titling the dialog/menu **"Timeouts"** (not "Limits"): it is the precise,
faithful name for the three fields and the most discoverable for a user who, per the
issue, is hunting for *timeouts*. ("Limits" is the issue's alternative; see Open
questions — easy to rename, and it would only host timeouts anyway since Max-sub-agents/
depth stay with Sub-agents.)

---

## Part B — per-model model-timeout override (optional)

Follows the exact pattern of the recent "context window as model setting" (commit
7bada0a): new optional field on `ModelConfig` + an `*OrDefault` accessor + apply at the
one resolution point.

1. **`internal/config/config.go`**
   - Add to `ModelConfig` (after `ContextWindow`, ≈line 47):
     `ModelTimeoutSeconds int \`json:"model_timeout_seconds,omitempty"\`` with a doc
     comment: ">0 overrides the global model timeout for this model (slow local models);
     0/unset = use the global `timeouts.model_seconds`."
   - Add accessor:
     `func (m *ModelConfig) ModelTimeoutSecondsOrDefault(globalSeconds int) int` →
     returns `globalSeconds` when `m == nil || m.ModelTimeoutSeconds <= 0`, else the
     override. (`omitempty` keeps old configs byte-identical on save.)

2. **`internal/gogent/gogent.go` — `buildConnection()`** (line 1551-1559): change the one
   line to resolve through the override:
   ```go
   global := g.config.Timeouts.ModelSecondsOrDefault()
   conn.SetTimeout(time.Duration(cfg.ModelTimeoutSecondsOrDefault(global)) * time.Second)
   ```
   This is the single place every model connection is built, so it covers all model
   calls with no other read-site changes. Scope of the override is the **model HTTP
   request timeout only** — tool/shell and sub-agent timeouts are not model-specific, so
   the issue's "let the model override the timeout (slow local models)" maps precisely to
   the model timeout.

3. **UI (optional within Part B) — `ui/tui/model_editor.go`**: add one optional numeric
   field "Model timeout (s):" (0 = use global) to the model add/edit form, reading/writing
   `cfg.ModelTimeoutSeconds`. Uses the editor's existing `field(...)`/`atoiOr` helpers.
   If we choose to keep Part B config-only, the field is editable via `config.json` and
   the catalog; the UI field is a small, isolated add. **Part B does not touch
   `internal/model`** — the timeout is applied through `conn.SetTimeout` on the HTTP
   client, so there is no collision with #589's `internal/model` settings struct.

---

## User-facing behavior

- A new **Config ▸ Timeouts…** menu entry and a **"Timeout settings"** command in the
  palette open a small, clearly-labelled dialog showing the three timeouts in seconds.
  Editing + OK persists immediately via the existing `SetTimeouts` path and applies live
  to active sessions (`SetTimeouts` already does this, gogent.go:3179-3213).
- The Sub-agent Settings dialog no longer shows timeouts (it keeps the sub-agent
  execution-style, recursion, fan-out/depth, review-edits and welcome toggles) and
  becomes a little shorter.
- Existing configs load and behave **identically** — same keys, same defaults.
- (Part B) Setting a model's "Model timeout (s)" makes that model wait longer/shorter
  than the global; leaving it 0 keeps today's behavior.

---

## Tests (deterministic, hermetic — no network/sleeps)

- **`internal/config`**: round-trip a `TimeoutConfig` (marshal→unmarshal, values
  preserved). Load a `config.json` that contains a `timeouts` block → effective timeouts
  equal the configured values (proves the explicit/relocated form loads). Load a config
  with **no** `timeouts` block → `*OrDefault()` yields the 300s defaults (proves
  defaults unchanged / old configs unaffected).
- **(Part B) `internal/config`**: round-trip `ModelConfig.ModelTimeoutSeconds`;
  `ModelTimeoutSecondsOrDefault(global)` returns the override when set and `global` when
  unset (the effective-timeout resolution gate); confirm `omitempty` keeps it out of
  serialized output when 0.
- **`ui/tui`**: update `dialog_issue317_test.go` — the Sub-agent dialog's pinned size
  expectation changes from `72×20` to the new shorter height (e.g. `72×16`); add a case
  that `showTimeoutsDialog()` (driven with `settingsHandlers()`) opens to its pinned
  footprint and is reachable. Assert the "&Timeouts…" item / palette command appears
  when the handlers are wired and is absent when they are not (gating).

---

## The 4 design gates

**(1) Goal match.** Exactly the issue's ask: timeouts become an explicit, discoverable
**Timeouts** settings page instead of being buried under Sub-agents; optional per-model
override added cleanly. No rename of config keys, no default changes, no unrelated scope.

**(2) Usability.** Users find timeouts under **Config ▸ Timeouts…** and via the palette
("Timeout settings") — where they'd look. Clear seconds labels + a "0 = default (300s)"
hint. User drives every value (numeric textboxes); `atoiOr` prevents a blank/garbage
field from wiping a timeout. Live-applied on OK. Per-model override is surfaced in the
model editor (not silent).

**(3) No regressions.** Config schema is untouched → all existing configs load
identically; `*OrDefault` defaults unchanged. The only behavioral move is UI placement.
Known regression points are bounded and handled: the Sub-agent dialog re-layout requires
updating its size assertion in `dialog_issue317_test.go` (expected, covered by the test
update). No new ActionID → keybinding action-enumeration tests untouched. Part B is
additive (`omitempty`, zero-value = today's behavior) and routes through the single
existing `buildConnection` resolution point, so non-overridden models are byte- and
behavior-identical. gofmt/build/vet/golangci-lint must stay clean; `go test ./...` green
(no `-race` on Pi5); the pre-existing `TestUserSessionSendMessage` 404 is the only
acceptable failure.

**(4) Holistic across both repos.** **gogent-only.** The new dialog is composed entirely
from **existing turbotui primitives** (`tv.Dialog`, `tv.TextBox`, `tv.Label`,
`tv.Button`, `tv.NewModalLayer`) and existing gogent helpers (`numField`, `newNumField`,
`atoiOr`, `dialogLabel`, `newButton`). **No turbotui change** is needed or made — the
seam (gogent assembles dialogs from turbotui widgets) is respected, no new widget pushed
down into the library. No new dependencies; stdlib-first.

---

## Regression risks (called out)

- **Sub-agent dialog re-layout.** Removing three rows shifts the height and the toggle
  positions; `dialog_issue317_test.go` pins the size and must be updated in lockstep.
  Mitigation: keep it a static `DialogSpec` (no terminal-dependent fields) so `Fit` on
  resize stays correct, and update the rationale comment + the test together.
- **Menu/palette gating.** Must gate the new entry on `GetTimeouts && SetTimeouts` so the
  daemon-unwired/headless paths don't show a dead entry (matches Notifications/Theme).
- **Part B precedence bug risk.** Easy to invert the fallback; the accessor + its unit
  test (override-wins / global-when-unset) pin the resolution order.

---

## Open questions

1. **Name: "Timeouts" vs "Limits".** I recommend **Timeouts** (precise + most
   discoverable for the issue's use case). If the maintainer prefers the issue's
   "Limits…" wording, the rename is trivial — but then should Max-sub-agents / Max-depth
   also move into "Limits" for consistency? My default is *no* (they're sub-agent
   topology, not cross-cutting limits) — which is itself a reason to prefer the
   "Timeouts" name.
2. **Ship Part B now?** It is clean and self-contained (config field + accessor + one
   line in `buildConnection`, optionally one model-editor field). Recommend including it;
   it directly serves the issue's "slow local models" motivation. If the ui/tui or model
   lanes are contended at the gate, Part B can ship config-only (no model-editor field)
   or be deferred without touching Part A.
3. **Chord.** No default keybinding is proposed for the Timeouts page (keeps the action
   registry/tests untouched). Add one later if discoverability via menu+palette proves
   insufficient.
