# Design — Unify model management into a single "Models…" dialog (issue #509, Solution A)

## Summary

Replace the two separate `Config` entry points (`&Models…` → `showModelEditor`,
conditional `&Add Model from Catalog…` → `showAddModelDialog`) with **one**
unified `Config → Models…` dialog that is the single home for **add / edit /
remove / set-default** of model config entries. It must open and work with
**zero** configured models and **offline**, in **both** the embedded and remote
(daemon HTTP) backends.

We implement **Solution A (gogent-only)**: compose the dialog from EXISTING
turbotui primitives (`tv.Tree`, `tv.Select`, `tv.TextBox`, `tv.Button`,
`tv.ShowConfirmYesNo`/`showConfirm`). **No turbotui change, no new widget, no
`go.mod` bump, no new deps.** turbotui already ships everything we need — the
codebase has multiple `tv.Tree`-in-dialog precedents
(`sessions_dialog.go`, `resources_dialog.go`, `commands_dialog.go`).

The one capability missing end-to-end today is **remove**, so the bulk of the
non-UI work is threading a new `RemoveModel` through core → handler → server →
api-client → both wirings, mirroring the existing `AddModel`/`UpdateModel` seams.

---

## Files / functions to touch

### turbotui — NONE
A read-only clone lives at `$HOME/work/turbotui`. We consume `tv.Tree`
(`turbotv/widget_tree.go`: `NewTree`, `NewTreeNode`, `Root`, `Selected`,
`OnActivate`, `OnSelect`), `tv.Select`, `tv.TextBox`, `tv.NewButton`,
`tv.ButtonLabelWidth`, and the existing confirm helper. All already exist and are
already used by gogent. The repo seam is respected: gogent depends on turbotui,
not vice-versa, and nothing here pushes gogent-specific concepts down into the
widget library.

### gogent — core (`internal/gogent/gogent.go`)
1. **`func (g *Gogent) RemoveModel(name string) error`** — new, placed next to
   `AddModel`/`UpdateModel`/`SetDefaultModel` (~L3307–3406). Deletes the matching
   `*config.ModelConfig` from `g.config.ModelConfigs`, enforces the edge-case
   policy below, fixes up `g.config.DefaultModel`, then `SaveConfig()`. Mirrors
   the existing mutators' `g.mu.Lock()` → mutate → `Unlock()` → `SaveConfig()`
   discipline (sessions rebuild their connection per send, so no live-registry
   surgery is needed beyond config — same as `AddModel`).
2. **Sentinel errors** (new, package-level in `internal/gogent`):
   `ErrModelNotFound`, `ErrModelInUse`, `ErrModelIsDefault`. These let the
   server map outcomes to precise HTTP status codes via `errors.Is` without
   string-matching. (`AddModel`/`UpdateModel` currently return ad-hoc
   `fmt.Errorf`; we leave those as-is to avoid scope creep, and only add
   sentinels for the new path. `RemoveModel` wraps them with `%w`.)

### gogent — handler struct (`ui/tui/tui.go`)
3. Add **`RemoveModel func(name string) error`** to the `Handlers` struct
   (next to `AddModel`/`UpdateModel`, ~L114–135). Optional like the others; the
   dialog gates the Remove button on it being non-nil.

### gogent — server (`internal/server/`)
4. **`api.go` ~L206–211**: add route
   `{Path: "/models/:name", Method: http.MethodDelete, Handler: mods.Delete, AuthLevel: req}`.
5. **`resources.go`**: add `func (svc modelsSvc) Delete(r *http.Request, name string) (interface{}, error)`
   next to `Update`/`Scan`. Calls `svc.s.g.RemoveModel(name)` and maps the
   sentinel errors:
   - `ErrModelNotFound` → `404 Not Found`
   - `ErrModelInUse` / `ErrModelIsDefault` → `409 Conflict` (message passed
     through so the TUI shows the reason)
   - other → `500`.
   Requires `requireHuman` like the sibling handlers. The policy itself lives in
   core, so the server is a thin status-mapper and embedded/remote behave
   identically.

### gogent — api client (`ui/tui/api_client.go`)
6. **`func (c *APIClient) RemoveModel(name string) error`** next to
   `AddModel`/`UpdateModel` (~L611–632): `c.do(http.MethodDelete,
   "/models/"+url.PathEscape(name), nil, nil)`. `APIClient.do` already turns a
   non-2xx (404/409) into a Go error carrying the body, which the dialog surfaces.

### gogent — wiring (both backends)
7. **`cmd/embedded_handlers.go` ~L166–186**: add
   `RemoveModel: func(name string) error { return g.RemoveModel(name) }`.
8. **`ui/tui/remote_handlers.go` ~L803+ (models block)**: add
   `RemoveModel: func(name string) error { return c.RemoveModel(name) }`.

### gogent — UI (new `ui/tui/model_dialog.go`; edits to `model_editor.go`)
9. **New `showModelsDialog()`** — the unified list dialog (details below).
10. **Extract `modelForm(...)`** from `showModelEditor` so Edit and Empty-slot
    Add share one field-set builder (no duplicated field list). `model_editor.go`'s
    `showModelEditor` is **removed** and its reusable parts (`field` builder,
    `load`/`store`, `scanModels`, the thinking/api-type selects) move into the
    shared form. The pure helpers (`thinkingIndex`/`thinkingValue`/`indexOrZero`/
    `longestRuneLen`, layout consts) stay and are reused.
11. **`ui/tui/tui.go` ~L1134–1141 (menu)**: delete the conditional
    `&Add Model from Catalog…` item; repoint `&Models…` to `showModelsDialog`.
    Drop the `catalogReady()` gate on the menu — the dialog must always open
    (Empty-slot add works offline).
12. **`ui/tui/command_palette.go` ~L249–251**: remove the
    `"Add model from catalog"` palette entry; repoint `"Models"` →
    `showModelsDialog`.

The catalog wizard (`model_catalog_dialog.go`) is **reused unchanged** as one of
the two Add paths — `showCatalogProviderStep` etc. are called from the new
dialog. `catalogReady`, `refreshModelsAfterSave`, `takenModelNames` are reused
as-is.

---

## Edge-case policy (decided, enforced in CORE so both backends match)

`RemoveModel(name)` resolves checks in this order:

1. **Unknown name** → `ErrModelNotFound` (server 404).
2. **In use by a live session** → `ErrModelInUse` ("model is in use by session
   N"). Enforced by scanning `g.userSessions` for any `s.PrimaryModel() == name`
   (live sessions are in-process in BOTH modes — the daemon holds them in remote
   mode, the embedded process holds them in embedded mode — so the single core
   check covers both). Report the first matching session id.
3. **Removing the default while other models remain** → `ErrModelIsDefault`
   ("\"X\" is the default model; set another default first"). BLOCK (we prefer an
   explicit block over silent reassignment so the user's default is never changed
   behind their back).
4. **Removing the last/only model** → ALLOW even if it is the default: delete it
   and clear `g.config.DefaultModel = ""`, yielding the empty-list state. (This
   is the one case where a default removal is allowed, because there is nothing
   left to "set another default" to — it resolves the apparent conflict between
   "block default" and "allow last".)

On success: delete the entry, `SaveConfig()`, return nil. A failed `SaveConfig`
is logged via `g.warnf` (same as the other mutators) but does not block the
in-memory delete — consistent with `AddModel`.

This policy is documented in a comment on `RemoveModel` and the server `Delete`
handler refers to it.

---

## User-facing behavior — the unified dialog

`showModelsDialog()` builds a modal `tv.Dialog` titled **"Models"**:

- **List (left/top): `tv.Tree`**, one node per configured model. Row label:
  `<default-marker> <DisplayName> — <model id> — <api type>`, e.g.
  `✓ GPT-4o — gpt-4o — openai`. The default marker (`✓`) comes from
  `GetDefaultModel()`. `tree.OnActivate` (Enter) = Edit the selected row;
  `tree.OnSelect` keeps the selection in sync so the toolbar buttons act on the
  highlighted row. List colours follow the `selectionColorsFor` pattern already
  used by `sessions_dialog.go` (issue #327).
- **Zero models**: NO early return (the current `showModelEditor` bug — it shows
  "No models are configured." and bails). Instead show an empty-list placeholder
  row ("No models configured — choose Add to create one."). Only **Add…** and
  **Done** are enabled; **Edit / Remove / Set Default** are disabled
  (rendered but non-actionable, guarded in their handlers and visually de-emph'd
  if cheap; at minimum the handlers no-op with no selection).
- **Toolbar / buttons** (bottom row, mirroring `sessions_dialog.go`'s 2-row
  footer with a hint line):
  - **Add from Catalog…** — only present/enabled when `catalogReady()`; runs the
    EXISTING wizard (`showCatalogProviderStep` → model → pre-filled review →
    `AddModel`). On the existing wizard's completion it already calls
    `refreshModelsAfterSave()`. After the wizard closes we re-open / refresh the
    list. If catalog is unavailable this button is hidden; Empty-slot still works.
  - **Add Empty…** — always present. Opens a BLANK `modelForm` (same field set as
    Edit, plus an **editable Name**) and on Save calls `AddModel(draft)`. Works
    with zero models and offline (no catalog touched). Name uniqueness reuses the
    `takenModelNames()` + suffix logic already in the catalog review step.
  - **Edit…** — opens the same `modelForm` pre-filled for the selected model;
    Save calls `UpdateModel`. Name is **read-only** in Edit (it is the stable key
    `UpdateModel` matches on; a rename would orphan the old entry — out of scope,
    achieved via remove+add).
  - **Remove** — `showConfirm`/`ShowConfirmYesNo`
    `Remove "<display>"? This cannot be undone.` → on Yes call `RemoveModel`.
    A blocked removal (default/in-use) returns an error which we surface in a
    `showConfirm` with the server/core message (e.g. "set another default
    first"). On success refresh the list + dropdowns.
  - **Set Default** — calls existing `SetDefaultModel(selectedName)`, updates the
    default marker, refreshes dropdowns. Disabled with no selection / when the
    handler is unwired.
  - **Done** — closes the dialog (Esc also closes).
- **Decision — two Add buttons, not a popup menu.** Two plain buttons are
  simpler than introducing a popup-menu affordance, make both paths discoverable
  at a glance, and let us hide the Catalog button cleanly when the catalog is
  unavailable. Documented here per the issue's request.

After any add / edit / remove / set-default, we **persist + refresh live
dropdowns** via the existing `refreshModelsAfterSave()` (`SetModels` +
`rebuildMenu`) — the same path `showModelEditor.save` and the catalog wizard
already use, so open session windows and the sidebar pick up changes immediately
(issue #389). The list dialog itself re-reads `GetModels()` to rebuild its tree
after each mutation.

### `modelForm` (shared builder)
Signature roughly: `modelForm(title string, initial config.ModelConfig,
nameEditable bool, onSave func(config.ModelConfig) error)`. It builds the field
set currently in `showModelEditor` (Name [editable only in Add], Display name,
API type, Endpoint, Model id [+ Scan], API key, Temperature, Max tokens,
Reasoning, Thinking, Project, Location), wires the same `load`/`store`
conversions, and calls `onSave` with the assembled `config.ModelConfig`. Edit
passes `UpdateModel`; Empty-add passes `AddModel`.

**Scan in Add mode**: omitted (button hidden when the form has no saved name),
matching the catalog review step's deliberate choice — a draft scan can't work in
remote mode (`ScanModels` is keyed by a SAVED model name and would 404). Scan
stays available in Edit mode (model is saved). Documented in a comment.

---

## The four design gates

**(1) Goal match.** Exactly the issue's ask, Solution A: a single `Models…`
dialog does add (Catalog + Empty-slot) / edit / remove / set-default; the
standalone `Add Model from Catalog…` menu + palette entries are deleted; the
dialog opens with zero models (no early-return) and Empty-slot add works offline;
it works embedded AND remote because the new `RemoveModel` is wired through both
`cmd/embedded_handlers.go` and `remote_handlers.go` and the DELETE route. No
scope creep: config schema unchanged, `AddModel`/`UpdateModel`/`Scan` untouched,
catalog wizard reused verbatim.

**(2) Usability.** List shows default marker + display + model id + api type;
Edit/Remove/Set Default disabled in empty/last states; Remove confirms; removing
the default is BLOCKED with a clear hint rather than silently reassigning;
in-use removal blocked with the session id; all changes refresh live dropdowns.
The user drives every input (Add via two clear buttons, Enter-to-edit on a row).
Nothing happens silently — blocks and successes both surface a dialog.

**(3) No regressions.** Edit / catalog-add / set-default / scan keep working
because the underlying handlers and the catalog wizard are reused, not rewritten;
`modelForm` is the same fields the old editor had. `RemoveModel` follows the
established lock/SaveConfig pattern, so persistence and session invariants hold
(sessions rebuild connections per send; an in-use model can't be pulled out from
under a live session because of the in-use block). Existing model-route server
tests stay green (we ADD a DELETE route, don't alter GET/POST/PUT). The
deprecated `showModelEditor` symbol is removed and all call sites repointed, so
no dangling references. gofmt/build/vet/golangci-lint clean; `go test ./...`
green except the pre-existing environmental `TestUserSessionSendMessage` 404.

**(4) Holistic across both repos.** turbotui is untouched — the dialog is pure
composition of existing primitives, honouring the library/app seam (the issue's
Solution B would have added a `Table` widget to turbotui; we explicitly chose A
to keep the change one-repo). Within gogent the change lands in the right layers:
UI in `ui/tui`, the mutation in `internal/gogent`, transport in
`internal/server` + `api_client` + the two wirings, with policy centralized in
core so embedded and remote can't drift. Downstream effect on turbotui:
none. Downstream effect within gogent: live dropdowns/menu refresh is already the
established post-mutation contract and is reused.

---

## Regression risks & mitigations
- **Default/last-model conflict** (block-default vs allow-last) — resolved
  explicitly in policy step 4 (allow + clear default only when it's the only
  model). Covered by a core test.
- **In-use detection differs embedded vs remote** — avoided by enforcing in core
  (`g.userSessions`), which both modes share; the server only maps status.
- **Removing `showModelEditor` breaks a caller** — `offerManualEditor` in
  `model_catalog_dialog.go` calls `w.showModelEditor()` as its offline fallback;
  it will be repointed to `showModelsDialog` (which itself offers Empty-slot add
  offline), so the graceful-degradation path still lands somewhere useful.
- **`tv.Tree` empty-state focus** — guard handlers against `tree.Selected()==nil`
  so the empty list can't panic on a button press.
- **Tests importing `internal/*` from `ui/tui`** — keep the UI test using the
  `Handlers` struct with stub funcs (no `internal/daemon|server` imports), per
  the issue's constraint and the existing catalog/daemon-dialog test style.

## Tests to add (implementation phase, not now)
- core `RemoveModel`: removes a non-default entry; blocks default (≥2 models);
  blocks in-use; allows last (clears default → empty); `SaveConfig` persists.
- server `DELETE /models/:name`: success; 409 default/in-use; 404 unknown;
  existing model routes still pass.
- api_client + remote_handlers: `RemoveModel` issues DELETE (mirror AddModel
  test).
- UI flow: open with zero models (only Add/Done enabled); Empty-slot add offline;
  catalog add (stubbed catalog); edit; remove with confirm; remove-default
  blocked with message.

---

## Open questions
1. **Rename in Edit** — we make Name read-only in Edit (rename = remove+add).
   Acceptable, or should Edit support rename (would need a core
   rename/`UpdateModel`-by-old-name path)? Recommendation: keep read-only;
   out of scope for #509.
2. **In-use scope** — block only on a model that is the *primary* model of a live
   session (`PrimaryModel()`). Should we also block if a session merely *can*
   switch to it? Recommendation: primary-only; switching picks a different model
   at send time and a removed model just falls back to default.
3. **List layout** — single-column tree of formatted rows (chosen) vs a
   tree+detail split like `sessions_dialog.go`. Recommendation: single column —
   the row already carries default/id/type; Edit shows full detail. Easy to add a
   detail pane later if desired.
4. **Post-add default prompt** — the catalog review step already asks "set as
   default?" after add. Should Empty-slot add ask the same? Recommendation: yes,
   for consistency (reuse the same `showConfirm` follow-up).
