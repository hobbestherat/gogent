---
name: debugging
description: Systematically find the root cause of a bug instead of guessing at fixes
---

# Debugging

Use this skill when investigating a bug, crash, test failure, or unexpected
behavior. The aim is to find the *root cause*, not to paper over a symptom.

## Method

1. **Reproduce.** Establish a reliable, minimal way to trigger the bug. If you
   can't reproduce it, you can't confirm a fix. Capture the exact command, input,
   and environment.
2. **Read the evidence.** Stack trace, error message, and logs point at the
   failure site. Read the *first* error, not the last — later errors are often
   cascades.
3. **Form one hypothesis.** State what you think is wrong and why, in terms of
   specific code. Avoid changing several things at once.
4. **Locate.** Narrow the failing region: bisect the input, add targeted logging
   or assertions, or use a debugger. Confirm the actual values at the boundary,
   not what you assume they are.
5. **Test the hypothesis.** Make the smallest change that would prove or disprove
   it. If disproved, discard it and form a new one — don't pile on patches.
6. **Fix the cause.** Address the underlying defect. Then check for sibling
   instances of the same mistake elsewhere.
7. **Verify.** Reproduce again to confirm the bug is gone, and run the surrounding
   tests/build to confirm you didn't break anything.
8. **Guard it.** Add or extend a test that fails before the fix and passes after.

## Useful tactics

- **Bisect** a regression with `git bisect` (or by reverting suspect changes) to
  find the commit that introduced it.
- **Binary-search the data**: halve the input until the smallest failing case
  remains.
- **Check the boundaries**: nil/empty/zero, first/last element, off-by-one,
  unicode, timezones, concurrency interleavings.
- **Question assumptions**: print or assert the value you're "sure" about. Bugs
  hide where you stopped looking.
- **Read the source of the dependency** rather than guessing its behavior.

## Anti-patterns to avoid

- Shotgun debugging: changing many things hoping one helps.
- Adding `try/catch`/`recover` to silence an error without understanding it.
- Declaring it fixed without reproducing the original failure first.
