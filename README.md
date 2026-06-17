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

Each model entry has an `api_type` selecting the provider conventions:

- `openai` (default) — any OpenAI-compatible server. `endpoint` may be a full
  `.../chat/completions` URL or just a base URL (e.g. `https://host/v1`); the
  remaining paths are appended automatically.
- `zai` — the [Z.AI platform](https://docs.z.ai). Leave `endpoint` empty to use
  the built-in base URL (`https://api.z.ai/api/paas/v4`) and just set `api_key`.

In the model editor, the **Scan** button next to the model id queries the
backend's listing endpoint and replaces the model-id field with a dropdown of
the advertised models.

## Status

Early / experimental (0.x). The model-connection layer (`internal/model`) is
intended to be split out into its own library later.
