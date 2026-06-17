# gogent

A small Go coding agent with streaming model support, an agent tree with
sub-agents, and a Turbo-Vision-style multi-session TUI.

---

## ⚠️ DANGER — NO PERMISSION SYSTEM YET ⚠️

> **gogent currently runs tools (shell, file write/edit/delete) with NO
> sandboxing and NO permission prompts.**
>
> It will happily `rm -rf` your home directory, overwrite your files, or run
> any command the model decides to run — if it feels like it. 🙂
>
> **Do not point it at anything you care about.** Run it in a throwaway VM or
> container. You have been warned.

---

## Build & run

```sh
go build ./...
./gogent
```

Configuration lives in `~/.gogent/config.json`. The default LAN endpoint honors
the `GOGENT_MODEL_URL` environment variable.

## Status

Early / experimental (0.x). The model-connection layer (`internal/model`) is
intended to be split out into its own library later.
