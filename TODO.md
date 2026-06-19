# gogent — TODO

Outstanding work, roughly in priority order. The first two items are the reason
gogent is currently **unsafe to run on anything you care about** (see README).

## 1. Permission system — DONE

A single resource+action permission gate now lives in `internal/permission`
(`Service`), replacing the two old overlapping implementations
(`internal/fileops/permission.go` was removed; the old action-only
`internal/permission` was rewritten):

- **Shell tool is gated** (`tool.RegisterShellTool`): asked once per session
  (Allow once / Always / Deny), plus a best-effort scan that prompts **per
  external root folder** for any path a command reaches outside the workspace.
- **Interactive prompt** is wired in the TUI (`ui/tui/permission_dialog.go`):
  the agent goroutine blocks on a modal via the `permission.Prompter` interface;
  "Always" persists to `~/.gogent/permissions.json`.
- **Scope matches the real workspace root** (launch cwd). Defaults: workspace =
  allow, outside / shell = ask; headless/non-interactive runs deny on "ask".

Remaining / future (moved out of scope here):
- [ ] Optional OS-level sandbox/dry-run mode for untrusted models (the shell
      path scan is a guardrail, not containment).
- [ ] Optional `config.json` block to pre-seed a default posture (today,
      persisted decisions in `permissions.json` already cover repeat grants).

## 2. Skills support (DONE)

Skills are loaded by `internal/gogent` (from `~/.gogent/skills` and `./skills`) and
wired through the agent via progressive disclosure:

- [x] An index of **active** skills (name + description) is injected into the agent
      system prompt through a system-context provider.
- [x] A `skill` tool lets the model load a skill's full `SKILL.md` on demand; usage
      (success/failure) is recorded per skill.
- [x] `activate`/`deactivate` and usage stats are surfaced in the TUI's Resources
      browser (Config → Resources…), alongside the registered tools.
- [x] Invocation model decided: progressive disclosure (index in prompt + `skill`
      tool), the standard for agent skills.

## 3. System instructions (AGENTS.md) (DONE)

- [x] `AGENTS.md` discovery implemented in `internal/gogent/syscontext.go`: walks from
      the workspace root up to the filesystem root (outermost first, nearest last) plus
      a global `~/.gogent/AGENTS.md`, concatenated with source headers and size-capped,
      then injected into the agent system context.

## 4. Model-connection library split (MEDIUM)

- [ ] Extract `internal/model` (the `Connector` capability interfaces + HTTP
      connection + session) into its own importable module, as the package docs
      already anticipate.

## 5. Context compression rewrite (MEDIUM)

Current compression is rudimentary and needs a proper rewrite:

- [ ] Replace the naive truncation with the model-driven, structured-summary
      strategy sketched in the old `context-compression-*` notes (Goal /
      Constraints / Progress / Decisions / Next steps / Critical context /
      Relevant files).
- [ ] Trigger on token-budget thresholds, preserve recent turns + tool results,
      and keep the summary in-context across model switches.

## 6. Provider-specific request parameters (thinking, reasoning effort, …) — DONE (issue #31)

The connector previously sent a fixed OpenAI-style request (messages, temperature,
max_tokens, tools), which 400s on current-gen reasoning models. Now resolved:

- [x] Per-provider request capabilities live on `providerSpec` (`internal/model/provider.go`):
      reasoning models use `max_completion_tokens` (OpenAI o-series / GPT-5) and drop the
      rejected `temperature`; `reasoning_effort`/`thinking` are emitted only where supported.
- [x] Typed `reasoning_effort` and `thinking` fields on `config.ModelConfig`, threaded into
      `CompletionRequest` (`buildRequest`) and gated by `api_type`. Z.AI sends
      `thinking: {type: enabled|disabled}` + `reasoning_effort`; `top_p` is now sent when set.
- [x] Numeric request params (`temperature`/`top_p`/`max_tokens`/`max_completion_tokens`) are
      pointers so a deliberate `0` is expressible and "unset" omits the field.
- [x] `completion_tokens_details.reasoning_tokens` is parsed into `TokenUsage.ReasoningTokens`.
- [x] Editor UI exposes "Reasoning:" (effort) and "Thinking:" (default/on/off).

Remaining / future:
- [ ] Z.AI `clear_thinking` (cross-turn reasoning retention) and `do_sample`.
- [ ] Hide the reasoning fields in the editor for providers that don't support them
      (currently always shown — harmless, but could be gated on `api_type`).

## 7. Smaller items

- [ ] Reconcile docs/README: confirm shell/file safety story everywhere.
- [x] Hook event types (`HookResponseComplete`, `HookError`, `HookStateChange`,
      `HookCompression`) are now wired (issue #47): a turn fires
      `HookStateChange` (thinking→idle on the root agent), `HookResponseComplete`
      / `HookError` at the turn boundary, and `HookCompression` is bridged off the
      model session's existing compaction callback.
- [ ] Tests that hit a live endpoint (e.g. `TestUserSessionSendMessage`) should
      be skipped/mocked when no model server is available.
- [ ] HTTP server mode: document and test the API surface (`/health`, `/message`,
      `/exit`).

## 8. Window scalability — DONE

- [x] Enable `Resizable` on session windows using turbotv's built-in support
- [x] Enable `Minimizable` on session windows for collapse/expand functionality
- [x] Add `WindowConfig` to config package with configurable `MinWidth`/`MinHeight`
- [x] Document window scaling features in README

## Known issues

- **Terminated sub-agents still count toward the running-agent limit.** Finished
  or failed agents are kept in the agent tree (so the UI can still show them) but
  are also counted against the configured fan-out / max-agent limit, so a session
  effectively runs out of agent slots over time. Workaround: raise the limit in
  the config. Fix: exclude terminal-state agents from the active count (or track
  active agents separately from the tree).

