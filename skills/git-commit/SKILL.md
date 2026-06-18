---
name: git-commit
description: Write clear Conventional Commits messages and stage changes safely
---

# Git Commit

Use this skill when asked to commit changes or write a commit message.

## Before committing

1. Inspect state first: `git status` and `git diff` (and `git diff --staged`).
2. Stage deliberately. Prefer `git add <path>` over `git add -A` so you don't
   sweep in unrelated files, build output, or secrets.
3. Never commit credentials, tokens, `.env` files, or large generated artifacts.
   If you see one staged, unstage it and warn the user.

## Message format (Conventional Commits)

```
<type>(<optional scope>): <short imperative summary>

<body: what changed and WHY, wrapped ~72 cols>

<optional footer: BREAKING CHANGE:, Refs #123>
```

- **type**: `feat`, `fix`, `docs`, `refactor`, `test`, `perf`, `build`, `chore`.
- **summary**: imperative mood ("add", not "added"/"adds"), ≤ ~50 chars, no
  trailing period.
- **body**: explain the motivation and any non-obvious decisions. Skip it only for
  truly trivial changes.
- **BREAKING CHANGE:** in the footer for any incompatible API/behavior change.

## Examples

```
feat(auth): add refresh-token rotation

Tokens were valid until expiry, so a leaked token stayed usable. Rotate on
every refresh and invalidate the previous one.
```

```
fix(parser): handle empty frontmatter without panicking

Refs #214
```

## Committing

- One logical change per commit. If the diff spans unrelated concerns, split it.
- Run the project's build/tests before committing when feasible.
- If this repository's conventions require a co-author or sign-off trailer, include
  it exactly as specified.
- For SVN repositories use `svn commit -m "..."` with the same message style.
