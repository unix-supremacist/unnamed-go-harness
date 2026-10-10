# AGENTS.md

Guidance for coding agents working in this repo. `CLAUDE.md` and `GEMINI.md` are symlinks to this
file, and every `.md` in `docs/` is symlinked into the repo root — edit the file in `docs/`, never the
root link.

**ugh** is a small personal coding-agent harness in Go (module `github.com/unix/ugh`): OpenCode Go /
OpenRouter / Codex models, a tool loop with a permission system, recursive subagents, a `council`
tool, and a Bubble Tea TUI with resumable JSONL sessions. User-facing docs are in
[README.md](README.md).

## Build and test

```sh
go build -o ugh ./cmd/ugh
go test ./... -count=1
go vet ./...
```

## Layout

```
cmd/ugh/              CLI entry point: TUI, headless `-p`, and subcommands
                      (auth, login, eval, tree, usage, send, watch, log)
internal/
  agent/              tool loop, subagent spawn/async/tree, council, routing and
                      orchestration, context ladder, recall, digest, compaction,
                      AGENTS.md instruction loading, ask_user
  llm/                provider contract and normalized message types ("provider/model" refs)
    chat/             OpenAI-compatible Chat Completions SSE client (OpenCode Go, OpenRouter)
    codex/            Codex Responses SSE client
  provider/           resolves model refs into clients; model registry; assembles the perm gate
  tools/              built-in tools: read, write, edit, ls, grep, find, bash
  perm/               permission system: Decide, y/a/n/s prompt broker, scrubbed JSONL decision log
    static/           programmatic classifier (parses bash with mvdan.cc/sh; no os/exec)
    model/            model-backed classifiers (Jev, Clef, Respan) and untrusted-output check
  policy/             purposes, delegation tiers and model families for routing
  admission/          cross-process premium-quota admission for Codex Sol/Astra
  worktree/           harness-managed git worktree lifecycle (locking, integration, cleanup)
  provstate/          shared record of exhausted/unavailable providers (~/.local/state/ugh)
  usage/              per-provider subscription/credit usage reporting and ledger
  auth/               credential store (keyring or file backend); secrets masked everywhere
  config/             TOML config (~/.config/ugh/config.toml) over built-in defaults
  session/            append-only JSONL session persistence for resume
  tui/                Bubble Tea interface: transcript, editor, footer, modals, slash commands
evals/council/        council eval scenarios and reports
docs/                 README, AGENTS.md (+ CLAUDE.md/GEMINI.md links), code of conduct
LICENSE               OSL-3.0
```

## Conventions

- Every package starts with a `// Package …` comment stating its role; keep it accurate when a
  package's responsibility changes.
- Tests sit next to the code (`*_test.go`); regression tests are named `*_regression_test.go`.
  Provider clients are tested against recorded SSE fixtures in `testdata/`.
- Secrets never appear in errors, logs or display output (see `auth.MaskKey`, `perm` scrubbing).
- `internal/perm/static` must not import `os/exec` or `syscall` (enforced by a test).
