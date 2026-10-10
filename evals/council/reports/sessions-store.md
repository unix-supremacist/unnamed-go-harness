# Council eval: Sessions store

- date: 2026-10-06T19:04:30Z
- elapsed: 8m53s
- question file: evals/council/sessions-store.md

## Question

For ugh JSONL sessions, append-only per-message files vs SQLite for a personal single-user harness: which should the session store use? Compare simplicity, resume, and crash handling.

## Reference answer

Keep the append-only per-message JSONL files. For a personal single-user harness JSONL is the simpler store: appending one message is a single append with no schema, migrations, driver or binary format; resume is a sequential read of the file; crash handling degrades to a torn final line that is detected and dropped on load. SQLite's real advantages — transactional multi-row atomicity, concurrent writers, and queryable history — fit workloads this harness does not have (one process, one user, replay is a scan), while adding a dependency plus its own failure modes (corruption, locks, vacuum, migration). Choose SQLite only if requirements grow to concurrent writers or indexed queries over history.

## Council

Panel: 10/10 seats answered; tokens 436763 in / 46499 out.

| seat | model | lens | status |
|---|---|---|---|
| P1 | glm-5.3-flash | evidence-map | ok |
| P2 | deepseek-v4.1-flash | evidence-map | ok |
| P3 | mimo-v2.6-flash | evidence-map | ok |
| P4 | qwen3.8-flash | evidence-map | ok |
| P5 | gpt-luna | evidence-map | ok |
| P6 | glm-5.3-flash | alternatives | ok |
| P7 | deepseek-v4.1-flash | alternatives | ok |
| P8 | mimo-v2.6-flash | alternatives | ok |
| P9 | qwen3.8-flash | alternatives | ok |
| P10 | gpt-luna | alternatives | ok |

## Recommendation

**Keep the append-only JSONL store — but note the question's framing is wrong: the code implements one append-only JSONL file per *session* (one JSON record per line, one meta record first), not "per-message files."** For a personal single-user harness, the existing `internal/session/session.go` (188 lines, stdlib-only) already meets the resume and crash-tolerance requirements, and SQLite would introduce the project's first storage dependency plus schema/migration machinery to buy capabilities the code never exercises. This is a recommendation to *keep* the incumbent with small hardening fixes (below), not a claim that the incumbent is complete.

## Supporting evidence (verified against code)

**Simplicity — decisive for JSONL (verified).** I read `internal/session/session.go` in full: 188 lines, imports only `crypto/sha256`, `encoding/json`, `os`, `path/filepath`, `sort`, `strings`, `sync`, `time` + `internal/llm`. Six-function surface (Create/Open/Append/Load/List/Latest), no schema, no migrations, one mutex. `go.mod` carries no database driver (all panelists who checked agree: Bubble Tea stack, TOML, keyring, x/term, mvdan.cc/sh). PLAN.md §8 fixes this design ("Resume = read all lines … That's it") and §10 explicitly retired the older, heavier session machinery in favor of "the ~100-line JSONL" — the complexity trade was deliberately made once already.

**Resume — works as designed (verified).** Both flows are `Latest → Load → Open` (headless `cmd/ugh/run.go`, TUI `internal/tui/model.go:858`); `Load` (session.go:155-177) is `ReadFile` + line-split + unmarshal returning meta + ordered messages; `Open` (session.go:100-106) reopens `O_APPEND|O_WRONLY` to continue the same file; the reopen round-trip test asserts 5-message continuity (`session_test.go:51-66`). Per-cwd scoping is `fmt.Sprintf("%x", sum[:8])` of SHA-256 (session.go:54-56) — 16 hex chars, i.e. hex of the first 8 bytes (P2's "sha256[:8] hex" is slightly imprecise; P7 got it right).

**Crash handling — tolerance is real but bounded (verified).** `Create` uses `O_CREATE|O_EXCL` (session.go:87, no clobber); `Append` is one mutex-guarded `Write(json+"\n")` (session.go:115-133) with **no fsync anywhere in the file**; `Load` fails hard on an unparseable line 0 but `continue`s on any later bad line ("tolerate torn tail", session.go:163-168), tested with a truncated record (`session_test.go:71-77`). Verified silent-drop paths: `Append` on a nil or closed store returns `nil` (session.go:116-123), and both multi-writer call sites discard errors (`_ = sess.Append(...)` at `internal/agent/spawn.go:159`, `internal/agent/council.go:450`).

**Why SQLite's advantages are unexercised here (verified).** The only access pattern is "newest file for this cwd" plus "read whole session"; there are no cross-session queries, no in-place edits, no schema version, and child-agent fan-out is goroutine-level within one process where each child gets its **own** file (spawn.go:146, council.go:431) — so per-file sharding already avoids write contention that a single DB would *introduce*. `Parent`/`Agent` are written as tree-node IDs (tree.go:44 format) at Create (spawn.go:146, council.go:431) and I found no reader — the lineage gap is real but is a feature gap, not a storage-technology gap.

**Concrete gaps worth fixing regardless of choice (verified):**
- No `Sync()` on append or close → power loss can drop tail records silently (session.go:115-147).
- Torn-tail concatenation (P9's point, logically follows from the code): a partial write leaves a line with no `\n`; the next `Append` concatenates its record onto the torn bytes, producing one corrupt line that `Load` then silently drops — losing the first post-crash record too. Cheap fix: truncate a trailing partial line on `Open`.
- `resumeList` (model.go:798-818, verified) fully parses up to 20 session files per render just to print date/model/msg-count; the `List` 20-cap (session.go:75-77) makes older sessions unreachable via `/resume`.
- `Append` errors discarded at both agent call sites; closed-store appends vanish without error.

## Dissent

The SQLite-leaning reports (P6, P7, P9, and P8's trade-conditions) make real, verified points: unfsynced writes, silent error swallowing, the fatal-first-line/silent-skip asymmetry, O(all-files) listing, `Parent` as an unqueryable string, no cross-process lock. But their own conclusions undercut the SQLite recommendation for this scope: P6 concedes "for a single-user, single-process personal harness … JSONL's simplicity is defensible"; P9 concludes SQLite "pays off only if lineage/tree queries, meta updates, or durability guarantees become product requirements; otherwise the JSONL gaps are cheaper fixed with fsync + error propagation + tail truncation"; P10 and P8 similarly condition SQLite on multi-process writers or query needs that the inspected code does not establish. No report identified an actual SQLite implementation, schema, or driver in the repo — the comparison on the SQLite side is inference by all ten reports' own admission. The framing dispute (P2/P4 correcting "per-message files" to "per-session files") is settled in their favor by session.go:1-2, 86-98.

## Confidence

- **High** that JSONL is correct for the stated scope (personal, single-user): simplicity advantage is observed code fact, and the crash-resume requirements are met by tested behavior.
- **Medium** on long-term fit: depends on unmeasured session sizes and whether multi-process or lineage-query needs ever become real.
- Independent verification performed: full read of `internal/session/session.go`; `internal/tui/model.go:796-825` (resumeList full-parse confirmed); greps for `compact|Compact` in `internal/` (only display-side comments — `/compact` is **unimplemented**; PLAN §8's compaction is design intent, P7/P8 correct on this) and for `sess.Append|Parent:|Agent:` in `internal/agent` (Parent/Agent write-only, errors discarded — confirmed).

## Remaining risks / unresolved

- **Real session file sizes never measured** by anyone (all reports flag this). If transcripts routinely reach multi-MB, whole-file `Load` at resume and full-parse listing become the practical cost argument for an index — unresolved.
- **Multi-process use unverified**: no evidence anyone runs two `ugh` processes on one cwd; P8's grep of headless/CLI paths was scoped and mine was too. If it happens, interleaved appends can corrupt lines (no flock anywhere in the package).
- **`Parent`/`Agent` readership**: I found no reader in `internal/agent` or `internal/tui`; a reader in unsearched packages (e.g. `cmd/`) was not excluded.
- **Torn-tail-concatenation frequency** is argued from code semantics, never observed in a real crash.
- **P8's inference that `Latest` can select a child/scout session** (same-cwd children land in the same hash dir with newer timestamp names) is plausible from session.go:86, 179-188 + spawn.go:146 but untested.
- **Power-loss durability requirements** are nowhere stated in docs; the recommendation assumes OS page-cache durability is acceptable for a personal harness.

## Agreement map

```json
{"conclusions":[{"id":"c1","summary":"Framing correction: the store is one append-only JSONL file per SESSION (meta record then one record per message), not per-message files; child agents get their own files.","supported_by":["P1","P2","P3","P4","P5","P7","P8","P9","P10"],"contradicted_by":[]},{"id":"c2","summary":"Simplicity strongly favors keeping JSONL: 188-line stdlib-only store, no DB dependency in go.mod, PLAN §8 mandates this design and §10 retired heavier prior machinery.","supported_by":["P1","P2","P3","P4","P5","P10"],"contradicted_by":[]},{"id":"c3","summary":"Resume works via Latest→Load→Open (whole-file read) on both headless and TUI paths, with reopen-append continuity tested; listing is capped at 20 and resumeList fully parses each listed file just for metadata.","supported_by":["P1","P2","P3","P4","P5","P6","P7","P8","P9","P10"],"contradicted_by":[]},{"id":"c4","summary":"Crash handling is torn-tail tolerance only: single un-fsynced Write per record, Load skips unparseable lines after line 0 but fails hard on a bad first line; tested with a truncated record.","supported_by":["P1","P2","P3","P4","P5","P6","P7","P8","P9","P10"],"contradicted_by":[]},{"id":"c5","summary":"SQLite's genuine advantages (atomic commit, indexed queries, relational parent/child joins, multi-process safety) are unexercised by current access patterns; adopting it would add the first storage dependency and a schema/migration surface, and centralize writes that today's per-child files keep contention-free.","supported_by":["P1","P2","P3","P4","P5","P9","P10"],"contradicted_by":["P6","P7"]},{"id":"c6","summary":"Real JSONL gaps exist and are worth fixing regardless of store: no fsync anywhere, Append errors discarded at spawn/council call sites, silent no-op on closed/nil store, and a torn tail concatenated with the next append can destroy the first post-resume record (P9).","supported_by":["P2","P3","P4","P5","P6","P7","P8","P9","P10"],"contradicted_by":[]},{"id":"c7","summary":"Parent/Agent lineage is write-only: set to tree-node IDs at Create in spawn.go:146 and council.go:431; no reader found — SQLite advocates treat this as the strongest query gap, others flag it as unverified.","supported_by":["P6","P7","P8","P9"],"contradicted_by":[]},{"id":"c8","summary":"PLAN §8's /compact (new file seeded with summary) is design intent only — no compaction/history-rewriting code exists in the tree (grep found only display-side 'compact' comments).","supported_by":["P7","P8"],"contradicted_by":[]},{"id":"c9","summary":"Trade conclusion: for a personal single-user harness, keep append-only JSONL; SQLite becomes the better choice only if per-message durability, cross-process writers, or parent/child queries become product requirements — a condition even the SQLite-leaning reports concede.","supported_by":["P1","P2","P3","P4","P5","P8","P9","P10"],"contradicted_by":["P6","P7"]}]}
```

## Single (mimo-v2.6-pro)

tokens 91578 in / 1669 out; 489 words.

## Answer: keep the append-only per-message JSONL files (as `internal/session` already does)

**Evidence base:** `internal/session/session.go` (188 lines, a single file), `internal/session/session_test.go`, `internal/tui/model.go:796–884`, `cmd/ugh/run.go:84–145`, `PLAN.md` §8. No `database/sql`/SQLite driver exists anywhere in `internal/` or `cmd/` (grep only hits config test fixtures and the perm shell-allowlist for the `sqlite3` binary).

### Simplicity
- The whole store is one file: `Store{mu, path, f *os.File}` with `Append` = `json.Marshal` + `Write(line+"\n")` (`session.go:115–133`). No schema, migrations, driver, or binary format; sessions are plain files a user can `cat`, `grep`, or tail — which matches the harness's "personal single-user" scope and `PLAN.md:385` ("one JSON line per message … Resume = read all lines. … That's it.").
- The same pattern is deliberately reused elsewhere (`internal/perm/log.go` for `decisions.jsonl`), so one mental model covers all persistence. `PLAN.md:416` even records that the earlier heavier `internal/session/*` journal was *replaced* by this "~100-line JSONL" — the project already went through this trade-off and chose the simpler store.

### Resume
- Resume is a sequential read: `Load` splits the file into lines and decodes each (`session.go:155–177`); `List`/`Latest` pick a file per cwd-hash directory (`session.go:59–79, 179–188`). The TUI's `open` (`tui/model.go:857–884`) reseeds agent history from `loaded.Messages` and continues appending to the *same* file (`session.Open` with `O_APPEND`); headless `cmd/ugh/run.go:84–136` does the identical thing via `OnMessage → sess.Append`. Child-agent files link by `Parent`/`Agent` fields in the `meta`/message records (`session.go:19–29`, used in `agent/spawn.go:146`, `agent/council.go:431`).
- SQLite would give no resume benefit here: there are no cross-session queries, joins, or concurrent writers — just "replay one conversation in order," which is exactly what a sequential file read does. Listing is a directory glob, and `List` already caps at 20 results in memory.

### Crash handling
- Each `Append` is one atomic-ish line write; a crash mid-write can at worst tear the final line. `Load` tolerates exactly that: a malformed line that isn't the first is skipped (`session.go:163–168`, `continue // tolerate torn tail`), and `session_test.go:71–77` proves a truncated final record (`{"type":"message"`) loads fine while a corrupt *first* (meta) line is a hard error. Loss is bounded to at most one in-flight message; all prior messages are already durable in the file.
- SQLite's WAL/rollback journal would protect multi-record transactions the harness doesn't need — message writes are single-record and append-only, so there is no partial-transaction state to roll back. The remaining durability knob (fsync policy at side-effect boundaries) is orthogonal to the storage engine; if needed, `Append` can add `f.Sync()` without any schema.

**When SQLite would win:** concurrent multi-process writers, frequent mutation of past records (rewrites/compaction in place), or non-sequential queries. `ugh` has none of these: it's single-user, one serialized writer per `Store` (mutex-guarded, `O_APPEND|O_EXCL` create at `session.go:87`), and `/compact` is specified as "start a new file seeded with the summary" (`PLAN.md:386–387`) rather than in-place edits.

**Conclusion:** the JSONL store matches the harness's actual workload — append, replay-in-order, survive a torn tail — with ~100 lines of dependency-free code versus SQLite's driver, schema, and migration surface. Keep it.

## Oracle (gpt-sol)

tokens 50546 in / 854 out; 358 words.

**Use append-only JSONL, not SQLite, for this personal single-user harness.** Specifically: **one file per session, one JSON record per message**—not a separate file for every message.

This is the repository’s explicit design: `PLAN.md:383–387` specifies timestamped JSONL files under a cwd hash, resume by reading all lines, and separate child-session files linked by parent ID. The plan prioritizes a simple personal harness and explicitly rejects journal-integrity/fail-closed replay machinery (`PLAN.md:8–21`).

| Concern | JSONL in this repository | SQLite trade-off |
|---|---|---|
| **Simplicity** | Metadata header followed by message records; standard-library JSON/file operations, straightforward inspection and copying. Creation uses private directories/files; reopening uses append mode (`internal/session/session.go:19–35,37–57,81–105,115–132`). | Adds database/schema/transaction machinery without a demonstrated need for indexed queries or coordinated writers. |
| **Resume** | Load records in order, restore model/system/thinking, seed agent history, append to the same file (`internal/session/session.go:155–176`; `internal/tui/model.go:855–885`). Tests cover user → assistant tool call → tool result → assistant, then reopen and append (`internal/session/session_test.go:11–69`). | Could support indexed lookup and transactional state updates, but those are not required by the current read-all-history design. |
| **Crash handling** | A malformed first record fails loading; malformed later records are skipped. A test verifies loading after a torn final record (`internal/session/session.go:161–168`; `internal/session/session_test.go:71–77`). **This is best-effort recovery, not durable transactional storage.** | Transactions would be a reason to choose SQLite if atomic committed-state recovery became a requirement; power-loss durability would still depend on its configuration. |

**Fix two small JSONL weaknesses rather than replacing the store:**

- **Repair a torn tail before appending on resume.** Currently `Load` skips it, but `Open` appends without truncating or separating it. If the torn record has no newline, the next message joins that invalid line and is also lost on subsequent load (`session.go:100–105,131,161–168`).
- **Report persistence failures and define the durability expectation.** `Append` performs a write without `Sync`; the TUI and child-agent callbacks discard append errors (`session.go:115–145`; `internal/tui/model.go:234`; `internal/agent/spawn.go:159`). Do not promise power-loss durability or exactly-once tool execution.

**Bottom line:** JSONL fits the documented scope and already implements resume. Prefer SQLite only when requirements change to transactional multi-record state, concurrent writers to shared state, or substantial cross-session querying—not merely because crashes are possible.
