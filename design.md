# Design — Issue #492: wire the Resources dialog's MCP tab to live MCP servers/tools

## Summary of the bug

MCP client support already exists end-to-end in gogent (`internal/mcp`,
`internal/gogent/mcp.go`, `internal/config` `MCPServers`). When `mcp_servers` is
configured, `StartMCPServers()` dials each server, calls `tools/list`, and
registers every remote tool into the tool registry via
`newMCPTool(server, client, mt)`. Those tools are already agent-callable and
already appear in the Resources dialog's **Tools** tab.

The only thing "missed" is the Resources dialog's dedicated **MCP** tab. It is a
hardcoded placeholder:

- `ui/tui/resources_dialog.go:138` — `currentItems()` returns
  `nil // MCP: no servers until #36` for `resourceMCP`.
- `emptyDetail`/`mcpPlaceholder` (~432/445) render "No MCP servers are
  configured yet… lands with issue #36".
- The package doc (~42) calls the MCP tab "a placeholder until MCP client
  support lands (#36)".

So even with connected MCP servers, the MCP tab shows nothing. **That is the
bug.** This is a *fix* (wire an existing-but-stubbed tab to live data), not a new
feature or a refactor.

## Verified facts (read from source, not assumed)

1. **MCP tools carry their server identity in the registered name.**
   `internal/gogent/mcp.go:125` defines `mcpToolPrefix = "mcp__"` and
   `newMCPTool` (line 135) names every remote tool
   `mcp__<server>__<toolName>`. The description and `InputSchema` are also set.
2. **That name reaches the UI verbatim on both transports.**
   - Embedded: `cmd/embedded_handlers.go:214` `GetTools` copies `t.Name`,
     `t.Description`, `tool.SchemaJSON(t.InputSchema)` straight into `ToolInfo`.
   - Remote: `ui/tui/remote_handlers.go:785` `GetTools` does the same from the
     daemon's `/api` tool list.
   `ToolInfo` (`ui/tui/tui.go:433`) = `{Name, Description, InputSchema string,
   Enabled, Invocations, LastUsed}`.
3. **`StartMCPServers()` is already invoked on every launch path** — embedded
   `cmd/main.go:256`, daemon `cmd/daemon.go:338`, handoff `cmd/handoff.go:337`.
   So Required-change #3 is already satisfied; **no startup wiring change is
   needed.**
4. The MCP tools are namespaced specifically so the bare name is unambiguous
   only via fallback (issue #360 note in `mcp.go`). The `mcp__<server>__<tool>`
   prefix is therefore a *stable, intentional* contract we can parse.

## Chosen approach: client-side derive (no new handlers/endpoints)

Because the registered tool items already carry server-origin info in their
name, we derive the MCP tab **entirely client-side** from the existing
`GetTools` data, confined to `ui/tui/resources_dialog.go` (+ its test). This is
the PREFERRED path in the task brief and the FALLBACK
(`GetMCPServers`/`GET /api/mcp`) is **not** needed. Crucially this avoids
touching `ui/tui/api_client.go`, `remote_handlers.go`, `tui.go`,
`internal/gogent/gogent.go`, and `internal/server`, all of which have in-flight
work (#482 ssh, #486 model catalog, #490 sidebar) — zero conflict surface.

### Files touched (gogent only)

- `ui/tui/resources_dialog.go` — the real change.
- `ui/tui/resources_dialog_test.go` — new tests + update the one stale
  placeholder assertion.

**No turbotui change.** The dialog only *consumes* turbotui widgets
(`tv.Tree`, `tv.TextView`, etc.); the repo seam is respected and there is no
downstream effect on `github.com/hobbestherat/turbotui`.

### Mechanics in `resources_dialog.go`

1. **New deriver `loadMCPItems(get func() []ToolInfo) []resourceItem`** (mirrors
   `loadToolItems`/`loadSkillItems`):
   - Call `get()`; keep only tools whose `Name` has prefix `mcp__`.
   - Parse each: strip `mcp__`, then split on the **first** `__` →
     `server` = left, `toolName` = right (the bare name may itself contain `__`,
     so split once, not greedily). A name with no second `__` is malformed and
     skipped.
   - Group by server. Sort server names; within a server sort tool names.
   - Emit, per server, in this order:
     - one **server header** item: `{kind: resourceMCP, name: server,
       canToggle:false, usage: "<n> tool(s)", detail: mcpServerDetail(server,
       tools)}`.
     - one **tool** item per tool: `{kind: resourceMCP, name: server+"/"+
       toolName, desc: description, canToggle:false,
       detail: mcpToolDetail(server, toolName, description, schema)}`.
   - Build the slice already in grouped/sorted order and **do not** re-run
     `sortResourceItems` on it (sorting by `name` would still keep
     `server` before `server/tool` before `serverB`, but constructing in order
     is clearer and guarantees the header leads its group).
   - MCP rows are **read-only** (`canToggle:false`): MCP tools stay toggleable
     from the existing Tools tab by their real registered name; making them
     toggle here would need the namespaced-name round-trip and risks diverging
     enabled-state between the two tabs — out of scope for this fix.

2. **`detail` renderers** (kept local, mirroring `toolDetail`):
   - `mcpServerDetail(server, tools)` — header `MCP server: <server>`, a tool
     count, and a bulleted list of the tool names it advertises.
   - `mcpToolDetail(server, toolName, desc, schema)` — `MCP tool: <toolName>` /
     `Server: <server>` / `Registered as: mcp__<server>__<toolName>` (so the
     user sees the agent-callable name) / `Description` / `Input schema`,
     matching `toolDetail`'s layout so the pane is visually consistent.

3. **`currentItems()` (line ~131)** — replace
   `return nil // MCP: no servers until #36` with `return allMCP`, where
   `allMCP = loadMCPItems(w.handlers.GetTools)` is captured in the
   `browser state` block (line ~125) alongside `allTools`/`allSkills`, and
   refreshed in the `catSel.OnChange` and `toggleSelected` reload blocks for
   parity (cheap; keeps the tab live if tools change).

4. **`emptyDetail`/`mcpPlaceholder` (~429/445)** — keep the empty-state path but
   rewrite `mcpPlaceholder` text: drop the "#36 / lands later" wording and say
   the tab is empty because **no MCP servers are configured or connected**, with
   a one-line pointer to the `mcp_servers` config key. It is shown only when the
   MCP item count is 0 and there is no search query — unchanged gating, so a
   non-empty server list never shows it.

5. **Package doc (~42)** — update to: the MCP tab lists the configured/connected
   MCP servers and the tools each advertises (via `tools/list`), derived from
   the registered `mcp__*` tools.

### Reused, unchanged plumbing

`filterResources` (search), `resourceListLabel` (checkbox/pad/usage rendering;
non-togglable rows already render a blank slot so headers look like section
rows), `render()`, `OnSelect`, the detail pane, and `installResizeReflow` all
work as-is on the new items — no changes to the shared render path.

## User-facing behavior

- Open Resources → **MCP** tab. With `mcp_servers` configured and connected, the
  list shows each server as a header row (`<n> tool(s)`) followed by its tools
  (`server/tool`). Arrow/click selects a row; the detail pane shows the server
  summary or the tool's description + input schema + its agent-callable
  `mcp__…` name.
- Search filters MCP rows by name/description like the other tabs.
- With no MCP servers (or none connected), the tab shows the rewritten empty
  state — never the old "#36" placeholder.
- MCP tools remain callable by the agent and still appear under the Tools tab
  (unchanged).

## Testing

Following `internal/mcp/mcp_test.go` / `internal/gogent/mcp_test.go`'s stub
pattern, but the new tests are **UI-level and need no live MCP server** because
the deriver consumes `[]ToolInfo`:

- `loadMCPItems` with synthetic `ToolInfo`s named `mcp__srvA__greet`,
  `mcp__srvA__echo`, `mcp__srvB__nav` → assert grouping (header per server, tool
  rows under the right server, sorted), counts, and that non-`mcp__` tools are
  excluded.
- `mcpToolDetail` / `mcpServerDetail` contain the tool name, server, namespaced
  name, description, and schema.
- Empty case: `loadMCPItems` over tools with no `mcp__` entries → empty slice;
  `emptyDetail(resourceMCP, 0, "")` returns the new empty-state text.
- Update the existing stale assertion `resources_dialog_test.go:188`
  (`{"mcp placeholder", resourceMCP, 0, "", "#36"}`) to assert the new
  empty-state wording instead of `#36`.

Optionally, an end-to-end test can stand up the existing stub MCP server, run
`StartMCPServers()`, build `ToolInfo`s from the registry via the same mapping
`embedded_handlers.go` uses, and assert `loadMCPItems` surfaces the stub's tool
— but the `ToolInfo`-level test already covers the UI contract without the
process plumbing.

**Live verification with `chrome-devtools-mcp` and `playwright-mcp` requires
Node/npx + browsers + network and is NOT part of the Pi5 automated gate.** The
PR body will note: "Automated tests use an in-process/stub MCP source; live
verification with chrome-devtools-mcp and playwright-mcp is a manual maintainer
step (verified locally / to be verified by maintainer)."

## Criteria assessment

**(1) Goal match.** Exactly the ask: the MCP tab now lists configured/connected
servers and their advertised tools with detail; placeholder gone when servers
exist; empty state retained for none. No scope creep (no new MCP runtime, no
toggle semantics, no startup rewiring — already present and verified).

**(2) Usability.** The MCP tab behaves like the Tools/Skills tabs: same list +
search + detail-pane interaction, user-driven selection, server grouping makes
the source obvious, and the agent-callable `mcp__…` name is surfaced rather than
hidden. The right thing is shown, not silently empty.

**(3) No regressions.** Change is additive and confined to `resourceMCP`
branches; Tools/Skills loaders, sort, filter, toggle, and render are untouched.
`internal/mcp` and `internal/gogent/mcp.go` behavior is unchanged (we only read
the registry the UI already reads). `StartMCPServers` gating and invocation are
unchanged. Only one existing test asserts old text (`#36`) and is updated
deliberately. gofmt/vet/build/lint/test gate per `[[dev-gate]]` (no `-race` on
Pi5); the pre-existing `TestUserSessionSendMessage` 404 remains the only
accepted failure.

**(4) Holistic / repo seam.** Confined to `ui/tui/resources_dialog.go` (+test),
the correct layer (a UI tab fed by an existing handler). No api_client/
remote_handlers/server/tui.go/gogent.go edits → no conflict with #482/#486/#490.
turbotui is only consumed, never modified; no go.mod bump; no new dependencies;
stdlib + existing MCP client reused.

## Regression risks

- **Stale placeholder test** (`resources_dialog_test.go:188`) asserts `#36`;
  must be updated or the suite fails. Identified and planned.
- **Name-parse assumption**: relies on the `mcp__<server>__<tool>` contract from
  `newMCPTool`. It is intentional and tested (`mcp_test.go` asserts the
  namespaced name). If that prefix ever changes, the deriver must change too —
  acceptable coupling within one repo; noted.

## Open questions

1. **Zero-tool / unconnected-but-configured servers.** The client-side-derive
   approach only surfaces servers that registered ≥1 tool (i.e. connected and
   non-empty). A server that is configured but **denied/unreachable**, or that
   advertises **zero tools**, will not appear in the MCP tab. The brief's
   acceptance says "configured/connected"; the pragmatic, conflict-free derive
   path shows *connected* servers. Showing configured-but-not-connected entries
   with a status would require the FALLBACK `Handlers.GetMCPServers` accessor +
   `GET /api/mcp` (touching api_client/remote_handlers/server) — which conflicts
   with in-flight work. **Recommendation:** ship the derive approach (covers the
   issue's real target — chrome-devtools-mcp/playwright-mcp both advertise many
   tools), and note this limitation; add the accessor later only if maintainers
   want connection-status for failed servers. Confirm this trade-off is
   acceptable.
2. **MCP tool toggling from the MCP tab.** Proposed read-only here (toggle stays
   on the Tools tab). If maintainers want Space/Enter to enable/disable an MCP
   tool from this tab too, it's a small follow-up wiring `SetToolEnabled` with
   the namespaced name. Confirm read-only is fine for this fix.
