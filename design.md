# Design — GOGENT half of issue #549 (TUI SSH terminfo colour detection)

Closes #549.

## Problem

Colour fidelity is detected **twice and independently**, and gogent's copy is
**env-only**:

- **turbotui** (`color.go`) owns a colour level: `init()` seeds it from
  `ColorLevelFromEnv(os.LookupEnv)` and the renderer downsamples every `Color`
  through `adaptColor(c, GetColorLevel())` before emitting SGR codes.
- **gogent** (`ui/tui/theme.go:719 detectColorLevel`) re-implements the same
  env rules (NO_COLOR / TERM / COLORTERM) in its own `switch`, and
  `ResolveTheme` (theme.go:661) degrades the palette to *that* gogent level.

Over an SSH hop `sshd` sets a valid `TERM` on the remote but rarely forwards
`COLORTERM`, so env-only detection can only see `TERM` and reports **256** (or
16) for a terminal that is really **truecolor**. turbotui's merged half
(`4151a29`) fixes this by consulting **terminfo** (`colors` / `Tc` / `RGB`,
keyed by the propagated `TERM`) via `ColorLevelFromEnvWithTerminfo` /
`DetectColorLevel`, which survives the hop. gogent must stop running its own
env-only detector and instead **resolve the theme at turbotui's detected
level**, otherwise gogent pre-degrades the palette to 256 ANSI indices and the
renderer (even when set to truecolor) can only pass those degraded indices
through — wasting the capability terminfo just recovered.

## turbotui seam (already merged @ `4151a29`, read-only)

`github.com/hobbestherat/turbotui`:

- `type ColorLevel uint8`: `ColorLevelNone | ColorLevel16 | ColorLevel256 | ColorLevelTrueColor`.
- `SetColorLevel(level)` / `GetColorLevel()` — the single active level (atomic global).
- `DetectColorLevel()` = `ColorLevelFromEnvWithTerminfo(os.LookupEnv, InfocmpCaps)` — the
  **production** detector; consults terminfo (shells `infocmp`, 2s timeout, fails
  open). `init()` deliberately uses env-only `ColorLevelFromEnv` so merely
  importing never spawns `infocmp`; a host opts into terminfo by calling
  `SetColorLevel(DetectColorLevel())` once at startup.
- `ColorLevelFromEnv(lookup func(string)(string,bool))` — env-only, no subprocess.

**No turbotui change** is made by this half (its side is merged). gogent only
bumps the dependency and consumes the new API.

## Design

turbotui becomes the **single source of truth** for the colour level. gogent
keeps its own `ColorLevel` enum and `degrade()` machinery (used pervasively by
the palette, audit and ~15 theme test files) but stops *detecting* — it maps
turbotui's level onto its enum and degrades at that level. Both layers then
always agree because gogent installs the level it resolves at.

### Files / functions touched (gogent only)

**`ui/tui/theme.go`**

1. **`detectColorLevel` (719)** — delete the env `switch`; defer to turbotui:
   ```go
   // detectColorLevel reports, in gogent's ColorLevel enum, the level turbotui
   // has detected. turbotui owns env+terminfo detection (it survives an SSH hop);
   // gogent only maps the result. Install the level once at startup with
   // tui.SetColorLevel(tui.DetectColorLevel()) before resolving a theme.
   func detectColorLevel() ColorLevel { return fromTUILevel(tui.GetColorLevel()) }
   ```
   Signature drops its `env` argument (its only non-test caller is `ResolveTheme`).

2. **`fromTUILevel` (new, small)** — total 1:1 map
   `ColorLevelNone→ColorNone, 16→Color16, 256→Color256, TrueColor→ColorTrue`.

3. **`ResolveTheme` (661)** — keep the **signature**
   `(cfg config.ThemeConfig, env func(string) string, noColorFlag bool)` to avoid
   churning ~100 call sites. Resolve at the level turbotui has detected, and — this
   is the corrected body (see Risk **R1**) — **only write the global on the two
   paths that must affect the renderer** (no-color, production); the explicit/test
   `env != nil` path computes its level **locally and purely**, exactly as today:
   ```go
   func ResolveTheme(cfg config.ThemeConfig, env func(string) string, noColorFlag bool) Theme {
       var level ColorLevel
       switch {
       case noColorFlag || cfg.NoColor:
           tui.SetColorLevel(tui.ColorLevelNone) // force none at BOTH layers (#2)
           level = ColorNone
       case env == nil:                          // production: resolve at the level the
           level = detectColorLevel()            // entry point installed (terminfo-aware)
       default:                                  // explicit/test env: PURE, no global write
           level = fromTUILevel(tui.ColorLevelFromEnv(lookup(env)))
       }
       ... // the 50 degrade(...) lines unchanged
   }
   ```
   - `env` gains a **nil sentinel**: *"resolve at the already-installed
     (terminfo-aware) level, don't recompute."* Production passes `nil`; tests
     pass their `envOf(...)` map as before.
   - `lookup` adapts gogent's `func(string)string` to turbotui's
     `func(string)(string,bool)` via `v := env(k); return v, v != ""` (so empty
     `NO_COLOR` is correctly treated as absent, matching the old rule).
   - **Why the `env != nil` path must not call `SetColorLevel`**: today
     `ResolveTheme` is side-effect-free (`level := detectColorLevel(env)`), so the
     ~168 test call sites are hermetic. Writing the global there would leak the
     last test's level across the sequential `ui` package run (the shared restore
     helper `issue204RestoreTheme` snapshots only `tv.ActiveTheme()`, **not**
     `tui.GetColorLevel()`), making render tests order-dependent. Computing
     locally reproduces today's behaviour exactly. The global is written **only**
     on the no-color path (required by #2) and the production `env == nil` path
     (where the entry point already owns the level) — both deliberate.
   - The old `if noColorFlag || cfg.NoColor { level = ColorNone }` block and the
     old `detectColorLevel(env)` call are replaced by the switch above.

**`cmd/main.go:199`, `cmd/attach.go:149`, `cmd/attach.go:302`,
`cmd/embedded_handlers.go:145`** — install detection once per entry point and
resolve at the installed level:
```go
tui.SetColorLevel(tui.DetectColorLevel())                 // terminfo-aware, survives SSH
tuipkg.ApplyTheme(tuipkg.ResolveTheme(cfg, nil, noColorFlag))  // nil → use installed level
```
- `main.go:199` — embedded startup.
- `attach.go:149` — attach startup (Workflow B: local `--connect ssh://host`
  resolves on the **local** terminal's `DetectColorLevel()` — correct).
- `attach.go:302` & `embedded_handlers.go:145` — the live `SetTheme` handlers:
  re-detect on each switch so an env change reflects consistently.
- `os.Getenv` arg is dropped at all four; remove the now-unused `os` import per
  file only if nothing else uses it (verify at impl time).

**`go.mod` / `go.sum`** — `go get github.com/hobbestherat/turbotui@4151a29...`
then `go mod tidy`. New pin: `v0.3.1-0.20260628112027-4151a29defc4` (exact value
confirmed by `go get`). No `replace`.

### Tests

- **`ui/tui/keybindings_issue401_test.go:175`** (version-pin guard) — update the
  pinned string to the new pseudo-version; the no-`replace` assertion stays.
- **`TestDetectColorLevel`** (theme_test.go) — rewrite: snapshot the global with
  `old := tui.GetColorLevel()` + `t.Cleanup(func(){ tui.SetColorLevel(old) })`
  (mirroring `withIssue366ColorLevel`); for each case
  `tui.SetColorLevel(tui.ColorLevelFromEnv(lookup(envOf(c.env))))` then assert
  `detectColorLevel() == c.want`. Note: the `{}`-empty-`TERM` case now yields
  `Color16` (turbotui's baseline), not `ColorNone` — a deliberate alignment to the
  shared detector (see Risk **R3**).
- **New layer-agreement test** — the property is *production* (`env == nil`): for
  each of none/16/256/truecolor, `tui.SetColorLevel(L)` then
  `th := ResolveTheme(cfg, nil, false)` and assert
  `fromTUILevel(tui.GetColorLevel()) == th.Level` (the two layers never disagree).
  Wrap with `old := tui.GetColorLevel()` + `t.Cleanup` restore.
- **`TestResolveThemeNoColor`** — still passes; the `flag`/`cfg` subtests hit the
  `SetColorLevel(None)` branch (a deliberate global write per #2) and the `env`
  subtest (`NO_COLOR=1`) computes `None` locally. Add `old := tui.GetColorLevel()`
  + `t.Cleanup` restore at the top so the three `None` writes don't leak (Risk
  **R2**).
- **All other ~15 ResolveTheme test files** — **genuinely unchanged**: they pass a
  non-nil `envOf(...)`, which now hits the **pure** `default` branch (no global
  write), so `ResolveTheme` is side-effect-free for them exactly as today and the
  returned/degraded values match bit-for-bit (their envs are
  truecolor/256/16/no-color — none use empty `TERM`).

## Criterion (1) GOAL MATCH

Exactly the issue's ask: a **fix**, not a feature/refactor. gogent stops
double-detecting and defers to turbotui's terminfo-aware level (#1), honours
`NO_COLOR`/`--no-color` at both layers (#2), installs detection once at every
production entry point (#3), and deletes its duplicate env rules (#4). No new
dialog, flag or scope beyond the four numbered points. gogent's `ColorLevel`/
`degrade` enum is retained (deleting it is out-of-scope churn).

## Criterion (2) USABILITY

No new interaction — colour is automatic. User-visible behaviour:
- An SSH'd truecolor terminal now renders truecolor instead of a degraded
  256-colour approximation (the fix). 256-only remotes still (correctly) get 256
  — detection stays faithful to observable signals.
- `--no-color` / `NO_COLOR` / `cfg.NoColor` blank colour at **both** layers, so
  no half-coloured chrome leaks through the renderer.
- Live theme switches (`SetTheme` handlers) re-detect, so the palette tracks the
  current terminal/env. Nothing is silent: detection drives what the user sees.

## Criterion (3) NO REGRESSIONS

- **Signature kept** → ~168 `ResolveTheme(cfg, env, flag)` call sites compile and
  pass unchanged; only `detectColorLevel`'s signature changes (1 prod caller, 1 test).
- **R1 (resolved) — no new global side effect on the test path.** `ResolveTheme`
  is side-effect-free today (`level := detectColorLevel(env)`), which is *why* the
  168 test call sites are hermetic in a package with **no** `t.Parallel()` (so
  ordering matters). The corrected body keeps the `env != nil` path **pure**
  (computes `fromTUILevel(tui.ColorLevelFromEnv(lookup(env)))`, no
  `SetColorLevel`), so those calls reproduce today's behaviour exactly — the
  ~15 files are genuinely unchanged. The shared restore helper
  `issue204RestoreTheme` snapshots only `tv.ActiveTheme()`, **not**
  `tui.GetColorLevel()`; the only sanctioned global writes are the no-color path
  (#2) and the production `env == nil` path, both covered by `t.Cleanup` in the
  tests that exercise them.
- **R2 (resolved)** — `TestResolveThemeNoColor`'s `flag`/`cfg` subtests write
  `ColorLevelNone` to the global (required by #2); add a `t.Cleanup` save/restore
  at the top of that test so nothing leaks to later tests.
- **Double-degrade stays idempotent**: gogent degrades to level L, turbotui's
  `adaptColor` at the *same* L is a no-op on already-degraded colours (verified at
  `turbotui/color.go:140-163`). In production the two levels are guaranteed equal
  because the entry point installs the level gogent then resolves at (previously
  they could drift: gogent env-only vs turbotui's `init` env-only).
- **R3 — behaviour change, empty `TERM`**: was `ColorNone`, now `Color16`
  (turbotui treats only `TERM=="dumb"` as none, `color.go:105-108`; deferring is
  what removes the duplicate rule, so this is unavoidable). This makes gogent
  agree with turbotui (which already rendered its own widgets at 16 in that case),
  removing a prior inconsistency. Low impact (a TUI needs a terminal), but a
  piped/non-interactive invocation now emits 16-colour ANSI where it emitted none
  — **call this out in the PR body**. No built-in palette test exercises empty `TERM`.
- Session/transcript invariants untouched (colour-only change); `ui/tui` keeps
  no `internal/daemon`/`server` imports (only `tui` + `config`, already present).

## Criterion (4) HOLISTIC DESIGN across both repos

- **Right place / seam respected**: detection (env + terminfo, the SSH-surviving
  signal) lives **once** in turbotui — the renderer's owner — exposed via
  `Set/GetColorLevel`/`DetectColorLevel`. gogent consumes it and never shells
  `infocmp` itself nor duplicates the rules. Import-time stays subprocess-free
  (turbotui `init` is env-only); the `infocmp` spawn happens only at gogent's
  explicit entry-point `DetectColorLevel()` call.
- **Downstream on turbotui**: none — gogent only reads the new API; turbotui's
  half is already merged and unchanged here.
- **Both-layers-agree** is the structural guarantee **in production**: the entry
  point installs the level via `tui.SetColorLevel(tui.DetectColorLevel())` and
  gogent resolves at that same `tui.GetColorLevel()`, so theme degrade and
  renderer downsample never diverge; the no-color path forces `None` on both. (The
  `env != nil` path is test-only and pure — it never reaches a real renderer, so
  it needs no global write; see R1.)
- **Parallel enum note**: gogent keeps its own `ColorLevel`
  (`ColorNone/16/256/True`) bridged to turbotui's by a hand-written `fromTUILevel`
  — a second representation of one concept. Justified by the pervasive
  `degrade()`/audit usage and explicitly scoped as retained; the cost is that a
  future new turbotui level must be added to `fromTUILevel` (a `default` should
  map to `ColorNone` defensively). Long-term-cleaner direction (gogent adopts
  turbotui's type) is out of scope for this fix.

## PR body

`Closes #549`. Summarise: gogent stops re-implementing colour detection and
defers to turbotui's terminfo-aware level (fixes truecolor-over-SSH), forces
`None` at both layers for `NO_COLOR`/`--no-color`, and installs detection once at
each production entry point. **Behaviour note (R3):** with detection now owned by
turbotui, an unset `TERM` resolves to 16-colour (turbotui's baseline) instead of
gogent's old no-colour — a non-interactive/piped invocation now emits 16-colour
ANSI where it previously emitted none.

## Open questions

1. **Re-detect cost on live `SetTheme`**: the two handler entry points call
   `DetectColorLevel()` (an `infocmp` spawn, ≤2s) on every theme switch. Task #3
   lists all four sites, so the design re-detects; acceptable for a
   user-initiated, infrequent action. Alternative: handlers reuse the installed
   level and only force `None` for no-color. Proceeding with re-detect per the
   task wording unless told otherwise.
2. **`nil`-env sentinel vs dropping the param**: keeping the param + `nil`
   sentinel avoids ~100 call-site edits and conflicts with the parallel #551/#552
   ui/tui work. Dropping `env` entirely would be cleaner long-term but is larger,
   churn-heavy, and out of this issue's scope. Proceeding with the sentinel.
