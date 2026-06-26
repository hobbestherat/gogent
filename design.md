# Design — Issue #492: wire the Resources dialog's MCP tab to live MCP servers/tools

## Summary of the bug

MCP client support already exists end-to-end in gogent (`internal/mcp`,
`internal/gogent/mcp.go`, `internal/config` `MCPServers`). When `mcp_servers` is
configured, `StartMCPServers()` dials each server, calls `tools/list`, and
registers every remote tool into the tool registry via
`newMCPTool(server, client, mt)`. Those tools are already agent-callable and
already appear in the Resources dialog's **Tools** tab.

The only thing "missed" is the Resources dialog's dedicated **MCP** tab, a
hardcoded placeholder:

- `ui/tui/resources_dialog.go:138` — `currentItems()` returns
  `nil // MCP: no servers until #36` for `resourceMCP`.
- `emptyDetail`/`mcpPlaceholder` (~432/445) render "No MCP servers are
  configured yet… lands with issue #36".
- The package doc (~42) calls the MCP tab "a placeholder until MCP client
  support lands (#36)".

So even with connected MCP servers the MCP tab shows nothing. **That is the
bug** — a *fix* (wire an existing-but-stubbed tab to live data), not a feature
or refactor.

## Verified facts (read from source)

1. `internal/gogent/mcp.go:125,135` — `mcpToolPrefix = "mcp__"`; `newMCPTool`
   names every remote tool `mcp__<server>__<toolName>`, sets `InputSchema`, and
   (lines 144-145) **appends** to the description:
   `\n\nWhen calling this tool, use its full name "mcp__…".`
2. That name/desc/schema reaches the UI verbatim on both transports — embedded
   `cmd/embedded_handlers.go:214-235` and remote `ui/tui/remote_handlers.go:785-800`
   both copy `Name`/`Description`/serialized `InputSchema` into
   `ToolInfo{Name, Description, InputSchema string, …}` (`ui/tui/tui.go:433`).
3. `StartMCPServers()` is invoked on every launch path — embedded
   `cmd/main.go:256`, daemon `cmd/daemon.go:338`, handoff `cmd/handoff.go:337`.
   **Required-change #3 is already satisfied; no startup wiring change is
   needed.**
4. `resourceListLabel` hard-truncates the name to `nameW = 22` runes with
   `truncateRunes` (`session_window.go:3111`) — **no ellipsis, a clean cut**.
   The list column `listW` is capped at 34 cols. This is the constraint that
   drives the label design below.
5. `ui/tui` already imports `internal/gogent` (`tui.go:17`,
   `remote_handlers.go:15`), so the deriver's end-to-end test can live in
   `ui/tui` and build a real `Gogent` (item below).
6. The only stale MCP-placeholder references are
   `resources_dialog.go:42-43/138/443-449` plus one test assertion
   (`resources_dialog_test.go:188` expects `#36`). Scope is fully enumerated.

## Chosen approach: client-side derive (no new handlers/endpoints)

The registered tool items already carry server-origin info in their name, so the
MCP tab is derived **entirely client-side** from the existing `GetTools` data —
the PREFERRED path in the brief. The FALLBACK
(`GetMCPServers` + `GET /api/mcp`) is **not** used, which avoids touching
`api_client.go`/`remote_handlers.go`/`tui.go`/`internal/gogent`/`internal/server`
— all of which carry in-flight work (#482 ssh, #486 catalog, #490 sidebar).
Zero conflict surface.

### Files touched (gogent only)

- `ui/tui/resources_dialog.go` — the change.
- `ui/tui/resources_dialog_test.go` — new tests + update one stale assertion.

**No turbotui change.** The dialog only *consumes* turbotui widgets
(`tv.Tree`, `tv.TextView`); the repo seam is respected, no downstream effect on
`github.com/hobbestherat/turbotui`.

### The list model: server header rows + bare-name tool rows

Two-tier flat list, grouped by server. This is the crux of the usability fix
(see critique resolution below): the server lives in a **header row**, and each
tool row shows the **bare tool name** so it survives the 22-column truncation.

`resourceItem` gains one unexported field, `group bool` (true = server-header
row). Minimal, explicit, used only by the MCP path; the Tools/Skills loaders
never set it.

**`loadMCPItems(get func() []ToolInfo) []resourceItem`** (mirrors
`loadToolItems`/`loadSkillItems`):
- `get()`, keep only `Name` with prefix `mcp__`.
- Parse: strip `mcp__`, split on the **first** `__` → `server` = left,
  `toolName` = right. The tool name may itself contain `__` (e.g.
  `mcp__srv__do__thing` → server `srv`, tool `do__thing`), so split once. A name
  with no second `__` is malformed and skipped.
- Group by server; sort server names; within each, sort tool names.
- Emit per server, in order:
  - **header**: `{kind: resourceMCP, group: true, name: server,
    usage: "<n> tool(s)", canToggle: false, detail: mcpServerDetail(server, toolNames)}`.
  - **per tool**: `{kind: resourceMCP, group: false, name: toolName,
    desc: cleanMCPDescription(description), canToggle: false,
    detail: mcpToolDetail(server, toolName, description, schema)}`.
- Built already grouped/sorted; **not** passed through `sortResourceItems`
  (constructing in order keeps each header leading its group deterministically).
- All MCP rows are **read-only** (`canToggle: false`): MCP tools stay toggleable
  from the Tools tab by their real registered name; toggling here would need the
  namespaced-name round-trip and risk diverging enabled-state between the two
  tabs — out of scope.

**`resourceListLabel` gains a `resourceMCP` branch** (Tools/Skills untouched):
- header (`group`): `"<server>  (<n> tool(s))"`, no checkbox slot — reads as a
  section heading.
- tool: `"  " + <toolName>` — a two-space indent conveys nesting, no checkbox,
  the bare name gets ~20 of the 22 columns. `navigate_page`, `click`, `fill`,
  etc. render in full; only the longest names (e.g. `performance_start_trace`)
  clip, and the full name is always in the detail pane.

### Search that preserves grouping — `filterMCPItems`

The generic `filterResources` would strip server headers (empty desc) and orphan
matching tool rows. So the MCP tab uses a small group-aware filter instead;
`render()` gets a one-line branch: `if curKind == resourceMCP` use
`filterMCPItems(allMCP, query)`, else `filterResources(currentItems(), query)`.
Tools/Skills behavior is unchanged.

`filterMCPItems(items, q)` walks the flat, ordered list using the `group` flag as
the group delimiter and keeps a coherent server→tools view:
- query empty → all items.
- a **server group is kept whole** (header + all its tool rows) when the server
  name matches `q` — so searching `chrome-devtools` shows that server and every
  tool it advertises.
- otherwise, individual **tool rows that match** `q` (name or cleaned desc) are
  kept, **and their server header is retained** above them — so searching
  `navigate` shows the matching tools *with* their server context, never
  orphaned.
- a server contributing neither a name match nor any tool match is dropped
  entirely (header included).

This resolves both usability concerns at once: bare tool names fit the column,
and grouping/server-context survives search without a compound `server/tool`
label.

### Detail renderers (local, mirroring `toolDetail`)

- `cleanMCPDescription(raw)` strips the model-targeted trailer
  `newMCPTool` appends (`"\n\nWhen calling this tool, use its full name …"`) by
  cutting at that fixed marker. The human-facing pane then shows only the
  server's real tool description; the agent-callable name is surfaced explicitly
  (below) instead. Pure cosmetic; if the marker is absent the description is
  shown unchanged.
- `mcpToolDetail(server, tool, rawDesc, schema)` →
  `MCP tool: <tool>` / `Server: <server>` / `Registered as: mcp__<server>__<tool>`
  (the agent-callable name) / `Description` (cleaned) / `Input schema` — same
  layout as `toolDetail` for a consistent pane.
- `mcpServerDetail(server, toolNames)` → `MCP server: <server>`, the tool count,
  a bulleted list of the tools it advertises, and a line noting they are
  registered as `mcp__<server>__<tool>` and callable by the agent.

### Wiring

- `currentItems()` (line ~131): replace `return nil // MCP: no servers until #36`
  with `return allMCP`, where `allMCP = loadMCPItems(w.handlers.GetTools)` is
  captured in the browser-state block (~125) and refreshed in `catSel.OnChange`
  and `toggleSelected` alongside `allTools`/`allSkills` (cheap; keeps the tab
  live).
- `mcpPlaceholder` (~445): rewrite — drop the `#36`/"lands later" wording; say
  the tab is empty because **no MCP servers are configured or connected**, with
  a one-line pointer to the `mcp_servers` config key. Still shown only when the
  MCP item count is 0 and the query is empty (unchanged gating), so a non-empty
  server list never shows it.
- Package doc (~42): update — the MCP tab lists the configured/connected MCP
  servers and the tools each advertises (`tools/list`), derived from the
  registered `mcp__*` tools.

### Reused, unchanged plumbing

`render()` (only the filter-selection branch added), `OnSelect`, the detail
pane, `installResizeReflow`, `loadToolItems`/`loadSkillItems`,
`sortResourceItems`, `filterResources` (Tools/Skills), and `toggleSelected`
(no-ops on `canToggle:false` MCP rows) are otherwise untouched.

## Resolving the critique

- **Tool-row truncation (material):** fixed. Tool rows now show the **bare tool
  name** under a per-server header, not `server/tool`. For `chrome-devtools-mcp`
  / `playwright-mcp` the tool name is fully visible instead of being eaten by a
  19-char server prefix.
- **Search breaking grouping:** fixed by `filterMCPItems` (group-aware), which
  keeps headers with their matching tools and supports searching by server name
  — so the bare-name label stays coherent under search.
- **Description noise in the pane:** fixed by `cleanMCPDescription`, which trims
  the appended "use its full name" trailer from the human-facing detail while
  surfacing the namespaced name as an explicit `Registered as:` line.
- **Configured-but-failed-server gap (literal acceptance wording):** documented
  as an explicit decision (below), not just an open question.
- **Stub-server test promoted from optional to required** (Testing, below).
- **Tree-can-nest note:** acknowledged below.

## Decision: which servers appear (vs. literal acceptance wording)

The client-side-derive path surfaces a server **only if it registered ≥1 tool**
— i.e. it connected and `tools/list` returned tools. A server that is
permission-**denied**, **unreachable**, or advertises **zero tools** will not
appear, leaving the empty state even though `mcp_servers` is technically
non-empty. This bends the literal criterion ("placeholder no longer shows when
`mcp_servers` is non-empty").

**Decision: ship the derive path and accept this.** It covers the issue's real
target — `chrome-devtools-mcp` and `playwright-mcp` both advertise many tools, so
both render correctly. Showing configured-but-not-connected servers *with a
connection status* would require the FALLBACK `Handlers.GetMCPServers` accessor +
`GET /api/mcp`, touching `api_client.go`/`remote_handlers.go`/`internal/server`
— exactly the files with in-flight work this design is meant to avoid. The
rewritten empty-state wording ("configured or connected") keeps the message
honest. If maintainers later want failed-server visibility, the accessor is an
additive follow-up. **This trade-off is called out so it is not a surprise at
review.**

## Holistic note: the Tree can nest

turbotui's `TreeNode` supports real parent/child nesting with expand markers
(`turbotv/widget_tree.go`), so a collapsible server node with tool children is an
alternative idiom. The flat header-row emulation is a deliberate, lower-risk
choice: real nesting would require restructuring the shared `render()` build
path (which Tools/Skills also use), adding regression surface to those tabs for
no functional gain here. The header-row grouping is not forced by the toolkit;
it is chosen to keep the change additive and confined.

## User-facing behavior

- Resources → **MCP** tab: with connected `mcp_servers`, each server shows as a
  heading (`<server>  (n tools)`) with its tools listed beneath as readable
  bare names. Arrow/click selects; the detail pane shows the server summary or
  the tool's cleaned description + input schema + its agent-callable `mcp__…`
  name.
- Search filters by tool or server name and keeps server grouping intact.
- With no connected MCP servers, the tab shows the rewritten empty state — never
  the old `#36` placeholder.
- MCP tools remain agent-callable and still appear under the Tools tab
  (unchanged).

## Testing

Confined to `ui/tui/resources_dialog_test.go`. No dependency on
`chrome-devtools-mcp`/`playwright-mcp`.

Unit tests (synthetic `[]ToolInfo`):
- `loadMCPItems` with `mcp__srvA__greet`, `mcp__srvA__echo`, `mcp__srvB__nav`,
  plus a non-`mcp__` tool → assert headers for `srvA (2)` and `srvB (1)` in
  sorted order, bare-name tool rows under the right server (sorted), `group`
  flags set, non-MCP tools excluded.
- Parse edges: `mcp__srvA__do__thing` → server `srvA`, tool `do__thing`;
  `mcp__bogus` (no second `__`) skipped.
- `resourceListLabel` for an MCP header vs. tool row → heading vs. indented bare
  name; assert a long tool name is not prefixed by the server (truncation
  budget preserved).
- `filterMCPItems`: search a server name keeps its header + all tools; search a
  tool name keeps matching tools **with** their headers; a non-matching group is
  dropped.
- `mcpToolDetail`/`mcpServerDetail` contain the tool name, server, namespaced
  `mcp__…` name, and schema; `cleanMCPDescription` strips the
  "use its full name" trailer (and is a no-op when absent).
- `emptyDetail(resourceMCP, 0, "")` → new wording; **update the stale
  `resources_dialog_test.go:188` assertion** from `#36` to the new text.

Required stub-server end-to-end test (closes the registry→`GetTools`→`ToolInfo`→
`loadMCPItems` seam the critique flagged) — lives in `ui/tui` since it already
imports `internal/gogent`:
- stand up an in-process MCP stub over httptest (the same shape as
  `internal/gogent/mcp_test.go`'s `mcpTestServer`), build a real `Gogent` with an
  allowing permission prompter and `MCPServers` pointing at the stub, call
  `StartMCPServers()`, then build a `GetTools` closure that maps
  `g.GetToolRegistry().List()` to `[]ToolInfo` exactly as
  `embedded_handlers.go` does, and assert `loadMCPItems(GetTools)` surfaces the
  stub's tool under its server header with the expected detail (description +
  schema + namespaced name). This exercises the real client → registry →
  namespacing → UI pipeline against a live (stub) MCP server.

**Live verification with `chrome-devtools-mcp`/`playwright-mcp` requires
Node/npx + browsers + network and is NOT part of the Pi5 gate.** PR body:
"Automated tests use an in-process stub MCP server (including an end-to-end
`StartMCPServers` → registry → MCP-tab pipeline test); live verification with
chrome-devtools-mcp and playwright-mcp is a manual maintainer step (verified
locally / to be verified by maintainer)."

## Criteria assessment

**(1) Goal match — OK.** Wires the stubbed MCP tab to live registry data without
rebuilding MCP; rests on the verified `mcp__<server>__<tool>` contract; confirms
`StartMCPServers` is already invoked everywhere. One disclosed caveat
(denied/unreachable/zero-tool servers don't appear) is an explicit, justified
decision, not silent scope-cutting.

**(2) Usability — OK (concerns resolved).** Bare tool names under per-server
headers fit the 22-col list, so the named servers' tools are readable;
`filterMCPItems` keeps grouping coherent under search and supports server-name
search; `cleanMCPDescription` removes model-targeted noise from the pane. The
tab behaves like Tools/Skills (list + search + detail), user-driven, with server
origin surfaced rather than hidden.

**(3) No regressions — OK.** Additive, confined to the `resourceMCP` branches +
a one-line `render` filter branch + one new unexported field. Tools/Skills
loaders, sort, generic filter, toggle, and resize-reflow are untouched;
`internal/mcp` and `internal/gogent/mcp.go` are read-only here. The single stale
test assertion is updated. gofmt/vet/build/lint/test per `[[dev-gate]]` (no
`-race` on Pi5); pre-existing `TestUserSessionSendMessage` 404 remains the only
accepted failure.

**(4) Holistic — OK.** Correct layer (a UI tab fed by an existing handler);
turbotui consumed, never modified; no `go.mod` bump; no new deps; reuses the
existing MCP client + registry; zero conflict with #482/#486/#490. The flat
grouping vs. native Tree nesting trade-off is acknowledged and justified.

## Regression risks

- **Stale placeholder test** (`resources_dialog_test.go:188`, expects `#36`) —
  must be updated; identified and planned.
- **Name-parse coupling** to the intentional `mcp__<server>__<tool>` contract
  (`newMCPTool`, asserted in `internal/gogent/mcp_test.go`). Acceptable in-repo
  coupling; if the prefix changes the deriver changes with it, and the e2e test
  would catch a drift.
- **`cleanMCPDescription` marker match** is a fixed-string cut; if `newMCPTool`'s
  trailer wording changes it silently stops trimming (cosmetic only, never drops
  real description text). Covered by a unit test asserting the trailer is gone.

## Open questions

1. **Failed/zero-tool server visibility** — design decision above is to *not*
   show them (derive path), to stay conflict-free. Confirm acceptable, or
   greenlight the additive `GetMCPServers` + `/api/mcp` follow-up to show
   connection status for configured-but-unconnected servers.
2. **MCP-tab toggling** — proposed read-only here (toggle stays on the Tools
   tab). Confirm; enabling it later is a small `SetToolEnabled` wiring with the
   namespaced name.
