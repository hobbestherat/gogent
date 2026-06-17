# gogent — TODO

Outstanding work, roughly in priority order. The first two items are the reason
gogent is currently **unsafe to run on anything you care about** (see README).

## 1. Permission system (HIGH — currently unsafe)

The agent runs tools with effectively no enforcement:

- **Shell tool is completely ungated** (`internal/tool/tool.go` `RegisterShellTool`)
  — any command the model emits is executed. This is the main danger.
- **No interactive prompt flow.** `fileops.PermissionService.Assert` returns a
  `PermissionRequiredError` on "ask", but nothing in the UI ever calls
  `Ask`/`Reply`, so "ask" effectively just fails the tool instead of prompting.
- **Two overlapping, duplicated implementations:**
  - `internal/permission` (`PermissionConfig`, three-level global→session→agent,
    yes/no/ask) — **not referenced anywhere** outside its own tests.
  - `internal/fileops/permission.go` (`PermissionService`, rule/effect based) —
    partially wired into the file tools only.
- Default allow rule targets `~/.gogent/workspace`, but the real workspace root
  is the launch cwd, so the rules don't line up with actual resources.

To do:
- [ ] Pick **one** permission model and delete the other.
- [ ] Gate the **shell tool** (and sub-agent spawning / network) through it.
- [ ] Wire an interactive **permission prompt** in the TUI (allow / deny / always),
      backed by `Ask`/`Reply` and persisted to `permissions.json`.
- [ ] Make permission scope match the real workspace root; sane defaults
      (workspace = allow, outside = ask, destructive = ask).
- [ ] Optional sandbox/dry-run mode for untrusted models.

## 2. Skills support (HIGH — loaded but unused)

Skills are read at startup (`skill.LoadSkills`, printed in `cmd/main.go`) but:

- [ ] Skills are **never injected** into the agent system prompt nor exposed as
      callable tools — `internal/skill` is not used by `internal/agent` or
      `internal/gogent`.
- [ ] Wire `activate`/`deactivate` and usage stats into the session/agent.
- [ ] Surface active skills + stats in the TUI.
- [ ] Decide on the skill invocation model (auto-context vs. explicit tool).

## 3. System instructions (AGENTS.md) (MEDIUM — planned, not implemented)

- [ ] Implement `AGENTS.md` discovery (walk up from cwd + config dirs), as
      described in the old design docs. No `SystemContext` exists yet.

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

## 6. Provider-specific request parameters (thinking, reasoning effort, …) (MEDIUM)

The connector currently sends a fixed OpenAI-style request (messages, temperature,
max_tokens, tools). Some providers expose extra controls we should surface in the
model config + editor, gated by `api_type` so they only appear where supported:

- [ ] **Z.AI** (`api_type: "zai"`, see https://docs.z.ai/api-reference/llm/chat-completion):
  - `thinking` — `{ "type": "enabled" | "disabled" }` to toggle chain-of-thought
    (GLM-4.5+); plus `clear_thinking` for cross-turn reasoning retention.
  - `reasoning_effort` — `max | xhigh | high | medium | low | minimal | none`
    (GLM-5.2), takes effect when thinking is enabled.
  - `do_sample`, `top_p` (already in config but not always sent).
- [ ] Add the corresponding fields to `config.ModelConfig` (e.g. a small,
      provider-namespaced `params`/`extra` map, or typed `thinking`/`reasoning_effort`
      fields) and thread them into `CompletionRequest` only for matching providers.
- [ ] Editor UI: show "Thinking" (on/off) and "Reasoning effort" (dropdown) when
      `api_type` supports them; hide otherwise.

## 7. Smaller items

- [ ] Reconcile docs/README: confirm shell/file safety story everywhere.
- [ ] Hook event types (`HookResponseComplete`, `HookError`, `HookStateChange`,
      `HookCompression`) are defined but unused — wire them or trim them.
- [ ] Tests that hit a live endpoint (e.g. `TestUserSessionSendMessage`) should
      be skipped/mocked when no model server is available.
- [ ] HTTP server mode: document and test the API surface (`/health`, `/message`,
      `/exit`).

## Known issues

- **Terminated sub-agents still count toward the running-agent limit.** Finished
  or failed agents are kept in the agent tree (so the UI can still show them) but
  are also counted against the configured fan-out / max-agent limit, so a session
  effectively runs out of agent slots over time. Workaround: raise the limit in
  the config. Fix: exclude terminal-state agents from the active count (or track
  active agents separately from the tree).

