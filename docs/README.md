# ugh

A small personal coding-agent harness in Go. It runs OpenCode Go / OpenCode Zen / OpenRouter / Codex
models (plus any OpenAI-compatible endpoint you configure) through a tool loop with a layered
permission system, recursive background subagents with quota-aware routing, a multi-model `council`
tool, managed git worktrees, a context "ladder" that keeps long runs cheap, and a Bubble Tea TUI with
resumable JSONL sessions.

- [Quick start](#quick-start)
- [Subscriptions: what you get, in ugh and out of it](#subscriptions-what-you-get-in-ugh-and-out-of-it)
- [Providers and credentials](#providers-and-credentials)
- [Running](#running)
- [TUI](#tui)
- [Tools](#tools)
- [Permissions](#permissions)
- [Subagents, routing and council](#subagents-routing-and-council)
- [Context: ladder, instructions, compaction](#context-ladder-instructions-compaction)
- [Model and role cards](#model-and-role-cards)
- [Files and configuration](#files-and-configuration)

## Quick start

```sh
go build -o ugh ./cmd/ugh
ugh login opencode         # paste an OpenCode key (backs opencode-go and opencode-zen)
ugh auth set openrouter    # optional: OpenRouter key from stdin (also used by the Jev/Clef routers)
ugh login codex            # optional: ChatGPT/Codex subscription (browser PKCE, or --device)
ugh                        # TUI in the current directory
```

The default model is `[models] default` if set, otherwise MiMo Pro: `openrouter/xiaomi/mimo-v2.6-pro`
with an OpenRouter key, else `opencode-go/mimo-v2.6-pro`. Thinking defaults to `high`. The permission
classifiers, the subagent router and the context ladder call the Jev/Clef decision models on
OpenRouter, so an OpenRouter key is strongly recommended even if you never chat through it.

## Subscriptions: what you get, in ugh and out of it

ugh talks to four kinds of backend: **Codex** (your ChatGPT subscription, via OAuth),
**OpenCode Go** (subscription), **OpenCode Zen** (pay as you go) and **OpenRouter** (pay as you go,
plus BYOK). Google and Anthropic consumer plans cannot be used by ugh directly. Some of them include
API credits, and you can spend those in ugh by adding the key to OpenRouter's **BYOK** (bring your
own key) integrations. Requests to `openrouter/google/…` or `openrouter/anthropic/…` are then billed
to your own Google or Anthropic account. OpenCode BYOK works too: connections made in the OpenCode
Console show up as `opencode-<name>` providers after `ugh login opencode-console`. OpenRouter is the
recommended route.

| plan | in ugh | outside ugh |
|---|---|---|
| Google AI Plus | nothing usable | — |
| Google AI Pro | $10/month of Gemini API credit via OpenRouter BYOK¹ | Google Drive storage, YouTube Premium Lite, Gemini apps |
| Google AI Ultra 5x | $40/month of Gemini API credit via OpenRouter BYOK¹ | as Pro |
| Google AI Ultra 20x | $100/month of Gemini API credit via OpenRouter BYOK¹ | as Pro |
| Claude Pro | nothing usable | Claude apps and Claude Code |
| Claude Max 5x | $100 of Anthropic API credit via OpenRouter BYOK | the best harness experience outside ugh (Claude Code) |
| Claude Max 20x | $200 of Anthropic API credit via OpenRouter BYOK | as Max 5x, more usage |
| Codex Free | direct (`ugh login codex`); realistically an occasional Luna subagent or council seat | — |
| Codex Go | direct; basic premium pool | — |
| Codex Plus | direct; basic premium pool | — |
| Codex Pro $100 | direct; 5x premium pool (auto-detected) | — |
| Codex Pro $200 | direct; 10x premium pool (auto-detected) | — |
| Codex Pro $500 | direct; 25x premium pool (set `chatgpt_plan = "25x"`) | — |
| OpenCode Go | direct (`opencode-go/…`); the default provider | — |
| OpenCode Go Plus | direct, same provider as Go with more usage | — |

¹ Requires a Pro or higher plan *and* a $5 purchase of Google AI credits.

**Google AI.** Gemini subscription models cannot be used in ugh directly. From Pro upward, once
you have bought $5 of Google AI credits, the plan includes free monthly Gemini API credit: $10 on
Pro, $40 on Ultra 5x, $100 on Ultra 20x. Put that API key into OpenRouter BYOK and ugh can spend it
on the Gemini routes (`openrouter/google/gemini-3.8-flash`, a council panel family). It is not the
best value for ugh alone, but the plan also pays for Google Drive and YouTube Premium Lite, so it
pays off elsewhere too. That is especially true at the Pro tier.

**Claude.** Claude subscription models cannot be used in ugh directly either. Max 5x and Max 20x
include $100 and $200 of API credit, which you can spend through OpenRouter BYOK on the
`openrouter/anthropic/…` routes (Haiku, Sonnet, Opus, Fable). Max also gives the best experience in
a harness outside ugh, so it makes a good companion subscription.

**Codex.** Codex brings no benefits outside ugh, but every tier works in ugh directly, Free
included, for Sol, Astra and Luna. ugh reads your plan from the Codex login (`[routing]
chatgpt_plan = "auto"`) to size the shared Sol/Astra pool: Pro $100 → 5x, Pro $200 → 10x, everything
else → basic. The $500 tier cannot be detected, so set `chatgpt_plan = "25x"` yourself. Free is
mostly useful for an occasional Luna subagent or council member.

**OpenCode Go.** Its model selection gives it extreme value: it just keeps going, especially when
you mostly use it for MiMo Flash subagents (MiMo Flash heads the default `low` tier and prefers Go).
**OpenCode Go Plus** is more of the same. You get less per dollar on some models, but it is still worth
it if you need the usage.

For anything ugh routes automatically, tell it what you pay for: `[routing.access] openrouter =
"granted"` (see [routing](#subagents-routing-and-council)).

## Providers and credentials

| provider | transport | credential |
|---|---|---|
| `opencode-go` | Chat Completions (`opencode.ai/zen/go/v1`) | `ugh login opencode` / `OPENCODE_API_KEY` |
| `opencode-zen` | Chat Completions (`opencode.ai/zen/v1`) | the same OpenCode key |
| `openrouter` | Chat Completions | `ugh auth set openrouter` / `OPENROUTER_API_KEY` |
| `codex` | Responses SSE | `ugh login codex` (OAuth) |
| `opencode-<name>` | Chat Completions | `ugh login opencode-console`: BYOK connections made in the OpenCode Console become providers (e.g. `opencode-openrouter`) |
| `[providers.<name>]` | Chat Completions | `ugh login <name>` / `<NAME>_API_KEY` (or `key_env`) |

Keys come from the environment first, then the credential store (Secret Service keyring when
available, otherwise `~/.config/ugh/credentials.json` with 0700/0600; `[auth] store = "auto" |
"keyring" | "file"`). Secrets are registered with the scrubber and never appear in errors, logs,
prompts or sessions.

```sh
ugh auth list                      # providers, masked keys, backend in use
ugh auth set openrouter            # reads the key from stdin (no echo, never argv)
ugh auth rm openrouter
ugh auth import pi                 # one-shot import of Pi's static keys (--path --only a,b --force)
ugh login opencode                 # OpenCode key, validated against /models before it is stored
ugh login codex                    # independent Codex OAuth (--device, --bg, --status)
ugh login opencode-console         # OpenCode Console login (browser); exposes BYOK connections
ugh usage                          # OpenCode Go and Codex usage windows, OpenRouter spend + balance
```

Codex OAuth is not imported from Pi by default. OpenAI rotates refresh tokens, so sharing one would
log the other tool out; `ugh login codex` creates an independent session (`auth import pi --codex`
shares it anyway).

A custom OpenAI-compatible endpoint:

```toml
[providers.myhost]
base_url = "https://example.com/v1"
key_env = "MYHOST_KEY"          # default MYHOST_API_KEY
headers = { "X-Extra" = "1" }
no_thinking = ["some-model-"]   # model id prefixes that reject `reasoning`
usage = "none"                  # opencode | openrouter | none
```

### OpenRouter flex

Gemini on OpenRouter has opt-in flex endpoints: half the standard price, best effort. ugh tries
them first for matching models. OpenRouter moves to normal routing by itself when a flex endpoint
errors. A flex queue can also be slow without erroring, so if nothing has streamed after
`first_token_s`, ugh cancels the request and resends it on normal routing. No output has been shown
at that point, so the agent never sees the retry. The root agent skips flex by default, since you
are waiting on it. With Google AI credits on OpenRouter BYOK, flex stretches the monthly credit
about twice as far.

```toml
[flex]
enabled = true
root = false                                   # also use flex for the root agent
models = ["google/gemini-"]                    # OpenRouter model id prefixes
endpoints = ["google-ai-studio/flex", "google-vertex/global/flex"]
first_token_s = 30
```

## Running

```sh
ugh                                # TUI
ugh -resume                        # resume the latest session for this cwd
ugh -p "create hello.go and run it" -m opencode-go/mimo-v2.6-pro   # headless
```

| flag | |
|---|---|
| `-p "…"` | run one prompt headless and exit |
| `-m ref` | model ref, `provider/model` (e.g. `openrouter/xiaomi/mimo-v2.6-pro`, `codex/gpt-6.1-sol`) or a model-card id |
| `-thinking` | `off · minimal · low · medium · high · xhigh · max`, clamped to what the model supports |
| `-mode` | `manual · classify · auto-safe · auto` (default `classify`) |
| `-cwd`, `-system` | working directory; system prompt |
| `-resume` | continue the most recent session for this cwd |
| `-max-turns` | root turn budget (default `agent.max_turns_root`, 200) |

Headless permission prompts arrive on stdin (`y/a/n/s`); with no stdin, `headless_prompt` decides.

Watching and steering a run from another terminal:

```sh
ugh watch                      # live tree of the most recently active run (--session, --interval, --once)
ugh log root/worker-2 -f       # follow one agent: messages, tool calls, trimmed results
ugh log root/worker-2 --full   # the whole transcript with complete tool output
ugh send root/worker-2 "these blocks use the wrong presentation"
ugh send --subtree root/worker-2 "..."   # … and every running agent below it
ugh tree --session FILE        # a run's whole tree from any of its session files
ugh eval council               # council vs single model vs oracle report (-q, -out, -timeout)
```

`watch` shows each agent's status, turns, tool calls, idle time and latest step. The statuses are:
- `thinking`: a model turn is streaming or waiting for a stream slot
- `tool`: running a tool
- `waiting`: blocked on its own children
- `queued`
- `ok`, `turn-out`, `error` or `cancelled` once it has finished

`watch`, `log` and `tree` read the session files, so they work on any run, even one already in
progress.

## TUI

- enter sends · alt+enter adds a newline · esc cancels (a second press is required when subagents
  are running) · ctrl+c clears (twice quits)
- ctrl+o expands tool calls and thinking (edit calls show a colored diff)
- ctrl+t opens the agent tree: ↑↓ + enter (or `/view <agent-id>`) switches the transcript to that
  agent, followed live. Typing then sends to that agent; esc returns to root.
- ctrl+l opens the model picker, populated live from each provider. In the picker, tab shows only
  scoped models. `/scoped-models` picks the subset (ctrl+a / ctrl+x select all / none), and
  ctrl+p / ctrl+shift+p cycle through it.
- ctrl+r opens the resume picker · shift+tab cycles the permission mode
- tab completes `/commands` and `@file` paths
- Thinking starts at `high`. The levels a model accepts come from its provider's model listing where
  the provider reports them (OpenRouter does, and `off` disappears when reasoning is mandatory), else
  from the model card, else every level is offered. An unsupported level clamps to the closest one.
  Bare `/thinking` cycles. Listings are cached and refreshed in the background at most daily (and
  whenever the model picker opens).

| command | aliases | |
|---|---|---|
| `/model [ref]` | `/models` | choose the conversation model |
| `/scoped-models` | | choose which models `/model` and ctrl+p cycle |
| `/thinking [level]` | `/effort`, `/think` | bare cycles |
| `/mode [mode]` | `/permissions`, `/approvals` | permission mode; bare cycles |
| `/new` | `/clear`, `/reset` | fresh session |
| `/resume [n]` | `/continue`, `/sessions`, `/history` | session picker |
| `/compact` | `/summarize` | summarize older turns in place |
| `/compact-model [ref\|default]` | | which model writes summaries (persists) |
| `/context` | | what the ladder, instructions and compaction are doing |
| `/usage` | `/cost`, `/quota` | plan usage and spend |
| `/routing` | `/status` | routing and admission status |
| `/login` | | log in to a provider |
| `/tree`, `/view [id]` | `/agents` (tree) | watch or enter subagents |
| `/send <id> <msg>` | | message a running subagent |
| `/ask <question>` | | answer from a live snapshot of the agent tree without interrupting |
| `/copy` | | last assistant message to the clipboard (OSC 52) |
| `/init` | | have the agent write `AGENTS.md` files for the project |
| `/help` | `/h`, `/?` | |
| `/quit` | `/q`, `/exit` | |

`/model /new /resume /context /compact /init` are refused while a run is in progress. A message sent
while root is busy is queued for its next turn.

The **resume picker** lists this project's root sessions, newest activity first (`title · 3h ago ·
model · 42 msgs`). Type to filter and press enter to open. The transcript is replayed, compaction
markers included, and up-arrow recalls the session's earlier prompts. Subagent and council sessions
are never resume targets.

The **permission modal** shows every classifier verdict. `y` allows once, `a` allows this
command/path for the session, `n` denies (with an optional reason), `s` stops the run.

The **ask modal** opens when any agent calls `ask_user`:
- ↑↓ or 1–9 move, space toggles in multi-select, enter confirms and advances
- shift+tab goes back, esc declines
- "Other" opens a private write-in field; your main draft is untouched

A permission prompt preempts the ask modal, and the half-answered question is restored afterwards.
`[ui] ask_timeout = "5m"` optionally bounds the wait. `[ui] theme = "dark" | "light" | "system"`.

## Tools

| tool | |
|---|---|
| `read`, `write`, `edit`, `ls`, `grep`, `find` | file tools (`[tools] timeout_s`, default 120) |
| `bash` | shell; timeout default 120 s, max 600. Output is streamed live and trimmed past 450 lines / 14 KiB. The whole output (up to a 1 MiB capture) is kept for `recall`. |
| `spawn`, `agent_status`, `wait_agents`, `resume_agent` | background subagents (below) |
| `council` | multi-model council (below) |
| `worktree_status`, `worktree_integrate`, `worktree_cleanup` | managed worktrees (orchestrator runs) |
| `recall` | page an old tool result back (`recall("c42")`, `start_line`, `grep`) |
| `ask_user` | 1–4 multiple-choice questions to you (TUI only) |
| `gutcheck` | 1–8 bounded advisory questions answered by a decisions model; never an approval |

Every agent gets a private scratch directory as `$UGH_SCRATCH` in bash
(`~/.local/state/ugh/scratch/<run>/<agent>`).

## Permissions

```toml
[permissions]
mode = "classify"          # manual | classify | auto-safe | auto
aggregate = "any"          # any | all | "n:2"
aggregate_action = "max"   # max | prompt | stop
headless_prompt = "deny"   # deny | stop | allow
workspace_roots = []       # extra roots that count as "inside the workspace"

[permissions.classifiers.static_normal]
state = "on"               # off | log | on
on_flag = "stop"           # prompt | stop
on_error = "ignore"        # prompt | stop | ignore

[permissions.tiers]        # model classifier panel per triage tier
high = ["jev_or", "clef_or"]
```

Static rules first triage every call into a tier. Each tier has its own panel of model classifiers,
and in classify mode any flag prompts:

| tier | what | models |
|---|---|---|
| trivial | read-only work inside the workspace, builds/tests, orchestration | none |
| low | edits in the workspace, scripts, unknown commands, `git add`, `mkdir` | Jev |
| middle | `rm`/`mv`/`sed -i`/`kill`, `git commit`/`reset`, redirects into files | Jev |
| high | network, `git push`, sudo, sensitive paths (even reads), writes outside the workspace, unparseable commands, anything a static rule flags | Jev |

Static analysis judges what a command *does*. It parses bash properly: quoting, `$(…)`, `-c`
strings, pipelines and wrappers. The model is asked only about *intent*: on task, off task,
following injected instructions, or unclear. Static analysis cannot see that. Clef, Clef Flash and
Luna Decisions stay configured, so any tier can get a bigger panel.

"Inside the workspace" covers:
- the cwd and its git root
- `workspace_roots`
- the temp dir
- the agents' scratch dirs

The modes:
- **`classify`** (default) prompts on flags.
- **`manual`** prompts on everything.
- **`auto`** runs every call immediately and only logs verdicts. It applies nothing, not even
  static stops.
- **`auto-safe`** is for long unattended runs. Static stops (`static_normal`, or any static
  classifier with `on_flag = "stop"`) still halt the offending agent. Nothing prompts or denies, and
  the model classifiers run in the background and are logged.

A stopped child returns `status: error` to its parent, and the rest of the tree keeps going.
Headless `-p` in classify mode turns prompts into denials (`headless_prompt`). Denials are listed at
the end of the run and in each child's spawn result. For hours-long runs, prefer
`-mode auto-safe`.

In classify mode a call prompts only when a static rule or a model classifier flags it. Tool output
(except from trivial calls) is also screened for injected instructions. A flagged output never
prompts by itself; it makes the model panel judge that agent's later calls, trivial ones included,
and tells it that earlier output looked injected, so Jev's `injected` question decides.

Every decision is logged to `~/.local/state/ugh/decisions.jsonl`, with the tier, the reason and the
billed cost of each verdict. Secrets are scrubbed.

## Subagents, routing and council

`spawn` starts children in the background and returns at once, so a parent keeps working while
they run:
- Finished results are injected at the parent's next turn.
- `agent_status` shows live progress.
- `wait_agents` blocks when there is nothing else to do (`spawn` with `wait: true` blocks
  immediately).
- `resume_agent` continues a finished child (typically a turn-out) with its full history and a
  fresh budget.
- A parent that answers while children are still running waits for them, or for a message from you.

At depth 4, `spawn` is no longer offered. Messages from `ugh send` / `/send` arrive before the
agent's next turn, or within about 2 s while it waits on children.

Each spawn result starts with `### <id> · status: ok|turn-out|error|cancelled · model · turns · tools
· tokens`, followed by the child's cwd and session file. Two turns before its budget runs out, an
agent is told to write its state and report. The last turn offers no tools. An agent that still has
no answer returns a `[TURN-OUT]` report.

```toml
[agent]
max_turns_root = 200
max_turns = 80                 # children without a role override
max_turns_by_role = { worker = 80, scout = 60, reviewer = 40, planner = 30, oracle = 30, "log-parser" = 20 }
max_turns_by_depth = { 1 = 150 }   # a child gets max(role, depth)
max_turns_cap = 300            # ceiling for a per-task max_turns
auto_compact = 0.7             # compact past 70% of the context window (0 = never)
child_timeout = "0"            # wall-clock budget per child, e.g. "45m" (tasks can set max_minutes)

[limits]
max_live = 64                  # agents running at once; parents waiting on spawn don't count
streams = { codex = 3, "opencode-go" = 6 }   # per provider; others use routing.provider_concurrency (4)
reserve = { codex = 90, "opencode-go" = 100, openrouter = 100 }   # % of a usage window after which new spawns avoid the provider
```

**Routing** protects scarce quota:

- **The router decides, not the spawning model.** `spawn` takes `task`, `cwd` and an optional
  `role` suggestion.
  - The preferred router (`[routing] preferred_router = "jev"`, with `"clef"` as the fallback) picks
    role, tier (`low`/`high`) and thinking. If it leaves thinking unset, subagents get `high`.
  - Each tier is a chain of model families. `[routing.tiers]` names the first one and
    `[routing.fallback]` links each family to the next; a link set to `""` ends the chain. Within a
    family, providers are tried in the order fixed in `internal/policy`.

    ```toml
    [routing.tiers]                # defaults
    low = "mimo-v2.6-flash"        # → gpt-luna → claude-haiku-5.5 → deepseek-v4.1-flash
    high = "gpt-sol"               # → mimo-v2.6-pro → claude-sonnet-5.5
    oracle = "gpt-astra"           # → claude-fable-5.1

    [routing.fallback]             # defaults
    "mimo-v2.6-flash" = "gpt-luna"
    "gpt-luna" = "claude-haiku-5.5"
    "claude-haiku-5.5" = "deepseek-v4.1-flash"
    "gpt-sol" = "mimo-v2.6-pro"
    "mimo-v2.6-pro" = "claude-sonnet-5.5"
    "gpt-astra" = "claude-fable-5.1"
    ```

    Delegated Sol and Astra always run on Codex, and Astra only as the oracle. Unknown family ids are
    skipped with a warning at startup.
  - If neither router answers, nothing is launched and the call returns `routing-unavailable`.
    `spawn` then exposes `tier` (`low`/`high`) and `thinking` for `traditional_cooldown_s` (default
    120 s) before the routers are retried. The caller only stands in for the router's tier decision;
    the tier's chain still picks the model.
- **Automatic delegation needs confirmed access.** A stored credential alone counts as *unknown*.
  - Mark the providers you pay for with `[routing.access] openrouter = "granted"` (or per
    `provider/model`).
  - Or set `routing.trust_credentials = true` to trust every stored credential.
  - A route that completed a request in this process also counts.
- **Codex Sol/Astra share a local pool** across ugh processes on the same machine and OS user, keyed
  by the Codex account.
  - The pool is sized by `chatgpt_plan`: basic/5x/10x/25x give 1/5/10/25 concurrent requests (see
    [subscriptions](#subscriptions-what-you-get-in-ugh-and-out-of-it)).
  - Root turns wait for a slot; delegated work falls back to the next route.
  - Council Sol coordination has its own pool (`council.exempt_sol_limit`, default 1).
  - Luna and non-Codex routes don't use the pool. `/routing` shows the live state.
- **Exhausted providers are shared.** A provider that refuses with a usage limit is marked exhausted
  until its reset, for every agent and every ugh process (`~/.local/state/ugh/providers.json`, shown
  by `watch` and `usage`). A running child whose provider fails moves to the next route in its chain
  and keeps its history. A transient failure before any output (5xx, a 429 that is not a quota
  error, or a stall) is retried once on the same route first. That failover lasts only 2 minutes,
  after which the agent goes back to its preferred route.
- **`oracle`** (GPT Astra) is available only when no council can run, or after a viable council
  **with your approval**. It is never auto-approved and is refused headless.

Roles are `scout`, `worker`, `planner`, `reviewer`, `oracle`, `log-parser` and `orchestrator`. The
`orchestrator` delegates feature work and cannot edit. Each worker it spawns gets its own git
worktree under `[worktree] dir` (default: under ugh's state dir), handled with `worktree_status`,
`worktree_integrate` and `worktree_cleanup`. Nothing is merged or removed automatically, and parent
checkout changes are *not* copied into worktrees. Worktrees separate checkouts; they are not a
sandbox.

**Council** convenes angles × panel families as read-only scouts, and a coordinator synthesizes their
anonymized reports into a recommendation with evidence, dissent and an agreement map.
- The panel families are Luna, MiMo Flash, DeepSeek Flash, GLM, Qwen, Haiku and Gemini; Luna, Haiku
  and Gemini are capped at 3 seats.
- A council needs three distinct families.
- It returns a council id, outcome and coverage, and keeps reports for `council retry=<id>` after a
  failed synthesis.
- Size limits are checked before any spend.

```toml
[council]
aggregator = "mimo-v2.6-pro"   # first coordinator; then the rest of MiMo Pro → Sol → Sonnet
panel = ["glm-5.3-flash", "deepseek-v4.1-flash", "mimo-v2.6-flash", "qwen3.8-flash", "gpt-luna"]  # a custom list limits the families
default_angles = 2
max_seats = 40
max_parallel_seats = 6
max_context_words = 24000
max_work_units = 400
seat_timeout_s = 240
single = "mimo-v2.6-pro"       # single and oracle: the comparison models of `ugh eval council` only
oracle = "gpt-sol"
```

## Context: ladder, instructions, compaction

What each turn shows the model is decided per request, not just appended.

**Visibility ladder.** Old tool results are those of at least `min_chunk_bytes` that sit outside the
last `keep_recent` messages. Each is shown in one of these views:
- `full`
- `short`: the first and last 15 lines, at most 4 KiB
- `hide`: a one-line placeholder
- `digest`: needs a digest model

A Jev call picks the views once per query (prompt, steer or inbox message), and they stay frozen for
that query's tool loop so the provider's prompt cache holds. A skipped, failed or malformed decision
leaves a chunk full. Nothing is lost: the session file is the archive, and `recall` pages any result
back as a new tool result.

> **Disclosure.** The ladder sends some data to the Jev endpoint (`routing.routers.jev`, OpenRouter
> `typesafe/jev-1.13` by default): the scrubbed current request, tool names and arguments (≤200 B),
> and the first 400 B of each candidate result. Turn it off with `[context] ladder = "off"`.

```toml
[context]
ladder = "jev"            # or "off"
keep_recent = 8
min_chunk_bytes = 2048
min_savings = 8192        # skip the Jev call when the eligible chunks could not save this much
instructions = true       # AGENTS.md, below
digest_model = ""         # opt-in, e.g. "opencode-go/mimo-v2.6-flash"
digest_tools = ["bash"]
digest_min_bytes = 16384  # measured on the raw capture

[compaction]
model = ""                # summarizer; "" = the conversation model (/compact-model persists here)
threshold = 100000        # prompt tokens; 0 = no absolute limit
[compaction.thresholds]   # per model ref
# "codex/gpt-6.1-sol" = 250000
```

With a digest model, a big fresh output, or an old chunk marked `digest`, becomes a summary of at
most 4 KiB. The harness first prefixes the facts it knows itself (`exit 1 · 812 KiB raw · N bytes
discarded`), so the summary cannot misreport them. If the digest comes back empty, slow or oversize,
ugh uses the normal result instead.

**Compaction** checks the request about to be sent, after the ladder. Size is estimated, then
calibrated against reported usage. It triggers at the lowest of three limits:
- the absolute threshold (`[compaction.thresholds]` for the model, else `threshold`)
- `auto_compact × window`
- `window − 16k`

Notes:
- With an unknown window, only the absolute threshold applies.
- A failover to a smaller-window model compacts for that model.
- The footer shows `ctx 63k/100k`. The 100k default is a cost heuristic (staying below long-context
  price tiers).
- `/compact` appends a `compaction` record to the same session file and deletes nothing, so `recall`
  and resume still reach every original result.

**Instructions.** The cwd's `AGENTS.md` (or, failing that, `CLAUDE.md`) joins the system prompt. An
`AGENTS.md` in a subdirectory is shown the first time `read`, `edit` or `write` touches a file under
that directory:
- A mutation with unseen instructions does not run. Its result carries the instructions, and the
  agent re-issues the call if it still complies.
- More specific files take precedence.
- Paths outside the working directory, including through symlinks, get no instructions.
- **`bash` is not covered.**

## Model and role cards

Cards are built into ugh (`internal/agent/cards.go`); nothing is read from disk.
- **Model cards** (`gpt-sol`, `gpt-astra`, `gpt-luna`, `mimo-v2.6-pro`, `mimo-v2.6-flash`,
  `glm-5.3-flash`, `deepseek-v4.1-flash`, `qwen3.8-flash`) give a short id usable anywhere a model
  ref is (`-m gpt-sol`). Each lists its routes in preference order, and the thinking levels and
  context window to use when the provider's `/models` listing doesn't report them (OpenCode and Codex
  report neither).
- **Role cards** (`scout`, `worker`, `planner`, `reviewer`, `oracle`, `log-parser`,
  `orchestrator`) give each role its system prompt, its tool set and the purpose the router reads
  when it picks a role.

Which model runs a delegated task is decided by routing policy (`internal/policy`), not by the cards.

## Files and configuration

| path | |
|---|---|
| `~/.config/ugh/config.toml` | configuration; missing keys keep their defaults |
| `~/.config/ugh/credentials.json` | file credential backend (when no keyring) |
| `~/.local/state/ugh/sessions/<cwd-hash>/<timestamp>.jsonl` | sessions: one JSON line per message; children write their own files linked by parent id; tool results are numbered `c1, c2, …`, which `recall` and the ladder refer to |
| `<session>.jsonl.blobs/` | full bash output behind trimmed results |
| `~/.local/state/ugh/decisions.jsonl` | permission decision log |
| `~/.local/state/ugh/providers.json` | exhausted/reserved providers |
| `~/.local/state/ugh/scratch/<run>/<agent>` | `$UGH_SCRATCH` |

Other config sections: `[models] default`, `[ui] theme / ask_timeout`, `[tools] timeout_s`,
`[routing.routers.<jev|clef>]` (decision endpoints), and `[pricing."<ref>"]` (`in`, `out`, `cached`
per million tokens, or `included = true` for subscription models; the footer shows `cost n/a`
without an entry).

## License

Copyright (c) 2026 unix-supremacist. Licensed under the Open Software License version 3.0; see
[LICENSE](LICENSE).
