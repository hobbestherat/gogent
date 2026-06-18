---
name: code-review
description: Review code changes for bugs, security, and correctness with a high signal-to-noise checklist
---

# Code Review

Use this skill when asked to review a diff, pull request, or a set of changes.
The goal is a high signal-to-noise review: surface issues that genuinely matter
and stay quiet about trivia.

## Process

1. Get the changes. Prefer the smallest accurate scope:
   - `git diff` (unstaged), `git diff --staged`, or `git diff <base>...HEAD` for a branch.
   - In an SVN repo use `svn diff`.
2. Read each changed hunk in the context of the surrounding file, not in isolation.
3. For every file, walk the checklist below.
4. Report findings grouped by severity. Cite `file:line`. Suggest a concrete fix.

## What to flag (in priority order)

1. **Correctness / logic bugs** — off-by-one, inverted conditions, wrong operator,
   missing `return`, unhandled `nil`/`null`, incorrect error propagation.
2. **Security** — injection (SQL/shell/HTML), unvalidated input, path traversal,
   secrets committed to source, missing authz checks, unsafe deserialization.
3. **Resource & concurrency** — leaked files/handles/goroutines, missing
   `defer close`, data races, lock ordering, unbounded growth.
4. **Error handling** — swallowed errors, errors logged but not handled, panics on
   recoverable conditions, losing the original error context.
5. **API & data contracts** — breaking changes to public signatures, serialized
   field renames, ID precision (64-bit IDs must not round-trip through JS numbers).
6. **Tests** — does new behavior have coverage? Do changed paths break existing tests?

## What NOT to comment on

- Formatting, import order, or anything a linter/formatter owns.
- Pure style preferences that don't change behavior.
- Speculative "you could also" rewrites unrelated to the change.

## Output format

```
### Blocking
- path/to/file.go:42 — <problem>. <why it matters>. Fix: <concrete suggestion>.

### Non-blocking
- path/to/other.go:13 — <minor concern>.

### Looks good
- <one line on what is solid, if anything notable>
```

If you found nothing substantive, say so plainly rather than inventing nits.
