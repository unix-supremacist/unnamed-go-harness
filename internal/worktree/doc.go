// Package worktree implements the Phase 3 worktree lifecycle contract
// from PLAN_ROUTING_ORCHESTRATION.md for harness-managed Git worktrees.
//
// It provides:
//   - Isolated worktree allocation for concurrent feature writers.
//   - Clean committed base enforcement, with dirty parent checkouts allowed
//     only via explicit disclosure (dirty changes absent) or authorized snapshot.
//   - Metadata recording (repo, base commit, branch, run, agent, task,
//     lifecycle, validation, result) compatible with existing session records,
//     without an external ownership database.
//   - Shared repository serialization via OS advisory flock on the Git
//     common directory with O_CLOEXEC file descriptors.
//   - Single active writer enforcement: refusing reuse, handoff, or cleanup
//     while active execution or surviving processes exist.
//   - Preservation of unfinished work on failure or cancellation, exposing
//     explicit recovery and cleanup operations gated by permission callbacks.
//   - Narrow, serialized integration checking target branch commit and dirty
//     state immediately before permission-gated merge or cherry-pick, stopping
//     cleanly on conflicts with no auto-merge.
//
// NO SANDBOX CLAIMS:
// Worktrees provide separate working directories and branch isolation for Git
// operations. They DO NOT constitute an OS security sandbox. Commands executed
// within a worktree run with standard host user privileges and are not prevented
// by worktree boundaries from accessing the filesystem, network, or other host
// resources outside the checkout.
package worktree
