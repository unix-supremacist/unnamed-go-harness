// Package admission implements the cross-process premium quota admission contract
// for Codex Sol and Astra requests specified in PLAN_ROUTING_ORCHESTRATION.md (§2).
//
// # Premium Quota Contract & Invariants
//
// 1. Scope & Identity:
//   - Coordination operates across UGH processes on the same local machine and OS user,
//     keyed by stable Codex account identity (auth.Credential.AccountID).
//   - Cross-machine coordination is out of scope.
//   - Unknown or missing account identity maps conservatively to a shared fallback
//     bucket ("shared_unknown") with the basic preset, rather than creating an unrestricted
//     fresh bucket.
//
// 2. Covered Models & Exclusions:
//   - The premium quota pool covers Codex Sol and Codex Astra requests only, including root turns.
//   - Non-Codex routes (OpenRouter, OpenCode Zen/Go) do not consume Codex capacity.
//   - Codex Luna does not consume a premium slot.
//   - Non-premium and unmetered requests return a pass-through Handle whose Close is a no-op.
//
// 3. Plan Presets & Dynamic Account Config:
//   - Combined Sol/Astra limit presets:
//   - basic: 1
//   - 5x ($100): 5
//   - 10x ($200): 10
//   - 25x ($500): 25
//   - Unknown or invalid preset defaults conservatively to basic (1).
//   - Account-scoped config is stored atomically (temp file + fsync + rename) under an
//     account serialization lock (admin.lock) and reread on each admission attempt.
//   - Independently cached process-specific limits are avoided.
//   - When the configured limit decreases, running requests are NEVER cancelled; they
//     run to completion, while subsequent admissions respect the reduced cap.
//
// 4. Council Coordinator Sol Exemption:
//   - Council Sol coordination/synthesis has a narrow, separate exempt bucket (default
//     1 simultaneous request per account).
//   - Exemption authority is narrowly typed in Go code (ExemptionCouncilCoordinatorSol),
//     minted only by harness control flow—never parsed from model strings or metadata.
//   - Strictly limited to Codex Sol. Codex Astra and non-Sol models cannot use this exemption.
//   - Both regular and exempt bucket capacities are validated before admission.
//
// 5. Root Priority & Waiter Registration:
//   - Active requests are counted, not idle sessions or waiting orchestrators.
//   - An idle root holds no slots, even on the basic preset.
//   - Running children are never cancelled or preempted.
//   - When capacity is occupied:
//   - Ordinary child requests perform nonblocking capacity fallback (ErrCapacityExceeded)
//     so the harness can fall back to alternative tiers without blocking.
//   - Root requests receive priority for the NEXT available slot.
//   - Root waiter registration is backed by an OS flock on an individual waiter file.
//     If the root process terminates or crashes, the OS kernel unconditionally releases
//     the lock. Subsequent processes detect the unheld lock, purge the stale record, and
//     do not block children on stale PID records.
//   - While a live root waiter is registered, child requests immediately fall back,
//     preserving the next slot for the root waiter.
//
// 6. Lock Lifetimes & Operating System Guarantees:
//   - Advisory locks use Linux OS flock(2) on slot descriptor files.
//   - All file descriptors are created with O_CLOEXEC (and syscall.CloseOnExec), ensuring
//     lock descriptors are never inherited by spawned subprocesses.
//   - A slot lock is held for the entire request lifetime until handle.Close() is called.
//   - handle.Close() must only be called after all caller cleanup (reading HTTP bodies,
//     streaming completion, stream error handling) is complete.
//   - If a process dies or crashes, the Linux kernel automatically cleans up all held locks.
//   - No heartbeat leases, TTL renewals, renewal-loss quarantines, or distributed fencing.
//
// 7. Request Boundary Acquisition Order:
//   - Common request boundary order:
//     1. Execution limit (e.g. Runtime tree max_live slot)
//     2. Provider limit (e.g. Runtime.Limit(provider) stream slot)
//     3. Premium quota slot (admission.Acquire)
//     4. HTTP request / stream execution
//     5. Stream completion & cleanup
//     6. Release premium quota (handle.Close())
//     7. Release provider limit (<-sem)
//     8. Release execution limit
//   - A premium slot is NEVER held while waiting for a provider stream slot, executing tools,
//     or waiting on children.
//
// 8. Error Handling & Storage Failures:
//   - Storage failures (unwritable directory, read-only filesystem, permission errors)
//     fail closed with ErrStorageUnavailable.
//
// 9. Diagnostics:
//   - Real-time diagnostics inspect slot and waiter locks to report active slot counts,
//     holder PIDs, request IDs, models, purposes, and registered root waiters.
//
// # Compact Test Matrix
//
// The test suite validates these contracts across two test files:
//
// 1. Table-driven Unit Tests (admission_test.go):
//   - TestPresets: basic (1), 5x (5), 10x (10), 25x (25), unknown/empty -> basic (1).
//   - TestClassifyModel: Codex Sol (metered), Codex Astra (metered), Codex Luna (unmetered),
//     non-Codex (unmetered).
//   - TestExemptionTyping: ExemptionCouncilCoordinatorSol permitted only for Codex Sol;
//     rejected for Astra, Luna, and non-Codex.
//   - TestConfigAtomicityAndReload: config write under lock with fsync+rename; immediate reread.
//   - TestCombinedCapEnforcement: basic preset permits 1 active; second child fails nonblocking.
//   - TestCouncilExemptCap: default 1 simultaneous exempt request; separate from combined pool.
//   - TestRootPriorityFallback: child fails nonblocking when root waiter is waiting.
//   - TestStorageFailures: read-only directory returns ErrStorageUnavailable.
//   - TestHandleIdempotency: multiple Close() calls safe; double release prevented.
//
// 2. Subprocess Tests (subprocess_test.go):
//   - TestSubprocessCaps: independent processes respect configured combined concurrency caps.
//   - TestSubprocessDeath: process holding a slot killed with SIGKILL; OS releases lock immediately;
//     second process acquires slot without delay or stale lock penalty.
//   - TestSubprocessFDNonInheritance: process holding slot spawns subprocess; parent closes FD/exits;
//     lock is free because O_CLOEXEC prevented child inheritance.
//   - TestSubprocessRootPriority: root waiter in separate process preempts ordinary child in another
//     process from acquiring the next freed slot.
//   - TestSubprocessSharedLimitReload: one process changes account limit from 1 to 5; second process
//     immediately acquires additional capacity without restart; limit decrease from 5 to 1 does not
//     cancel in-flight streams.
//   - TestSubprocessUnknownAccountFallback: multiple processes without account ID coordinate within
//     the shared fallback bucket.
package admission
