# Design: Language Server Protocol (LSP) Support

Status: **Draft** — for review
Audience: gogent maintainers
Prior art in this repo: the MCP integration (`internal/mcp`,
`internal/gogent/mcp.go`), the shell tool, the tool registry (`internal/tool`),
the permission service (`internal/permission`), the file machinery
(`internal/fileops`: `FileMutation`, `Checkpointer`), and the TUI status panes.

---

## 1. Motivation

gogent understands code as text. It reads files, greps, and runs the compiler,
linter, and tests through the **shell**. It has no *semantic* view: no "go to
definition", "find references", "hover for type", or live, per-file diagnostics —
the things every editor gets for free from a **language server**.

The single highest-value capability LSP adds is a **feedback loop**: after the
model edits a file, it can ask "what's wrong with this file *now*?" and get
structured, language-aware diagnostics back in milliseconds, without shelling out
to a whole-project build. Navigation and symbol understanding come close behind.

The posture of this design is **maximize value to the agent**, not maximize spec
coverage. We mirror how working agent harnesses (e.g. OpenCode) actually do it: a
thin, pragmatic, library-backed client that implements the slice with real agent
value and *politely declines* the rest. Implementing editor-only protocol
features for the sake of completeness is an explicit anti-goal. The protocol is
large; the part an agent benefits from is small and well understood.

A hard requirement runs through the whole design: **one abstraction over many
servers.** A single generic client must serve `gopls`, `rust-analyzer`,
`pyright`, `clangd`, … with **no per-language Go code**. Adding a language is a
config entry, never a code change. `gopls` ships as the proof of concept.

---

## 2. Goals / Non-Goals

### Goals
- A **single, language-agnostic LSP client** (`internal/lsp`) with **no
  per-language Go code**. All per-language knowledge lives in a declarative
  `ServerConfig`.
- **Capability-driven dispatch.** Every request is gated on the server's
  negotiated capabilities (initialize result + dynamic registration).
  "Unsupported by this server" is a clean, expected result — never an assumption.
  Negotiation also covers the **result-shaping sub-capabilities** our curated ops
  depend on, and the edge layer normalizes the union response shapes servers may
  return (§7.2, §10).
- **Diagnostics as the agent feedback loop** (Tier 1, §3) — push *and* pull,
  debounced, deduped, with a bounded wait for freshness keyed on document
  **version correlation**.
- **High-value navigation/understanding tools** (Tier 2): definition family,
  references, hover, document/workspace symbols, call hierarchy.
- **Mutations** (Tier 3): rename, code actions, formatting — preview-then-apply,
  routed through gogent's existing write/edit permission and checkpoint/undo
  machinery. A first-class deliverable of this design, not a later add-on.
- **Lazy, per-server lifecycle** reusing the MCP integration's shape:
  no-app-dependency package, config mirroring `MCPServerConfig`, `Start…/Close…`
  on `Gogent`, permission-gated launch.
- **Zero-config Go support** when `gopls` is on `PATH`.

### Non-Goals
- **Full LSP 3.17 coverage.** We implement the slice with agent value and handle
  everything else gracefully. The underlying library defines the full type set;
  we are free to ignore most of it.
- **Editor-only features.** Completion, semantic tokens, document color,
  folding/selection ranges, inlay hints, inline values, document links, linked
  editing, monikers — out of scope (§5). Their value to a non-interactive agent is
  near zero.
- **Hand-rolling the protocol.** We build on an existing typed Go LSP library
  (§6).
- **Mirroring a live editor buffer.** gogent owns files on disk; document sync is
  driven by disk state and gogent's own edits, not keystroke-level mirroring.
- **Incremental document sync.** We send **full-document** `didChange` (§11.1).
  Range-based incremental sync is an editor-grade optimization with low agent
  value and real UTF-16/versioning correctness cost; it is a deferred optimization,
  not a co-equal path.
- **A standing workspace file-watcher.** We do not poll the tree to synthesize
  watched-file events for files gogent never touched; that drifts toward
  hand-rolling a filesystem watcher (§11.5). A bounded, scoped check is a deferred
  optimization, added only if a concrete staleness bug demands it.
- **Replacing batch build/test.** "Does the whole project build / do the tests
  pass?" stays a shell command (§4).
- **Auto-installing language servers.** A missing server command is skipped with a
  warning. OpenCode-style auto-install is a possible future enhancement, not core.

---

## 3. What we expose and why — value tiers

The model sees a curated, permission-gated tool set, organized by how much value
it delivers to an agent. **All three tiers are in scope and fully specified here**
— the tiering prioritizes value and orders implementation, it does not gate what
ships. Every tier is covered end-to-end by the `Client` interface (§7.2) and the
`lsp_*` tools (§12). Tier order is also roughly the build order (§14).

### Tier 1 — Diagnostics (the point)
The reason to do this at all. After an edit, the agent asks `lsp_diagnostics` for
a file and gets structured findings (severity, code, source, message, range)
without a project-wide build. This is the tight semantic feedback loop a shell
build cannot give per-file or per-keystroke-of-progress.

This tier has real engineering substance (§11):
- **Both transports.** Push (`textDocument/publishDiagnostics`) *and* per-file
  pull (`textDocument/diagnostic`) when the server advertises the pull capability.
  Whole-project pull (`workspace/diagnostic`) is **not** a transport here: no tool
  consumes it and project-wide checks are the shell's job (§4); we would re-add it
  only behind a concrete tool that needs it.
- **Debounce** (~150ms) so a burst of pushes collapses to one settled set.
- **Dedup** by `(code, severity, message, source, range)`.
- **Bounded wait for freshness, keyed on version.** Each `didOpen`/`didChange`
  carries a monotonically increasing document version. The freshness wait blocks
  until `publishDiagnostics` arrives reporting **the last version we sent** for the
  document — the standard, reliable correlation signal that the findings reflect
  the latest content, not a stale-empty set from before the edit. Keying on the
  last version sent via *either* `didOpen` or `didChange` means a freshly opened,
  unedited file (the headline `lsp_diagnostics` case) correlates on its `didOpen`
  version rather than falling through to the slower fallback path. Fallbacks
  (§11.4) handle servers that omit the version field and pull-only servers.

### Tier 2 — Navigation & understanding (read-only)
`definition` (with declaration / type-definition / implementation selected by a
`kind` arg), `references`, `hover`, `document_symbols`, `workspace_symbols`,
`call_hierarchy`. These are pure reads; they need no extra permission beyond the
server already running.

### Tier 3 — Mutations
`rename`, `code_actions`, `formatting`. **Preview-then-apply**: the tool returns
the proposed `WorkspaceEdit` for inspection; applying it routes through gogent's
existing write/edit permission and `Checkpointer` (undo) machinery. This tier
must materialize edits that servers return *lazily* — a code action often arrives
with no edit attached and must be resolved via `codeAction/resolve` before a
`WorkspaceEdit` exists to preview (§12). This is where we handle
`workspace/applyEdit`. `workspace/executeCommand` is a **distinct, higher-risk**
action with its own gate and allow-list (§12) — its side effects are *not*
checkpointable.

---

## 4. Division of labor: shell for batch, LSP for semantics

| Question | Tool |
|---|---|
| "Does the project build? Do the tests pass?" — whole-project, pass/fail | **shell** (`go build ./...`, `go test ./...`, `cargo build`, `pytest`, …) |
| "What's wrong with *this file* right now?" — live, per-file, structured | **LSP** (`lsp_diagnostics`) |
| "Where is this defined? Who calls it? What's its type?" | **LSP** |

As part of this work we **remove the existing `diagnostics` and `verify` tools**
(`internal/diagnostics`, `internal/verify`, their `internal/tool` wrappers, and
the `ActionDiagnostics` / `ActionVerify` permission actions). They were
per-language batch runners with bespoke output parsers — exactly the per-language
coupling this design exists to avoid. A per-language compiler-output parser does
not scale; it is the same coupling, just on the output side. And LSP has **no
test-execution method**, so running a test suite is intrinsically a shell
concern. Whole-project build/test stays a shell command; LSP provides live,
per-file semantics. Trade-off: the model reads raw shell output and exit status
for batch checks; structured per-file findings come from `lsp_diagnostics`.

---

## 5. Explicitly out of scope (and why)

The chosen library generates types for the entire protocol. We simply **do not
use or expose** the following, because they serve an interactive editor, not a
headless agent:

| Feature | Why not |
|---|---|
| `completion` / `completionItem/resolve` | The agent writes whole edits; it does not type characters and ask for completions. |
| `semanticTokens/*` | Syntax-highlighting data for a renderer. No agent value. |
| `documentColor` / `colorPresentation` | Color swatches in a gutter. |
| `foldingRange` / `selectionRange` | Editor cursor/fold affordances. |
| `inlayHint` / `inlineValue` | Inline editor annotations / debugger overlays. |
| `documentLink` | Clickable links in an editor. |
| `linkedEditingRange` | Simultaneous multi-cursor editing. |
| `moniker` | Cross-repo code-intel indexing, not interactive use. |

If a concrete agent need appears later, any of these is reachable through the
library without redesign — but none is surfaced as a tool today.

---

## 6. Don't hand-roll: the library choice

**Decision: build on `go.lsp.dev/protocol` (typed LSP messages) +
`go.lsp.dev/jsonrpc2` (Content-Length framing, request/response/notification
dispatch, id matching, server→client handler registration).**

Hand-rolling the protocol means reinventing two things that are pure cost:
1. **JSON-RPC 2.0 over `Content-Length` framing** — the read loop, id↔caller
   matching, notification vs. request demultiplexing, and a responder for
   server→client requests. `go.lsp.dev/jsonrpc2` provides exactly this, including
   registering a handler the server can call back into.
2. **Hundreds of generated message types** — every structure, enumeration, and
   params/result pair in the protocol. `go.lsp.dev/protocol` provides these as
   typed Go with correct optional/pointer handling.

Writing and maintaining either by hand is waste and a bug farm: framing edge
cases, id races, and a large surface of structs that drift from the spec.

**This explicitly relaxes the prior std-lib-only rule for `internal/lsp`.** The
dependency is justified: the alternative — `sourcegraph/jsonrpc2` plus a
hand-written subset of types — saves nothing real. We would still write the
framing glue and would now own a partial, hand-curated type set that rots against
the spec and silently mishandles fields we did not model. The typed library wins
because per-language correctness and capability negotiation depend on having the
*real* types and on never guessing at wire shapes. The rest of gogent's rules
(no app dependency in the package, clean lifecycle) are unchanged.

**One caveat we design around up front.** `go.lsp.dev/protocol` is loosely
maintained and tracks roughly LSP 3.16; the 3.17 `general.positionEncodings`
negotiation is **not exposed** by its capability types. We therefore do **not**
attempt utf-8 negotiation: **utf-16 is the only wire encoding**, and gogent's edge
layer owns correct rune→UTF-16 column conversion (§11.3). Where the library lags
the spec on a field we actually need, we add a thin local type rather than fork —
but for the agent-value slice this is rarely required.

---

## 7. The multi-language abstraction — the heart of the doc

This is the load-bearing requirement: **a single generic client, no per-language
Go code.** Two ideas make it work — a declarative `ServerConfig`, and
capability-driven dispatch.

### 7.1 Per-language knowledge lives only in `ServerConfig`

```go
type ServerConfig struct {
    Name        string            // "gopls" — the client/process identity
    LanguageID  string            // default LSP languageId: "go", "rust", "python"
    Languages   map[string]string // optional per-extension override: ".tsx" → "typescriptreact"
    Extensions  []string          // [".go"] — routing key
    Command     string            // "gopls"
    Args        []string          // ["serve"]
    Env         map[string]string
    RootMarkers []string          // files that mark a project root: ["go.mod","go.work"]
    InitOptions map[string]any    // initializationOptions
    Settings    map[string]any    // workspace/configuration source
    AllowedCommands []string      // executeCommand allow-list (§12); empty ⇒ none allowed
}
```

Everything a server needs is data:
- **Routing is by file extension** → `ServerConfig`.
- **Workspace root is found by walking up** from the file for the configured
  `RootMarkers` (`go.mod`, `Cargo.toml`, `package.json`, `pyproject.toml`, …),
  falling back to the gogent workspace root.
- **`LanguageID`** (or the per-extension `Languages` override) populates
  `didOpen`'s language id. The override exists because some servers serve several
  languageIds from one process — `.ts`→`typescript`, `.tsx`→`typescriptreact`,
  `.js`→`javascript`. Modeling them as separate configs would spawn duplicate
  processes; instead one config, keyed by **server name** (see §9), maps each
  extension to the correct languageId.
- **`InitOptions` / `Settings`** feed `initialize` and `workspace/configuration`.
- **`AllowedCommands`** scopes the higher-risk `executeCommand` action (§12).

Adding `rust-analyzer` is a config entry (§13), not a line of Go. **Nothing in
the client is gopls-specific.**

The model targets one process per `ServerConfig`. Single-language servers
(`gopls`, `rust-analyzer`, `pyright`, `clangd`) are the common case and need only
`LanguageID`; the `Languages` map plus server-name keying is what makes a genuine
multi-language server (e.g. `typescript-language-server`) expressible as a single
config without code.

### 7.2 Capability-driven dispatch

Servers differ wildly in what they support, and many features are only enabled
*after* startup via dynamic registration. The client therefore maintains a **live
capability table** built from two sources:
1. the `ServerCapabilities` in the `initialize` result, and
2. `client/registerCapability` / `client/unregisterCapability` notifications the
   server sends afterward.

**Every operation is gated on this table.** Before issuing, say,
`textDocument/callHierarchy`, the client checks whether the server advertises it
(for the relevant document selector). If not, the operation returns a clean,
typed `ErrUnsupported` — the tool reports "not supported by this server for this
file" and the agent moves on. The client **never assumes a feature exists.** This
is what lets one implementation serve servers with very different feature sets
without per-server special-casing.

**Capability is more than a yes/no bit — it shapes the result.** Several
high-value operations return *different response shapes* depending on the
sub-capabilities the client advertises (§10), and some require a follow-up
resolve. The negotiation contract therefore has two halves: we advertise the
sub-options our curated ops actually need, *and* the edge-conversion layer accepts
both union shapes and normalizes them:

- **`documentSymbol`** returns a flat `SymbolInformation[]` unless we advertise
  `hierarchicalDocumentSymbolSupport`, in which case it returns the nested
  `DocumentSymbol[]` tree our `[]Symbol` result models. We advertise the rich
  form *and* the edge layer accepts either union arm, synthesizing a flat tree
  from `SymbolInformation` so the `lsp_document_symbols` tree is never silently
  lost on a server that returns the flat shape.
- **`codeAction`** returns bare `Command[]` unless we advertise
  `codeActionLiteralSupport`; the edge layer accepts both the `Command` and
  `CodeAction` arms (§12).
- **`hover`** quality depends on the advertised `contentFormat` preference
  (markdown vs. plaintext).
- **`rename`** quality depends on `prepareSupport` (so `textDocument/prepareRename`
  validates the position and returns the precise rename range).

```go
type Client struct { /* conn, capabilities table, open-doc table (versioned), diag cache, mu */ }

// Curated, language-independent operations — the tool-facing surface.
// Each consults the capability table first and may return ErrUnsupported.
func (c *Client) Definition(ctx, file string, pos Position, kind DefKind) ([]Location, error)
func (c *Client) References(ctx, file string, pos Position, inclDecl bool) ([]Location, error)
func (c *Client) Hover(ctx, file string, pos Position) (Hover, error)
func (c *Client) DocumentSymbols(ctx, file string) ([]Symbol, error)
func (c *Client) WorkspaceSymbols(ctx, query string) ([]Symbol, error)
func (c *Client) CallHierarchy(ctx, file string, pos Position, dir Direction) ([]CallItem, error)
func (c *Client) Diagnostics(ctx, file string) ([]Diagnostic, error)
func (c *Client) Rename(ctx, file string, pos Position, newName string) (WorkspaceEdit, error)
func (c *Client) CodeActions(ctx, file string, rng Range) ([]CodeAction, error)
func (c *Client) Format(ctx, file string) (WorkspaceEdit, error)
```

The boundary types are a thin gogent-owned layer (1-based positions, file paths
not URIs) converted to/from the library's `protocol.*` types at the edge. The
edge is also where union shapes are normalized (above):

```go
type Position struct { Line, Character int } // 1-based at the tool edge; 0-based UTF-16 on the wire
type Range     struct { Start, End Position }
type Location  struct { Path string; Range Range }
type Diagnostic struct { Range Range; Severity int; Code, Source, Message string }
type WorkspaceEdit struct { Changes map[string][]TextEdit } // keyed by file path
```

---

## 8. Architecture overview

```
        ┌──────────────────────────────────────────────────────────────┐
        │ internal/gogent (wiring & lifecycle)                          │
        │  StartLSPServers() / CloseLSPServers()                        │
        │  implements Host (configuration, applyEdit, log/progress)     │
        └───────────────┬───────────────────────────────┬──────────────┘
                        │                               │ callbacks (Host)
        ┌───────────────▼───────────────┐   ┌───────────▼──────────────┐
        │ internal/lsp.Manager           │   │ Host (in internal/gogent) │
        │  - clients keyed by server name │   │  - configuration ← Settings│
        │  - lazy spawn (single-flight)   │   │  - applyEdit → write/edit │
        │  - route by ext                 │   │    + Checkpointer (undo)  │
        │  - per-server root detection    │   │  - log / progress         │
        │  - diagnostics cache per URI     │   └───────────▲──────────────┘
        └───────────────┬────────────────┘               │
                        │                                │
        ┌───────────────▼──────────────────────────────────────────────┐
        │ lsp.Client  (generic; identical for every language)           │
        │  lifecycle · capability table · doc sync · diagnostics         │
        │  curated capability-gated ops (Definition/References/…)        │
        └───────────────┬──────────────────────────────────────────────┘
                        │  go.lsp.dev/protocol + go.lsp.dev/jsonrpc2
        ┌───────────────▼────────────┐
        │ gopls / rust-analyzer / …   │  (stdio subprocess)
        └─────────────────────────────┘

  internal/tool   — curated lsp_* tools over Manager (§12)
```

### Package layout
```
internal/lsp/
  client.go     // Client: lifecycle, capability table, doc sync, re-sync-on-access, curated ops
  capability.go // live capability table + registered file-watcher globs (init result + dyn registration)
  diagnostics.go// per-file push+pull cache, debounce, dedup, version-keyed freshness wait
  watch.go      // watched-file matching + didChangeWatchedFiles emission (fileops mutation stream)
  handlers.go   // server→client request/notification handlers (via Host)
  host.go       // Host interface (configuration, applyEdit, log, progress)
  manager.go    // Manager: per-server clients, single-flight spawn, routing, root detection, cache
  config.go     // ServerConfig
  *_test.go     // in-memory fake server over a pipe; gopls integration (build-tag)
internal/gogent/
  lsp.go        // StartLSPServers / CloseLSPServers / Host implementation
internal/tool/
  lsp.go        // RegisterLSPTools(*lsp.Manager)
internal/config/
  config.go     // LSPServerConfig + Config.LSPServers
internal/permission/
  permission.go // ActionLSP (launch gate), ActionLSPCommand (executeCommand);
                // mutations reuse ActionWrite
```

---

## 9. Client & Manager

### Manager
```go
type Manager struct { /* workspaceRoot, configs, clients-by-name, diag cache, host */ }
func NewManager(workspaceRoot string, configs []ServerConfig, host Host) *Manager
func (m *Manager) ClientForFile(ctx, path string) (*Client, error) // lazy spawn + cache
func (m *Manager) Shutdown()
```

- **Lazy spawn on first matching-file use.** LSP servers are heavy (gopls indexes
  the module); configuring five servers does not launch five.
- **Reuse across requests.** One server per `ServerConfig`, kept alive for the
  session.
- **Clients are keyed by server name (`ServerConfig` identity), not by language.**
  Routing maps the file's extension to a `ServerConfig`, and the client cache is
  keyed by that config's `Name`. This is what lets one process serve several
  languageIds (§7.1): the `didOpen` languageId is resolved per file from
  `Languages`/`LanguageID`, while all of them share one client. `ErrNoServer`
  when no config matches (a clean, non-fatal result).
- **Per-server root detection.** On first use, walk up from the file for the
  config's `RootMarkers` to choose the `rootUri`.
- **Diagnostics cache keyed by URI**, fed by push and pull (§11).
- **Clean shutdown.** `shutdown` request → `exit` notification → kill the
  subprocess if it lingers. Mirrors `internal/mcp` stdio `Close()`.

### Lazy-launch gate
The first `ClientForFile` for a server checks `ActionLSP` (resource = server
name), then spawns. This mirrors `ActionMCP` gating an MCP server launch once.

### Concurrency & cancellation model
Tool calls run concurrently and hit `ClientForFile` and the curated ops while the
`jsonrpc2` read loop is mutating shared state. The model is explicit:

- **One read loop owns inbound traffic.** All server→client notifications and
  requests (publishDiagnostics, register/unregister, progress, applyEdit) are
  dispatched on the single `jsonrpc2` read goroutine. Outbound requests block
  their caller's goroutine on the matched response.
- **Shared tables are guarded.** The live capability table (mutated by
  register/unregister), the versioned open-doc table, and the per-URI diagnostics
  cache are each protected by the `Client` mutex (or owned by one goroutine behind
  a channel). No table is read without the lock.
- **Freshness waiters register before the version is sent.** This is a real race,
  not an implementation detail: a fast `publishDiagnostics` can arrive before a
  waiter subscribes and be dropped. So the order is fixed — under the lock, record
  interest in "version N for URI", *then* send the `didOpen`/`didChange` that bumps
  to version N. A push that arrives for N finds the waiter already present (§11.4).
- **Spawn is single-flight.** Two concurrent first-touches of the same server must
  not launch two processes; `ClientForFile` deduplicates concurrent spawns per
  server name (e.g. `singleflight` keyed on `Name`) and shares the one client.
- **Cancellation maps to `$/cancelRequest`.** The client keeps an in-flight
  request→id map; when a caller's context is cancelled, it emits `$/cancelRequest`
  for that exact id and unblocks the caller. The id map is cleared on response.

---

## 10. Lifecycle, capabilities & server→client callbacks

### Lifecycle
1. **`initialize`** — send `rootUri` (from root detection), `workspaceFolders`,
   `InitOptions`, and a `ClientCapabilities` that advertises **only what we use**,
   but advertises it *correctly* — including the result-shaping sub-options the
   curated ops depend on (§7.2):
   - text sync (full + didSave);
   - the Tier 1–3 language features with `dynamicRegistration: true`;
   - `textDocument.documentSymbol.hierarchicalDocumentSymbolSupport: true` (so we
     get the `DocumentSymbol[]` tree, not a flat list);
   - `textDocument.codeAction.codeActionLiteralSupport` with the kinds we use, and
     `dataSupport` + `resolveSupport` so lazy code actions can be resolved (§12);
   - `textDocument.rename.prepareSupport: true`;
   - `textDocument.hover.contentFormat: ["markdown","plaintext"]`;
   - `workspace.configuration`, `workspace.workspaceFolders`,
     `workspace.didChangeWatchedFiles` (`dynamicRegistration: true`, so servers may
     register file-watch globs — §11.5);
   - `workspace.applyEdit` (with `documentChanges`); and
   - `window.workDoneProgress`.

   We do **not** advertise `positionEncodings` (unavailable in the library, §6);
   the wire encoding is utf-16. We do not advertise capabilities for features we
   never call; that keeps servers from enabling machinery we would only have to
   ignore. The sub-options above are the deliberate exception — they shape results
   for ops we *do* call, so omitting them silently degrades exactly the
   cross-server fidelity this abstraction exists to provide.
2. **`initialized`** notification.
3. **Capability table built up over time** (§7.2). gopls and others register most
   features *after* `initialized` via `client/registerCapability`; the client
   keeps the table live and consults it before every request.
4. **Configuration** — push `workspace/didChangeConfiguration` and answer
   `workspace/configuration` pulls from `ServerConfig.Settings`.
5. **Shutdown** as above.

### Server→client callbacks — the minimum to keep real servers happy
`go.lsp.dev/jsonrpc2` lets us register a handler for inbound traffic. We handle
exactly what gopls et al. need to function:

| Inbound | Handling |
|---|---|
| `workspace/configuration` | Answer from `ServerConfig.Settings`, **scope-aware** (per requested section/scopeUri). |
| `client/registerCapability` / `unregisterCapability` | Maintain the live capability table (§7.2). A registration for `workspace/didChangeWatchedFiles` also **records its watcher globs** so we can emit matching notifications (§11.5) — not merely flip a capability bit. |
| `workspace/workspaceFolders` | Return the Manager's known roots. |
| `window/workDoneProgress/create` + `$/progress` | Track the token; used as the *fallback* diagnostics-readiness signal (server idle ⇒ settled, §11.4). |
| `workspace/diagnostic/refresh` | Invalidate the per-file pull cache so the next `lsp_diagnostics` re-pulls. |
| `$/cancelRequest` (outbound, on ctx cancel) | Cancel in-flight requests when the caller's context is done (§9). |
| `workspace/applyEdit` | Route through `Host.ApplyEdit` (§12), gated via `ActionWrite` + `Checkpointer`; a denied or stale edit returns `applied:false`. |

Politely handled / sane defaults (headless agent, no editor UI):

| Inbound | Handling |
|---|---|
| `window/showMessage` / `logMessage` / `$/logTrace` / `telemetry/event` | **Log** to gogent's log stream. No prompt. |
| `window/showMessageRequest` | Log; return `null` (no selection). We deliberately do **not** pick the first/default action: it is often the affirmative one and may trigger a command or mutation, which would be an un-gated decision for a headless agent. |
| `window/showDocument` | Log; return `success:false` (we are headless). |

Surfacing progress/status in the TUI later is a **nice-to-have, not core**: the
defaults above keep every real server working with no UI attached.

---

## 11. Document synchronization

gogent owns files on disk (not an editor buffer), but the server needs a current
view to produce correct diagnostics and navigation.

### 11.1 Open / change / close
- **Any tool access to a file ensures `didOpen` first, idempotently.** The
  open-doc table records whether a URI is open; the first tool that touches a file
  (diagnostics, hover, definition, …) opens it with on-disk text + the resolved
  languageId + an initial version. Asking `lsp_diagnostics` on a file gogent has
  not edited therefore opens it and yields push diagnostics. "When to open" is
  common to every code path; only the *close/keep-fresh* policy differs (§11.2).
- **`didChange` is full-document.** On each change we resend the whole document
  with the next version number. This is correct by construction and within spec
  even for incremental-capable servers, and it avoids UTF-16 range math, per-edit
  version coordination, and a full-sync fallback to maintain. Incremental sync is a
  deferred optimization (§2 Non-Goals), not part of the PoC.
- **Re-sync from disk on tool access.** Before issuing a request against an
  *already-open* document, the client cheaply compares the file's current
  size+mtime (and, on a mismatch, content hash) against what was last synced. If
  they differ — the file changed out-of-band — it reads the file and `didChange`s
  to the new version *before* the request (and before any freshness wait, §11.4).
  This closes the staleness window for changes that did not come through gogent's
  own write path, and is the primary defense for out-of-band edits to open files
  (§11.5).
- **`didSave`** (with text when the server requested it) and **`didClose`**.

### 11.2 Keeping the server view fresh after gogent edits
The opening trigger (§11.1) is shared; this section is only about what happens to
an *already-open* document and when it closes.

**Decision (deliberate coupling):** the `Manager` subscribes to `internal/fileops`
mutations (`FileMutation` / `Checkpointer` write path) and pushes full-document
`didChange`/`didSave` for any open document gogent edits. This guarantees the
server's view tracks gogent's writes without the agent doing anything, and it is
what lets the version-keyed freshness wait (§11.4) work: the edit and the
`didChange` version are coordinated in one place.

The looser alternative is **open-on-demand**: open a file when a tool touches it
and `didClose` immediately after, with no subscription. It is simpler and lower
coupling but yields colder diagnostics (the server re-indexes on each open) and
misses cross-file effects of an edit. We accept the `fileops` coupling for the
warmer, more correct feedback loop that Tier 1 depends on.

The `fileops` subscription is necessary but **not sufficient**: agents constantly
mutate files outside it — shell `sed`/`patch`, formatters, `go generate`, codegen,
`git checkout/stash`, dependency installs. Those changes are handled by the
re-sync-on-access check (§11.1) for open documents, so an already-open document
never serves content that no longer exists on disk.

### 11.3 Positions and encoding
Positions are **1-based at the tool boundary** (what the model speaks) and
**0-based UTF-16 on the wire** (what LSP mandates). Because the library does not
expose `positionEncodings` (§6), **utf-16 is the only wire encoding** — there is no
utf-8 negotiation path to take. gogent's edge layer therefore **owns correct
rune→UTF-16 column conversion**: a character offset on a line is the number of
UTF-16 code units before it, so non-ASCII (and astral) characters must be counted
as 1 or 2 units, not as runes or bytes. This is the classic off-by-column bug
class and is an explicit PoC checklist item (§14).

### 11.4 Freshness signals (diagnostics settling)
Ranked, most reliable first:
1. **Version correlation (primary).** Track the **last version sent for the URI**,
   via either `didOpen` or `didChange`. Push diagnostics are considered *settled*
   when `publishDiagnostics` arrives carrying that same `version`. Keying on the
   last sent version (not only the last `didChange`) means a freshly opened,
   unedited file — the headline `lsp_diagnostics` case (§11.1) — correlates on its
   `didOpen` version instead of being forced onto the fallback path. The waiter is
   registered *before* that version is sent (§9) so a fast push is never dropped.
   This removes the stale-empty risk on the feature the whole design exists for.
2. **Work-done progress "idle" (fallback).** For servers that omit the optional
   `version` field on `publishDiagnostics`, treat diagnostics as settled when the
   relevant work-done progress reports completion/idle.
3. **Timeout (final fallback).** A bounded wait (~the debounce window plus a small
   ceiling) caps latency for servers that do neither.

Pull-mode (`textDocument/diagnostic`) sidesteps the question entirely: the request
itself returns the current report, so freshness is synchronous and we use it when
the server advertises pull and omits versioned push.

### 11.5 Watched files and out-of-band changes
Servers do not rely on document sync alone. gopls and rust-analyzer dynamically
register file watchers via `client/registerCapability` for
`workspace/didChangeWatchedFiles`, and use those notifications to track files they
have *not* opened: `go.mod`/`go.work`/`Cargo.toml`, generated code, and siblings
that affect analysis. If we consume the registration only to flip a capability bit
(§7.2) and never emit the notifications, a class of cross-file staleness for
gogent's own writes goes unrecovered.

So the client **honors registered watcher globs** and emits
`workspace/didChangeWatchedFiles` from a single, cheap, data-driven source:

- **`fileops` mutations** (§11.2) — gogent's own writes, classified as
  created/changed/deleted and matched against the registered globs. No
  per-language code; the globs come from the server's registration.

We deliberately do **not** run a standing workspace mtime poll to synthesize
events for files gogent never touched. A periodic tree-wide scan is unbounded in
cost on large repositories, speculative, and the most over-engineered piece of an
otherwise pragmatic design; it drifts toward hand-rolling the filesystem watcher
the posture warns against, and it is not needed for the PoC. For open documents,
re-sync-on-access (§11.1) already keeps every file under edit honest regardless of
how it changed.

If a concrete staleness bug appears for a *non-open, watched* file changed purely
out-of-band (e.g. `go.mod` edited via a shell command), the fix is a **bounded,
debounced check scoped to the registered globs only** — added then, not a standing
workspace scan. It is listed as a deferred optimization alongside incremental sync
(§2 Non-Goals).

---

## 12. Tools (model-facing) & permission

A curated set over the `Manager`. Tiers (§3) map to tools:

| Tool | Tier | Args | Result |
|---|---|---|---|
| `lsp_diagnostics` | 1 | `path` | settled, deduped diagnostics for a file |
| `lsp_definition` | 2 | `path`, `line`, `column`, `kind?` | location(s); `kind` ∈ definition/declaration/type/implementation |
| `lsp_references` | 2 | `path`, `line`, `column`, `include_declaration?` | locations |
| `lsp_hover` | 2 | `path`, `line`, `column` | `{contents}` (type/docs) |
| `lsp_document_symbols` | 2 | `path` | symbol tree |
| `lsp_workspace_symbols` | 2 | `query` | matching symbols |
| `lsp_call_hierarchy` | 2 | `path`, `line`, `column`, `direction` | incoming/outgoing calls |
| `lsp_code_actions` | 3 | `path`, `range` | available fixes/refactors, resolved to edits (preview) |
| `lsp_rename` | 3 | `path`, `line`, `column`, `new_name` | proposed `WorkspaceEdit` (preview) |
| `lsp_format` | 3 | `path` | proposed formatting edit (preview) |

Behavior:
- Tier 1–2 tools set `ReadOnly: true`.
- **Capability gating is user-visible.** When the server does not support an
  operation, the tool returns "not supported by this server for this file" — an
  expected result, not an error condition.
- **Result-shape normalization is the tool's job, not the model's.**
  `lsp_document_symbols` returns the same tree whether the server answered with
  `DocumentSymbol[]` or `SymbolInformation[]` (§7.2). `lsp_code_actions` accepts
  both the `Command` and `CodeAction` arms.
- **Routing miss is clean.** "No LSP server configured for `.xyz`" is a clear,
  non-fatal message, like a declined MCP server.
- **Tier 3 is preview-then-apply, and resolves lazy edits.** The tool returns the
  `WorkspaceEdit`; applying it goes through `Host.ApplyEdit`. Critically, gopls and
  rust-analyzer routinely return code actions with **no edit attached** — only a
  `data` payload — so before previewing, `lsp_code_actions` calls
  `codeAction/resolve` (when the server advertises resolve support) to materialize
  the real `WorkspaceEdit`. Previewing an unresolved action would show an empty
  edit; resolving first is part of the Tier 3 flow, not an afterthought. Actions
  that carry only a `Command` (no edit, no resolvable edit) are surfaced as
  `executeCommand` candidates subject to the allow-list below, never silently run.
- **`Host.ApplyEdit`** validates against current content, opens a `Checkpointer`
  turn, applies the edit (including create/rename/delete resource ops), and returns
  `applied`/`failureReason` to the server for `workspace/applyEdit`.
- **`ApplyEdit` snapshots every path a `documentChanges` entry touches, before
  performing the op.** The `Checkpointer` records each path's pre-state keyed by
  path, so undo only round-trips if every affected path is snapshotted first. Plain
  text edits touch one path; resource operations touch more: a `CreateFile`/
  `DeleteFile` must snapshot the **target** (so undo can delete the new file or
  restore the deleted one), and a `RenameFile` is delete-source + create-target, so
  it must snapshot **both source and target** in the right order. Snapshotting only
  the content-edited paths would leave renames and deletes un-undoable — explicitly
  not acceptable, and a PoC checklist item (§14).

### `workspace/executeCommand` — a distinct, higher-risk action
`executeCommand` is **not** folded into the checkpointed write scope. When the
client asks the server to run a command, the **server itself** performs arbitrary
work out-of-band: gopls commands can run `go generate`, tidy `go.mod`, regenerate
cgo, touch files, or shell out. The `Checkpointer` can snapshot only the
`applyEdit` callbacks a command happens to send back — it **cannot** snapshot or
undo the command's direct, server-side side effects. Treating it as "the same
checkpointed scope" as `applyEdit` would overclaim safety.

Therefore:
- `executeCommand` is **default-deny**. Only commands listed in the server's
  `AllowedCommands` (§13) may run; an empty list means no command executes.
- It is gated on its **own** action, `ActionLSPCommand` (resource = server name +
  command id), **not** `ActionWrite` — the prompt must make clear the agent is
  asking the server to do potentially-uncheckpointable work, distinct from an
  undoable file edit.
- Any `applyEdit` a command sends back still flows through `Host.ApplyEdit` and is
  checkpointed; the prompt language states plainly that other effects are not.

This resolves former open question #3 in the design rather than deferring it.

### Permission
- **`ActionLSP` (`"lsp"`)** gates *launching* a server (subprocess) **once per
  server** (resource = server name), exactly like `ActionMCP`. Queries against an
  already-running server need **no further prompt**.
- **Mutations (rename/format/code-action edits) reuse `ActionWrite`** and the
  `Checkpointer` — an LSP-driven edit prompts and is undoable identically to a
  normal gogent edit. No separate, surprising grant.
- **`ActionLSPCommand` (`"lsp_command"`)** is the separate, higher-risk gate for
  `executeCommand`, allow-listed per server (above).
- The removed `ActionDiagnostics` / `ActionVerify` (§4) leave with their tools.

---

## 13. Configuration

Mirror `MCPServerConfig`, adding `extensions`, `language`/`languages`,
`root_markers`, and `allowed_commands`:

```go
type LSPServerConfig struct {
    Name            string            `json:"name"`
    Language        string            `json:"language,omitempty"`        // default LSP languageId
    Languages       map[string]string `json:"languages,omitempty"`       // optional per-extension override
    Extensions      []string          `json:"extensions,omitempty"`      // routing key
    Command         string            `json:"command"`
    Args            []string          `json:"args,omitempty"`
    Env             map[string]string `json:"env,omitempty"`
    RootMarkers     []string          `json:"root_markers,omitempty"`    // e.g. ["go.mod"]
    InitOptions     map[string]any    `json:"initialization_options,omitempty"`
    Settings        map[string]any    `json:"settings,omitempty"`
    AllowedCommands []string          `json:"allowed_commands,omitempty"` // executeCommand allow-list (§12)
    Disabled        bool              `json:"disabled,omitempty"`
}

type Config struct {
    // ...
    LSPServers []LSPServerConfig `json:"lsp_servers,omitempty"`
}
```

**Default Go config** so the PoC works with zero config when `gopls` is on `PATH`:

```json
{ "lsp_servers": [
  { "name": "gopls", "language": "go", "extensions": [".go"],
    "command": "gopls", "args": ["serve"], "root_markers": ["go.work", "go.mod"] }
] }
```

**A second server, config-only** — proving "config, not code." Adding Python via
`pyright` requires no gogent changes:

```json
{ "lsp_servers": [
  { "name": "pyright", "language": "python",
    "extensions": [".py", ".pyi"],
    "command": "pyright-langserver", "args": ["--stdio"],
    "root_markers": ["pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"],
    "settings": { "python": { "analysis": { "typeCheckingMode": "basic" } } } }
] }
```

`rust-analyzer` is the same shape: `extensions:[".rs"]`,
`command:"rust-analyzer"`, `root_markers:["Cargo.toml"]`. A genuine
multi-language server uses one config with a `languages` map:

```json
{ "name": "typescript", "language": "typescript",
  "extensions": [".ts", ".tsx", ".js", ".jsx"],
  "languages": { ".tsx": "typescriptreact", ".jsx": "javascriptreact", ".js": "javascript" },
  "command": "typescript-language-server", "args": ["--stdio"],
  "root_markers": ["tsconfig.json", "package.json"] }
```

The client never learns it is talking to Rust or TypeScript — capability
negotiation (§7.2) and routing (§9, keyed by server name) handle the differences.

A **missing command is skipped with a warning** and reported cleanly by the tools
(the language simply has no server). `Disabled` skips an entry without removing
it.

---

## 14. Proof of concept (gopls) & phasing

### PoC checklist (gopls)
- `initialize` handshake completes, including the result-shaping sub-capabilities
  (§10), post-`initialized` dynamic registration, and work-done progress.
- `lsp_diagnostics` reflects live errors and clears them after a fix; the
  debounce/dedup/**version-keyed freshness** path returns settled results, not
  stale-empty ones — including a freshly opened, unedited file correlating on its
  `didOpen` version (§11.4).
- An out-of-band edit to an *open* file (e.g. `sed`/`git checkout`) is picked up by
  re-sync-on-access (§11.1), so diagnostics track current disk content rather than
  the last gogent write. A gogent write to a watched non-open file (e.g. `go.mod`)
  emits `didChangeWatchedFiles` from the `fileops` stream (§11.5).
- `lsp_definition` (all `kind`s), `references`, `hover`, `document_symbols`,
  `workspace_symbols`, `call_hierarchy` return correct results;
  `document_symbols` yields a tree (hierarchical support negotiated, §7.2).
- Columns are correct on non-ASCII lines (utf-16 conversion at the edge, §11.3).
- Tier 3: a `lsp_code_actions` result that arrives without an edit is resolved via
  `codeAction/resolve` and previews a real `WorkspaceEdit`; `lsp_rename` /
  `lsp_format` previews apply through `ActionWrite` + `Checkpointer` and are
  undoable; an allow-listed `executeCommand` prompts under `ActionLSPCommand`.
- A `WorkspaceEdit` with a `RenameFile`/`DeleteFile` resource op applies and then
  **undoes cleanly** — confirming `ApplyEdit` snapshotted both the source and
  target paths (§12).
- Concurrent tool calls and a cancelled context behave correctly: shared tables
  stay consistent and a cancel emits `$/cancelRequest` for the right id (§9).
- An unsupported operation returns a clean "unsupported by this server".
- Clean shutdown leaves no orphan `gopls` process.

### Build order (all three tiers ship)
Tiers 1–3 are all deliverables of this design; the sequence below is bring-up
order, not a scope filter. Each step is independently shippable and none is
optional — the PoC checklist above already validates all three tiers end-to-end.

1. **Client + lifecycle + capability table** on `go.lsp.dev/*`; lazy
   single-flight Manager, routing, root detection; in-memory fake-server tests.
2. **Document sync + Tier 1 diagnostics** (push+pull, debounce, dedup,
   version-keyed freshness); `lsp_diagnostics`; `fileops` subscription; `gopls`
   end-to-end.
3. **Tier 2 read tools** (definition family, references, hover, symbols, call
   hierarchy), with edge-layer union-shape normalization.
4. **Tier 3 mutations** — `applyEdit` / rename / code actions (with
   `codeAction/resolve`) / format through `ActionWrite` + checkpoint;
   `executeCommand` under `ActionLSPCommand` + allow-list.
5. **Config-only second server** (`pyright` or `rust-analyzer`) validated end to
   end to prove the abstraction; optional TUI progress/status surfacing.

---

## 15. Open questions

1. **`fileops` coupling shape.** Event subscription vs. explicit Manager calls for
   `didChange`/`didSave` — interface boundary and ordering guarantees against the
   `Checkpointer` write path.
2. **Pull vs. push preference per server.** When a server advertises both, do we
   prefer versioned push (lower latency, event-driven) or pull (synchronous
   freshness), or pick per request based on whether a fresh version is pending?
3. **Auto-install.** Worth adding OpenCode-style server bootstrapping later, or
   leave servers a user responsibility?
4. **Default `AllowedCommands` for gopls.** Ship an empty default (no command runs
   until the user opts in), or a vetted short list of safe, edit-only commands?
5. **Scoped out-of-band check.** If a concrete staleness bug for non-open watched
   files appears (§11.5), what is the right debounce/scope for the bounded,
   glob-scoped check — and which signal (shell-tool completion?) triggers it?

---

## 16. Summary

This design adds LSP to gogent to **maximize value to the agent**, not to cover
the spec. We build on `go.lsp.dev/protocol` + `go.lsp.dev/jsonrpc2` rather than
hand-rolling framing and types (§6) — a deliberate, justified relaxation of the
std-lib-only rule for `internal/lsp` — while designing around the library's gaps
(utf-16-only wire encoding, §11.3). The client is a **single generic
implementation with no per-language Go code**: per-language knowledge is a
declarative `ServerConfig`, routing is by extension, clients are keyed by server
name so one process can serve several languageIds, roots come from configured
markers, and **every operation is capability-gated** so "unsupported" is a clean
result — with negotiation advertising the result-shaping sub-options our ops need
and the edge layer normalizing the union response shapes servers return (§7, §10).
The model sees curated, permission-gated tools in three value tiers —
**diagnostics as the feedback loop** (push+pull, debounced, deduped, with a
freshness wait keyed on the last version sent via `didOpen` or `didChange`), then
navigation/understanding, then preview-then-apply mutations (resolving lazy code
actions via `codeAction/resolve`) through gogent's existing write/edit + checkpoint
machinery, with `executeCommand` quarantined behind its own allow-listed
`ActionLSPCommand` because its side effects are not checkpointable (§3, §12).
Concurrency is explicit: one read loop owns inbound traffic, shared tables are
locked, freshness waiters register before the version is sent, spawn is
single-flight, and context cancel emits `$/cancelRequest` (§9). The server view is
kept honest against out-of-band edits via re-sync-on-access for open documents and
`workspace/didChangeWatchedFiles` emitted from gogent's own `fileops` writes — no
standing workspace poll (§11.1, §11.5). Server→client callbacks are handled to the
minimum that keeps real servers happy, with sane headless defaults (§10). The batch
`diagnostics`/`verify` tools are removed in favor of the shell (§4). `gopls` is the
zero-config proof of concept; new servers — `pyright`, `rust-analyzer`, … — are
**a config entry, not code.**
