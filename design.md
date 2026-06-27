# Design — FIX-MODELS-UPDATE-PARAM-ORDER (#537) + ROUND-TRIP-TEST (#538)

Branch: `pair1/fix-models-update-param-order-round-trip` · gogent-only · stdlib-only · no new deps · no go.mod bump · no turbotui change.

---

## 0. Headline finding (read first) — the #537 code fix is ALREADY in origin/main

The one-line param reorder that #537 asks for is **already landed**. I verified it against the live tree, not from memory:

- `git show d36bf56:internal/server/resources.go` (parent of the current tip) → `func (svc modelsSvc) Update(r *http.Request, name string, req updateModelRequest)` — the **buggy** `(r, name, req)` order.
- `git show origin/main:internal/server/resources.go` (`33d3fee`, **#536** "Validate model configs at save & load; fail-safe unroutable defaults") → `func (svc modelsSvc) Update(r *http.Request, req updateModelRequest, name string)` — the **fixed** `(r, req, name)` order.
- `git log -S"req updateModelRequest, name string" -- internal/server/resources.go` → the fixed signature was introduced by `33d3fee` (#536).
- `HEAD == origin/main == 33d3fee` (clean tree).

So #536 incidentally bundled the exact reorder #537 prescribes — the handler's body comment (`resources.go:53–61`) even documents the convention and the silent-body-drop bug. **resources.go needs no change.** The task brief assumed the fix still had to be applied; reality is it shipped with #536. I'm surfacing this rather than re-applying a no-op edit.

### What this means for the PR
The durable, still-missing deliverable is the **#538 regression test**, which does not exist (`internal/server/` has `models_create_test.go` but no `models_update_test.go`). The PR therefore:
- Adds `internal/server/models_update_test.go` (the guard).
- Closes **both** issues: `Closes #537` (fixed by #536; now permanently guarded) and `Closes #538` (the test). The PR body will state plainly that the #537 code fix already landed in #536 and link the commit, so the maintainer isn't misled into expecting a resources.go diff.

### "Fails before the fix / passes after" — how we honor it
Against current origin/main the test **passes** (fix present), so its discriminating power can't be shown by the diff alone. During the BUILD/VERIFY phase we prove the test actually catches the bug by **transiently** reverting the signature locally to `(r, name, req)`, running `go test ./internal/server/ -run TestUpdateModelRoundTripPersistsAllFields -count=1` (expect RED), then restoring `(r, req, name)` (expect GREEN). The revert is a throwaway verification step — never committed. This is recorded in the PR body as evidence.

---

## 1. Root cause (for the record — already remediated)

webapi's reflection binder decodes the JSON request body **only** into the handler's index-1 parameter (`handlerType.In(1)`). With the old `Update(r, name string, req updateModelRequest)`, `In(1)` was the path-param `string`, so the body decode was skipped and a **zero-value** `updateModelRequest` was persisted — every editable field blanked, 200 OK, silent. Only `name` (from the path) and `api_key` (empty-key-preserve borrow) survived. Embedded mode (`g.UpdateModel` in-process) never went through webapi, so it was unaffected. The fix makes the body struct index-1 (`(r, req, name)`), matching `watchersSvc.Update` / `commandsSvc.Update`; webapi then fills the remaining `string` from the path. `updated.Name = name` still makes the path name win.

---

## 2. Files touched

| Repo | File | Change |
|---|---|---|
| gogent | `internal/server/resources.go` | **NONE** — fix already present (#536). |
| gogent | `internal/server/models_update_test.go` | **NEW** — `TestUpdateModelRoundTripPersistsAllFields`. |
| turbotui | — | **NONE.** Seam respected (see §6). |

No `go.mod`, no webapi, no new helpers.

---

## 3. The regression test — `TestUpdateModelRoundTripPersistsAllFields`

Package `server` (white-box: it reads `srv.g` and the temp home directly, like `default_model_issue507_test.go:36` and `background_state_issue353_test.go:32`).

**Harness (reuse only):** `loopbackReq` + `serveOne` from `server_test.go`. The seed/reload need direct handles to `g` and the home dir, which `newTestServer` does not expose, so the server is built with the established 3-line manual pattern rather than via `newTestServer`:

```
home := t.TempDir()
g := gogent.NewGogent(home)
srv := NewServer(g, Options{Password: "x"})
```

This is an existing idiom (not new machinery) and gives us `home` (for `config.LoadConfig`) and `g` (for `g.AddModel`). No model provider is contacted — we never send a message; only config CRUD over the loopback `/api`.

**Steps:**

1. **Seed a maximal model via `g.AddModel(config.ModelConfig{...})`** populating EVERY field so the assertion surface is future-proof:
   `Name:"work"`, `DisplayName:"Work"`, `Model:"gpt-x"`, `Endpoint:"https://api.example.com/v1"`, `APIType:"openai"`, `APIKey:"seed-secret-key"`, `Temperature:0.4`, `TopP:0.9`, `MaxTokens:2048`, `ContextWindow:128000`, `ReasoningEffort:"medium"`, `EffortOptions:[]string{"low","medium","high"}`, `Thinking:boolPtr(true)` (non-nil pointer), `Project:"proj"`, `Location:"us-central1"`, `Free:true`.
   The full endpoint+model+key makes the config routable, so it passes the #532/#536 save-time validation `AddModel` now enforces. `AddModel` persists to disk, so `LoadConfig` will see it.

2. **GET `/api/models`** via `serveOne(t, srv, loopbackReq(GET, "/api/models", nil))`; assert 200; decode `[]modelView`; find the entry with `Name=="work"`. (This is the redacted view the TUI actually receives — note `modelView` has **no** `api_key` field, only `has_api_key`.)

3. **Build the PUT body by re-marshalling that `modelView`**, mutating exactly one field: `view.DisplayName = "Work Renamed"`. `json.Marshal(view)` → body. Because `modelView` carries no `api_key`, the body **omits api_key** — exactly what the TUI does (redacted on GET, never re-sent), exercising the empty-key-preserve path for free. No need to hand-craft JSON.

4. **PUT `/api/models/work`** with that body via `serveOne(... loopbackReq(PUT, "/api/models/work", bytes.NewReader(body)))`; assert status **200**.

5. **`cfg, err := config.LoadConfig(home)`**; find the `ModelConfig` named `"work"` in `cfg.Models`; assert the full round-trip from disk:
   - Edited field changed: `DisplayName == "Work Renamed"`.
   - Every other non-redacted field **unchanged from seed**: `Model`, `Endpoint`, `APIType`, `Temperature`, `TopP`, `MaxTokens`, `ContextWindow`, `ReasoningEffort`, `EffortOptions` (slice-equal), `Thinking` (non-nil && `*Thinking==true`), `Project`, `Location`, `Free`.
   - **`APIKey == "seed-secret-key"`** — preserved despite being absent from the PUT body (the empty-key-preserve rule).
   - Pre-fix, every one of these (except `Name`, and `APIKey` via the borrow) would be the zero value → the test fails loudly on the first field. Post-fix all match.

6. **Decode the PUT response body** into `modelView`; assert it's a non-empty view reflecting the update: `resp.Name=="work"`, `resp.DisplayName=="Work Renamed"`, `resp.Model=="gpt-x"`, `resp.Endpoint != ""`. Pre-fix this is the zero-value husk #537 produced (`Model==""`, `Endpoint==""`); post-fix it's the real config. This gives a second, response-level discriminator independent of disk reload.

**Discriminating fields (why it fails pre-fix):** `Model`, `Endpoint`, `Temperature`, `MaxTokens`, `ContextWindow`, `ReasoningEffort`, etc. are all non-empty in the seed and all zero in the husk. The assertion fails on the very first, so the RED is unambiguous when the signature is reverted.

A tiny `func boolPtr(b bool) *bool { return &b }` local to the test file is the only helper; it's trivial, not "harness machinery."

---

## 4. The four design gates

**(1) Goal match.** #537's ask — body struct as the 2nd param so remote-mode edit persists ALL fields, only the edited field changes, api_key preserved — is satisfied by the code already in main; the PR adds the #538 test that *asserts* exactly that contract and fails on the old ordering. No scope creep: resources.go is untouched; webapi is untouched (its `body→In(1)` contract is correct, the optional upstream hardening is explicitly out of scope); embedded mode untouched. The one deviation from the brief — not editing resources.go — is forced by the fix already existing, and is surfaced, not silently absorbed.

**(2) Usability.** Remote/attached Models… → Edit → change a field → Save now persists the whole config (this is the behavior #536 already restored). The test locks in the user-visible promise: the dialog round-trip never again blanks a config, and the empty-key-preserve means the user never has to re-enter an API key they can't even see (it's redacted). The 200 + non-husk response also means the TUI's optimistic redraw shows the real config, not blanks.

**(3) No regressions.** Test-only addition; no production code changes → zero behavioral risk to existing paths. `TestDeleteModelLeavesOtherModelRoutesIntact` (`remove_model_test.go`, PUTs `/api/models/routecheck`, asserts 200) keeps passing — it exercises the same now-correct route. The new test builds its own isolated `t.TempDir()` core, so it can't perturb sibling tests. Embedded mode and session/transcript invariants are not touched. Gate: `gofmt`/`build`/`vet` clean, `golangci-lint` 0 new issues, `go test ./internal/server/ -count=1` green (and the known-acceptable `TestUserSessionSendMessage` 404 elsewhere is unrelated).

**(4) Holistic, both repos.** The change lives exactly where the contract is enforced — an `internal/server` handler test next to `models_create_test.go`. The gogent↔turbotui seam is respected: turbotui's Models dialog calls `APIClient.UpdateModel`, which GETs the redacted `modelView` and PUTs it back **without api_key**; the test reproduces that client behavior precisely (re-marshal the redacted view, omit the key), so it guards the real cross-repo flow without importing or changing turbotui. No turbotui edit is warranted — the bug and its fix are entirely server-side; turbotui was always sending a correct body, the server was dropping it.

---

## 5. Regression risks & mitigations

- **False GREEN (test passes for the wrong reason).** Mitigated by the transient-revert verification in §0: we confirm RED on `(r, name, req)` before trusting the GREEN.
- **Seed rejected by save-time validation (#532/#536).** Mitigated by giving the seed a fully routable config (endpoint+model+api_type+key).
- **`modelView`↔`ModelConfig` field drift.** The view and config field sets are asserted field-by-field; if a future field is added to `ModelConfig` but not `modelView`, the round-trip silently drops it — the maximal seed makes that visible the moment someone adds an asserted field. (Out of scope to auto-detect; noted for the reviewer.)
- **Rebase at the gate.** Rebase onto current `origin/main` (already contains #536's resources.go). The PR is file-disjoint from in-flight #534 (`ui/tui/overall_stats.go`) and touches neither `api_client.go` nor `remote_handlers.go`, so it's conflict-free despite the coarse ui/tui↔server heuristic.

---

## 6. turbotui seam (reference only — no change)

`$HOME/work/turbotui` Models dialog → `APIClient.UpdateModel(name, cfg)` → `PUT /api/models/:name` with a body derived from the redacted GET (`has_api_key`, no `api_key`). The server is the sole owner of "empty key preserves stored key" and of binding the body. Nothing about the client needs to change; the test emulates the client faithfully so the seam is covered from the gogent side. No go.mod bump, no turbotui commit.

---

## 7. Open questions

1. **Closing #537 with no resources.go diff.** Since #536 already shipped the reorder, the PR closes #537 via the regression test plus a body note crediting `33d3fee`. Acceptable to the maintainer, or would they prefer #537 be closed separately as "fixed by #536" and this PR close only #538? Default (pending word): close both, explain in the body. *(This is the one judgment call worth a maintainer nod; everything else is mechanical.)*
2. **Test placement/name.** `internal/server/models_update_test.go` with `TestUpdateModelRoundTripPersistsAllFields` per the brief — confirm no preference for folding it into `models_create_test.go` instead. Default: new file (keeps create/update concerns separate, mirrors the brief).
