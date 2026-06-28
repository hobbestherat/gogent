# Design — Surface provider error reasons (un-swallow 4xx/5xx model errors)

Closes #555.

## Problem (verified against current code)

Every provider error (400/403/404/429/5xx) reaches the user as an opaque
status. The real rejection reason — which the provider returns in the response
body and which we already capture into `ModelError.RawResponse` — never reaches
`ModelError.Error()` and so never reaches the TUI.

Verified trace (line numbers current as of `bc5232c`, post-#556):

- `internal/model/connection.go:1853` `analyzeError(statusCode, response)` stores
  the full body in `RawResponse: response` on **every** branch, but `Message` is
  status-only or a fixed phrase:
  - generic catch-all `:1925` → `Message: "unexpected error: status %d"`
  - 400 sniff `:1862` (`strings.Contains(lower, "context"||"length")`) →
    `"context window overflow"` (OpenAI-shaped; misses Anthropic/Gemini wording)
  - 403/404/429/504 → fixed phrases, body discarded from the message.
- `ModelError.Error()` `:691` → `fmt.Sprintf("%s: %s", e.Type, e.Message)`. It
  reads **only** Type + Message; `RawResponse` is never consulted.
- The error is then wrapped through plain `%w` layers
  (`model_session.go:623 "complete with tools: %w"`,
  `user_session.go:1043 "model round-trip: %w"`) and surfaced as a string at
  `ui/tui/session_window.go:1779` → `sw.addError(ev.Err.Error())`. There is **no
  structured channel** — `.Error()` is the only path to the user.

So the captured reason exists but is dropped at the very first layer
(`analyzeError`). The headline example — Anthropic over-limit `max_tokens` —
contains neither "context" nor "length", so it falls through to the generic
catch-all and is reported as `generic: unexpected error: status 400`, with
`max_tokens: ... is greater than the maximum...` sitting unused in `RawResponse`.

## Chosen approach (minimal, provider-agnostic, in the model layer)

**Enrich `ModelError.Message` inside `analyzeError` with a bounded excerpt of the
provider's `error.message`, extracted by a single provider-agnostic helper.**
`Error()` already prints `Message`, so the reason surfaces with no change to
`Error()`, no change to the `%w` chain, no change to `SessionEvent`, and no TUI
change. This is the smallest sufficient fix and the one the issue points at.

### Why enrich `Message` rather than change `Error()`

Two ways to make `Error()` surface the reason:

- **(A, chosen)** append the extracted reason to `Message` in `analyzeError`.
- (B, rejected) make `Error()` read/append `RawResponse`.

(A) is localized to the HTTP-error path: only the strings produced by
`analyzeError` change; all other `ModelError` constructions (config errors,
stream-panic, empty-response, the synthetic errors in `issue487_*_test.go`) keep
their exact current `.Error()` output. (B) would change `.Error()` for **every**
`ModelError` that carries a `RawResponse`, risk double-printing (e.g.
`Message:"context window overflow"` + appended `"too long"`), and is a broader
behavioral change for a function called in many places. (A) also keeps the
"all info in one place" property the issue wants: each branch's message is
self-describing.

### New helper — `internal/model/provider_error.go` (new file)

Keeps `connection.go`'s diff small; co-locates the extractor with its tests.

```
// extractProviderMessage pulls the human-readable reason out of a provider error
// body. Every provider we target nests the reason at error.message:
//   OpenAI / Z.AI / OpenRouter / Vertex-OpenAI shim:
//       {"error":{"message":"...","type":"invalid_request_error"}}
//   Anthropic / Vertex-Anthropic:
//       {"type":"error","error":{"type":"invalid_request_error","message":"..."}}
//   Vertex native Gemini:
//       {"error":{"code":400,"message":"...","status":"INVALID_ARGUMENT"}}
// A single struct{ Error struct{ Message string } } covers all three. Fallback
// ladder for off-spec gateways, in order, first non-empty wins:
//   1. error.message (object form, the common case above)
//   2. top-level "message"            {"message":"..."}
//   3. error-as-string               {"error":"..."}     (error is a JSON string)
//   4. raw body                       (non-JSON: HTML page, plain text, gateway blurb)
// Returns "" when nothing usable is found (e.g. empty body) so the caller can
// skip appending and preserve today's status-only message.
func extractProviderMessage(body string) string

// boundedReason trims, takes the first line, and caps at modelErrReasonMaxRunes
// runes (rune-safe, no mid-codepoint split). Mirrors ui/tui.firstLine but lives
// here so internal/model never imports ui/tui. Cap chosen larger than the TUI's
// 120-rune notify cap because this is a diagnostic line, not a toast.
const modelErrReasonMaxRunes = 300
func boundedReason(s string) string
```

The extractor uses `json.Unmarshal` into small anonymous structs (stdlib only,
no new deps). Tolerant decode: a body that doesn't match a shape simply yields
"" for that rung and we fall through.

### Wiring into `analyzeError` (`connection.go:1853`)

Compute the reason once at the top:

```
reason := boundedReason(extractProviderMessage(response))
```

Then append it to each branch's `Message` only when non-empty, e.g.:

- generic catch-all: `status %d` → `status %d: <reason>` (when reason present)
- 400 overflow: `context window overflow` → `context window overflow: <reason>`
- 403 refusal / 429 / 504 / 404: same `"<phrase>: <reason>"` shape.

A tiny local join (`withReason(base, reason)` returning `base` when reason is
empty) keeps it DRY and guarantees **no regression when the body is empty or
non-JSON-and-blank** (message stays exactly as today). `RawResponse: response`
is left untouched on every branch — full body still persisted/truncated
downstream.

**`Type` and all `Stats` counters are unchanged** — only message text changes —
so the classification contract and counters survive intact.

## Scope decisions

- **PRIMARY (this PR):** the extractor + `analyzeError` wiring above. This alone
  satisfies the headline acceptance criterion: the Anthropic over-limit
  `max_tokens` 400 falls to the generic catch-all and now reads
  `generic: status 400: max_tokens: ... is greater than the maximum allowed ...`.
- **SECONDARY (deferred, follow-up):** reliable `ErrorContextOverflow`-vs-generic
  classification (OpenAI `code=="context_length_exceeded"`, Anthropic
  type/message, Gemini `status`). **Intentionally not done here** — reworking the
  400 sniff risks regressing `TestAnalyzeErrorCounters`' `context_overflow` case
  and is orthogonal to transparency (once the reason is shown the user can
  self-diagnose). Left as a clean follow-up.
- **TERTIARY (deferred, follow-up):** pre-emptive `max_tokens` clamp via a new
  `ModelCaps.MaxTokensLimit` merged in `buildRequest`'s clamp + curated rows in
  `model_overrides.go`. Robustness only; adds surface to `caps.go` /
  `model_overrides.go` / `buildRequest`. Out of scope to keep this fixes-first PR
  tight; the seam is documented for the follow-up.

## Files touched

- **`internal/model/provider_error.go`** (new): `extractProviderMessage`,
  `boundedReason`, `modelErrReasonMaxRunes`, `withReason` helper.
- **`internal/model/connection.go`**: `analyzeError` (`:1853`) — compute `reason`
  once, append to each branch's `Message`. No other function changes.
- **`internal/model/provider_error_test.go`** (new): per-provider body-shape
  fixtures for the extractor (OpenAI/Z.AI/OpenRouter, Anthropic, Vertex-Anthropic,
  Vertex Gemini, top-level message, error-as-string, non-JSON, empty) +
  bounding/rune-cap test.
- **`internal/model/connection_test.go`**: add a case asserting a 400 with a
  known body string surfaces that string in `ModelError.Error()` (e.g. the
  `max_tokens` body). Existing `TestAnalyzeErrorCounters` stays green untouched.

No other gogent files. **No turbotui change. No go.mod change. No new deps.**

## Design criteria

### (1) Goal match
Exactly the issue's ask — a transparency **fix**, not a feature/refactor. One
provider-agnostic extractor in `analyzeError` makes `ModelError.Error()` surface
the bounded `error.message` for **all** providers with **no per-provider
branching** (single `error.message` path + fallback ladder). `RawResponse` still
populated. Secondary/tertiary explicitly deferred — no scope creep.

### (2) Usability
Every 4xx/5xx now names the offending field in the user-visible error. Excerpt is
bounded (first line, ≤300 runes, rune-safe) so no multi-KB page is dumped into the
transcript. The user already drives nothing here (it's an error surface); the
right thing is now surfaced instead of swallowed. Empty/blank bodies fall back to
today's status-only message — never a dangling `": "`.

### (3) No regressions
- `Type` + every `Stats` counter unchanged → `TestAnalyzeErrorCounters`,
  `routable_config_validation_test` (404 retryability), `connection_empty_response_test`
  (504→timeout) all stay green; those tests assert Type/counters/retryability, not
  message text.
- `issue487_*` tests build synthetic `ModelError`s and assert `Message`/`RawResponse`
  directly (not via `analyzeError`) → unaffected.
- `RawResponse: response` left as-is on every branch → `session_store.go`
  `truncateRaw`/`rawResponseCap` (8 KiB) path unchanged, no transcript/persistence
  regression.
- `Error()` format string unchanged; `%w` chain unchanged; `SessionEvent.Err`
  shape unchanged → no downstream surprise.
- `gofmt`/`go build`/`go vet`/`golangci-lint`/`go test ./...` per the dev gate
  (tests without `-race` on Pi5).

### (4) Holistic design across gogent + turbotui
- Right place: the reason is dropped at its origin (`analyzeError`), so the fix
  belongs there, not by retrofitting the emit→apply→addError chain (far more
  invasive, and unnecessary since `.Error()` is the channel).
- Seam respected: `internal/model` gains **no** dependency on `ui/tui` — the
  `firstLine`-style bound is **duplicated** as `boundedReason` (≈4 lines) rather
  than imported. `ui/tui` stays free of new deps and of `internal/*`
  server/daemon imports.
- turbotui (`$HOME/work/turbotui`) is a terminal-primitive library (cells, color,
  terminfo, screen). It never imports or sees `ModelError`; the one grep hit
  (`turbotv/binding.go:208`) is an unrelated doc comment. The cross-repo seam is
  the rendered string, and we only change that string's **content**, not the type
  or shape of anything turbotui touches → **no turbotui change, by construction.**

## Regression risks (called out)
- **Noisy non-JSON fallback:** a proxy HTML 502 page yields a bounded first line
  (e.g. `<html>...`) instead of the old opaque status. Still bounded and strictly
  more informative; acceptable. Mitigation if it ever matters: a follow-up could
  suppress the raw fallback for obvious HTML; not done now to keep the helper
  simple.
- **Message double-info vs RawResponse:** the bounded excerpt in `Message`
  partially duplicates `RawResponse`. Intended split: `Message`/`.Error()` for the
  user-visible one-liner, full `RawResponse` for the transcript. No functional
  conflict.

## Open questions
- **Bound size:** 300 runes for the diagnostic excerpt (vs the TUI's 120-rune
  notify cap). Reasonable for a one-line provider reason; flag if a tighter/looser
  cap is preferred.
- **Secondary classification:** confirm it's acceptable to ship transparency
  first and leave reliable `ErrorContextOverflow` detection (and the
  `max_tokens` clamp) to follow-ups, as planned. The issue marks both as optional.
- **Rebase note:** if an in-flight `internal/model` PR (#544 and queued
  #545/#547) lands first and reshapes `analyzeError`/`buildRequest`, resolve the
  incidental overlap at the gate by rebasing onto current `origin/main` before
  finalizing.
