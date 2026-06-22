# Development

gogent is a Go module (module path `gogent`, Go 1.25.11). This guide covers building, testing, the CI pipeline, and how to extend the project — adding tools, providers, skills, and documentation. For the full annotated package map, see [architecture.md](architecture.md).

## Building & testing

Build the whole module:

```sh
go build ./...
```

Or produce a named binary from the entrypoint:

```sh
go build -o gogent ./cmd
```

Vet the tree:

```sh
go vet ./...
```

Run the test suite with the race detector and no result caching:

```sh
go test ./... -race -count=1
```

The race detector is architecture-independent, but CI runs it on x86 because the production target (aarch64) lacks a race detector. Prefer running the `go` commands directly over the `test.sh` script at the repo root — `test.sh` is a convenience wrapper that builds, tests, builds the binary, and runs it, but it contains a hardcoded path.

## CI pipeline

CI is defined in `.github/workflows/ci.yml` and runs on push to `main` and on pull requests. It has three jobs:

- **test** — "build & test (-race)": `go build ./...` → `go vet ./...` → `go test ./... -race -count=1`. The Go version is pinned via `go-version-file: go.mod`.
- **vulncheck** — "govulncheck": `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.
- **lint** — "golangci-lint": `golangci/golangci-lint-action@v8` with a v2 config. This job is advisory — `continue-on-error: true` — because the linter set is opinionated (gosec, wrapcheck) and is not yet blocking merges.

> Note on the lint action version: the repo's `.golangci.yml` is a v2 config, so action v8 (which installs golangci-lint v2) is required. The v6 action installs golangci-lint v1.64.x built with go1.24, which refuses to target go1.25.

## Module layout

See [architecture.md](architecture.md) for the full annotated package map. In brief:

- `cmd/` — the entrypoint.
- `internal/` — all private packages: the gogent singleton, agent runtime, model connector, tools, permission, config, server, and more.
- `ui/tui` — the terminal UI.
- `skills/` — built-in `SKILL.md` skills.
- `docs/` — this documentation.

## Cross-repo dependencies

gogent depends on two first-party repositories we own:

- **github.com/hobbestherat/turbotui** (imported as `turbotv`) — the TUI toolkit. Window geometry, widgets, and dialogs live here.
- **github.com/hobbestherat/webapi** — the HTTP API framework (reflection-bound handlers, SSE, auth). It backs `internal/server`.

Plus third-party libraries:

- **github.com/alecthoms/chroma/v2** and **github.com/yuin/goldmark** — markdown rendering and syntax highlighting.

When a feature spans both gogent and turbotui (for example, window tiling or dialog sizing), split it: put pure geometry/algorithm code in turbotui where it can be tested in isolation, and keep session-aware orchestration in gogent. See `.claude/design-tiling.md` for a worked example.

## How to add a tool

1. Implement the tool interface (`Name`, `Description`, `Execute(args) (interface{}, error)`) in the appropriate package — `internal/fileops` for file tools, `internal/tool` for system tools, and so on.
2. Register it in `internal/gogent/gogent.go`'s tool registry. Set `ReadOnly: true` for read-only tools to enable the concurrent fast-path.
3. Gate side-effecting tools through `internal/permission` by choosing or creating an `Action` constant. See [tools-and-permissions.md](tools-and-permissions.md) for the model.
4. File tools route through `fileops.CheckFileAccess` for workspace-boundary enforcement.
5. Add a test. The `ToolRegistry.ExecuteToolCall` wrapper handles validation, stats, panic containment, and audit logging automatically — you only need to test the tool's own `Execute` logic.

## How to add a provider

1. Add an `APIType` constant in `internal/model/provider.go`.
2. Add a `providerSpec` entry: `defaultBaseURL`, `chatPath`, `modelsPath`, `authMode`, and capability flags.
3. Only if the provider speaks a genuinely different protocol, add a new adapter in `adapterFor()` (`internal/model/adapter.go`). OpenAI-compatible providers reuse `openAIAdapter`.
4. Wire the `StringToAPIType` mapping.
5. See [providers.md](providers.md) for the provider model.

## How to add a skill

1. Create a folder under `~/.gogent/skills` or `./skills` containing a `SKILL.md`.
2. `SKILL.md` begins with YAML frontmatter containing `name:` and `description:` (both required).
3. The body is the full instruction markdown, loaded on demand via the skill tool (progressive disclosure).
4. Trust boundary: symlinks are not followed; files must be inside the resolved root; directory depth is bounded at 16; file size is capped at 1 MiB.
5. Toggle a skill in the TUI under **Config → Resources…**, or via the API (`PUT /api/skills/:name/active`).

## Adding documentation

All user docs live in `docs/` (this folder). The `README.md` is a landing page with a link index pointing into these docs. Keep documentation code-grounded — verify field names, endpoints, and keybindings against the source before writing. `config.sample.json` at the repo root is the canonical config example; see [configuration.md](configuration.md) for field reference, and [api.md](api.md) for the HTTP API surface.
