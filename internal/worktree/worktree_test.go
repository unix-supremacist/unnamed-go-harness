package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/session"
)

func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\nOutput: %s", name, args, err, string(out))
	}
	return string(out)
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.name", "Test User")
	runCmd(t, dir, "git", "config", "user.email", "test@example.com")
	runCmd(t, dir, "git", "checkout", "-B", "master")

	initialFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Initial commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, dir, "git", "add", "README.md")
	runCmd(t, dir, "git", "commit", "-m", "initial commit")
	return dir
}

// TestFreshIsolation verifies that a fresh worktree is allocated on a separate
// branch and directory, and that commits in the worktree remain isolated from
// the parent repository until integrated.
func TestFreshIsolation(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	headCommit := runCmd(t, repo, "git", "rev-parse", "HEAD")

	opts := AllocateOptions{
		Run:         "run-101",
		Agent:       "root/1",
		Task:        "implement feature A",
		Branch:      "feature-a",
		WorktreeDir: filepath.Join(t.TempDir(), "wt-a"),
	}

	wt, err := Allocate(ctx, repo, opts)
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if wt.Meta.DirtyBasePolicy != PolicyClean {
		t.Errorf("expected PolicyClean, got %s", wt.Meta.DirtyBasePolicy)
	}
	if wt.Meta.Branch != "feature-a" {
		t.Errorf("expected branch feature-a, got %s", wt.Meta.Branch)
	}
	if wt.Meta.Run != "run-101" || wt.Meta.Agent != "root/1" {
		t.Errorf("unexpected owner metadata: %+v", wt.Meta)
	}

	// Commit work in the worktree
	workFile := filepath.Join(wt.Dir, "feature.txt")
	if err := os.WriteFile(workFile, []byte("feature content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wt.Dir, "git", "add", "feature.txt")
	runCmd(t, wt.Dir, "git", "commit", "-m", "add feature")

	// Verify isolation: main repo must remain clean and on initial commit
	parentHead := runCmd(t, repo, "git", "rev-parse", "HEAD")
	if parentHead != headCommit {
		t.Errorf("parent HEAD changed prematurely; got %s, want %s", parentHead, headCommit)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); !os.IsNotExist(err) {
		t.Errorf("feature.txt leaked into parent repo before integration")
	}
	parentClean, err := isDirty(ctx, repo)
	if err != nil || parentClean {
		t.Errorf("parent repo should be clean; dirty=%v, err=%v", parentClean, err)
	}
}

// TestDirtyParentDisclosure verifies the three cases of dirty parent handling:
//  1. Rejection when neither disclosure nor snapshot authorization is provided.
//  2. Acceptance with explicit disclosure (DiscloseDirtyAbsent=true), confirming
//     dirty changes are absent from child checkout.
//  3. Acceptance with snapshot authorization (AuthorizeSnapshot=true), confirming
//     snapshot commit is created and checked out without modifying parent working state.
func TestDirtyParentDisclosure(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Make parent repo dirty
	readmePath := filepath.Join(repo, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Initial commit\nUncommitted dirty parent change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Without disclosure or snapshot authorization -> must fail
	optsNoDisclosure := AllocateOptions{
		Run:         "run-dirty-1",
		Agent:       "root/1",
		Branch:      "branch-no-disclosure",
		WorktreeDir: filepath.Join(t.TempDir(), "wt-no-disclosure"),
	}
	_, err := Allocate(ctx, repo, optsNoDisclosure)
	if !errors.Is(err, ErrDirtyParentNotDisclosed) {
		t.Fatalf("expected ErrDirtyParentNotDisclosed, got: %v", err)
	}

	// 2. With disclosure (DiscloseDirtyAbsent=true) -> clean committed base, dirty changes absent
	optsDisclosed := AllocateOptions{
		Run:                 "run-dirty-2",
		Agent:               "root/2",
		Branch:              "branch-disclosed",
		WorktreeDir:         filepath.Join(t.TempDir(), "wt-disclosed"),
		DiscloseDirtyAbsent: true,
	}
	wtDisclosed, err := Allocate(ctx, repo, optsDisclosed)
	if err != nil {
		t.Fatalf("allocate with disclosure failed: %v", err)
	}
	if wtDisclosed.Meta.DirtyBasePolicy != PolicyDirtyDisclosedAbsent {
		t.Errorf("expected PolicyDirtyDisclosedAbsent, got %s", wtDisclosed.Meta.DirtyBasePolicy)
	}

	// Verify parent dirty changes are absent in the child worktree
	wtReadme, err := os.ReadFile(filepath.Join(wtDisclosed.Dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(wtReadme) != "# Initial commit\n" {
		t.Errorf("parent dirty changes leaked into child worktree: got %q", string(wtReadme))
	}

	// 3. With snapshot authorization (AuthorizeSnapshot=true) -> snapshot commit checked out
	optsSnapshot := AllocateOptions{
		Run:               "run-dirty-3",
		Agent:             "root/3",
		Branch:            "branch-snapshot",
		WorktreeDir:       filepath.Join(t.TempDir(), "wt-snapshot"),
		AuthorizeSnapshot: true,
	}
	wtSnapshot, err := Allocate(ctx, repo, optsSnapshot)
	if err != nil {
		t.Fatalf("allocate with snapshot failed: %v", err)
	}
	if wtSnapshot.Meta.DirtyBasePolicy != PolicySnapshotAuthorized {
		t.Errorf("expected PolicySnapshotAuthorized, got %s", wtSnapshot.Meta.DirtyBasePolicy)
	}
	// Verify snapshot contains parent's dirty state
	snapReadme, err := os.ReadFile(filepath.Join(wtSnapshot.Dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(snapReadme) != "# Initial commit\nUncommitted dirty parent change\n" {
		t.Errorf("snapshot did not capture parent dirty state: got %q", string(snapReadme))
	}
	// Parent repo must still have its dirty changes intact
	parentReadme, _ := os.ReadFile(readmePath)
	if string(parentReadme) != "# Initial commit\nUncommitted dirty parent change\n" {
		t.Errorf("parent dirty changes were modified or lost: got %q", string(parentReadme))
	}
}

// TestPreserveFailure verifies that when an agent task fails or is cancelled,
// unfinished work is preserved on disk, not deleted or pruned, and can be recovered.
func TestPreserveFailure(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-fail")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-fail",
		Agent:       "root/1",
		Branch:      "feature-fail",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// Commit partial work and create uncommitted work
	if err := os.WriteFile(filepath.Join(wtDir, "partial.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wtDir, "git", "add", "partial.go")
	runCmd(t, wtDir, "git", "commit", "-m", "partial commit")

	if err := os.WriteFile(filepath.Join(wtDir, "unfinished.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mark failed
	if err := wt.MarkFailed("build failed: syntax error"); err != nil {
		t.Fatalf("mark failed error: %v", err)
	}

	// Verify files still exist on disk
	if _, err := os.Stat(filepath.Join(wtDir, "partial.go")); os.IsNotExist(err) {
		t.Errorf("partial.go was deleted on failure")
	}
	if _, err := os.Stat(filepath.Join(wtDir, "unfinished.txt")); os.IsNotExist(err) {
		t.Errorf("unfinished.txt was deleted on failure")
	}

	// Verify Recover inspects preserved state accurately
	rep, err := Recover(ctx, repo, wtDir)
	if err != nil {
		t.Fatalf("recover error: %v", err)
	}
	if !rep.WorktreeExists {
		t.Errorf("expected WorktreeExists=true")
	}
	if rep.Meta.Lifecycle != LifecycleFailed {
		t.Errorf("expected LifecycleFailed, got %s", rep.Meta.Lifecycle)
	}
	if rep.IsClean {
		t.Errorf("expected IsClean=false due to unfinished.txt")
	}
	if len(rep.CommitsAhead) != 1 {
		t.Errorf("expected 1 commit ahead, got %d", len(rep.CommitsAhead))
	}
}

// TestActiveWriterRefusal verifies that reuse, handoff, and cleanup are refused
// while a writer is active or has surviving processes.
func TestActiveWriterRefusal(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-active")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-active",
		Agent:       "root/1",
		Branch:      "feature-active",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// 1. Attempting to allocate same directory or branch must fail
	_, err = Allocate(ctx, repo, AllocateOptions{
		Run:         "run-another",
		Agent:       "root/2",
		Branch:      "feature-active",
		WorktreeDir: filepath.Join(t.TempDir(), "wt-another"),
	})
	if !errors.Is(err, ErrWorktreeExists) {
		t.Errorf("expected ErrWorktreeExists for duplicate branch, got: %v", err)
	}

	// 2. Cleanup while LifecycleActive must be refused
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{})
	if !errors.Is(err, ErrActiveWriter) {
		t.Errorf("expected ErrActiveWriter on active cleanup, got: %v", err)
	}

	// 3. Cleanup while a surviving process is tracked must be refused
	// Start a background sleep process
	sleepCmd := exec.Command("sleep", "30")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleepCmd.Process.Kill()
		_ = sleepCmd.Wait()
	}()

	_ = wt.TrackPID(sleepCmd.Process.Pid)
	_ = wt.MarkCompleted("done") // lifecycle is completed, but process is alive

	err = Cleanup(ctx, repo, wtDir, CleanupOptions{})
	if !errors.Is(err, ErrActiveProcesses) {
		t.Errorf("expected ErrActiveProcesses for surviving PID, got: %v", err)
	}

	// Kill process
	_ = sleepCmd.Process.Kill()
	_ = sleepCmd.Wait()

	// 4. Now cleanup should succeed
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{DeleteBranch: true})
	if err != nil {
		t.Errorf("cleanup failed after process terminated: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree directory still exists after cleanup")
	}
}

// TestChangedDestinationRejection verifies that integration rejects when the target
// branch commit has changed (does not match ExpectedTargetCommit) or destination is dirty.
func TestChangedDestinationRejection(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	c1 := runCmd(t, repo, "git", "rev-parse", "HEAD")
	c1 = c1[:len(c1)-1] // trim newline

	wtDir := filepath.Join(t.TempDir(), "wt-dest")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-dest",
		Agent:       "root/1",
		Branch:      "feature-dest",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// Make a commit in the worktree
	if err := os.WriteFile(filepath.Join(wtDir, "file_a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wtDir, "git", "add", "file_a.txt")
	runCmd(t, wtDir, "git", "commit", "-m", "worktree commit")

	// Advance master in repo with an independent commit C2
	if err := os.WriteFile(filepath.Join(repo, "master_file.txt"), []byte("M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, "git", "add", "master_file.txt")
	runCmd(t, repo, "git", "commit", "-m", "master advanced")
	c2 := runCmd(t, repo, "git", "rev-parse", "HEAD")
	c2 = c2[:len(c2)-1]

	// 1. ExpectedTargetCommit is C1, but target is now at C2 -> Rejection
	_, err = Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:          wt.Dir,
		TargetBranch:         "master",
		ExpectedTargetCommit: c1,
	})
	if !errors.Is(err, ErrTargetCommitMismatch) {
		t.Fatalf("expected ErrTargetCommitMismatch, got: %v", err)
	}

	// 2. Target working tree is dirty -> Rejection
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:          wt.Dir,
		TargetBranch:         "master",
		ExpectedTargetCommit: c2,
	})
	if !errors.Is(err, ErrTargetDirty) {
		t.Fatalf("expected ErrTargetDirty, got: %v", err)
	}

	// Clean dirty file
	_ = os.Remove(filepath.Join(repo, "dirty.txt"))

	if err := wt.MarkCompleted("ready for integration"); err != nil {
		t.Fatal(err)
	}
	// 3. With correct ExpectedTargetCommit and clean destination -> Succeeds
	res, err := Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:          wt.Dir,
		TargetBranch:         "master",
		ExpectedTargetCommit: c2,
		Strategy:             StrategyMerge,
	})
	if err != nil {
		t.Fatalf("expected integration to succeed, got: %v", err)
	}
	if res.NewCommit == "" || res.NewCommit == c2 {
		t.Errorf("expected new commit after merge, got %s", res.NewCommit)
	}
}

// TestLockSerialization verifies that concurrent operations on the shared Git common
// directory are safely serialized via flock with CLOEXEC, preventing race conditions.
func TestLockSerialization(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	gitCommonDir, err := FindGitCommonDir(ctx, repo)
	if err != nil {
		t.Fatalf("find git common dir: %v", err)
	}

	var activeHolders int32
	var maxConcurrent int32
	var wg sync.WaitGroup

	iterations := 15
	wg.Add(iterations)

	for i := 0; i < iterations; i++ {
		go func() {
			defer wg.Done()
			err := WithRepoLock(ctx, gitCommonDir, func() error {
				current := atomic.AddInt32(&activeHolders, 1)
				for {
					oldMax := atomic.LoadInt32(&maxConcurrent)
					if current <= oldMax || atomic.CompareAndSwapInt32(&maxConcurrent, oldMax, current) {
						break
					}
				}

				// Simulate brief critical section
				time.Sleep(15 * time.Millisecond)

				atomic.AddInt32(&activeHolders, -1)
				return nil
			})
			if err != nil {
				t.Errorf("lock error: %v", err)
			}
		}()
	}

	wg.Wait()

	if maxConcurrent != 1 {
		t.Fatalf("mutual exclusion violated: max concurrent lock holders was %d, want 1", maxConcurrent)
	}
}

// TestIntegrateMergeConflict stops immediately with ErrConflict and performs no auto-merge.
func TestIntegrateMergeConflict(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-conflict")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-conf",
		Agent:       "root/1",
		Branch:      "feature-conf",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Change README.md in worktree
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("Worktree version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wtDir, "git", "add", "README.md")
	runCmd(t, wtDir, "git", "commit", "-m", "worktree edit README")

	// Change README.md in master conflictingly
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("Master conflicting version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, "git", "add", "README.md")
	runCmd(t, repo, "git", "commit", "-m", "master edit README")

	if err := wt.MarkCompleted("ready for integration"); err != nil {
		t.Fatal(err)
	}
	// Attempt integrate -> must conflict and stop without auto-merge
	_, err = Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:  wt.Dir,
		TargetBranch: "master",
		Strategy:     StrategyMerge,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}

	var confErr *ConflictError
	if !errors.As(err, &confErr) {
		t.Fatalf("expected error to be *ConflictError, got: %T", err)
	}
	if len(confErr.ConflictedFiles) == 0 || confErr.ConflictedFiles[0] != "README.md" {
		t.Errorf("expected README.md in conflicted files, got %v", confErr.ConflictedFiles)
	}

	// Verify master is restored clean (aborted, no conflict markers left)
	dirty, err := isDirty(ctx, repo)
	if err != nil || dirty {
		t.Errorf("repo should be clean after aborted conflict; dirty=%v, err=%v", dirty, err)
	}
}

// TestIntegrateCherryPick verifies integration via cherry-pick strategy.
func TestIntegrateCherryPick(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-cp")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-cp",
		Agent:       "root/1",
		Branch:      "feature-cp",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wtDir, "cp_file.txt"), []byte("cherry-pick content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wtDir, "git", "add", "cp_file.txt")
	runCmd(t, wtDir, "git", "commit", "-m", "add cp file")
	if err := wt.MarkCompleted("ready for integration"); err != nil {
		t.Fatal(err)
	}

	res, err := Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:  wt.Dir,
		TargetBranch: "master",
		Strategy:     StrategyCherryPick,
	})
	if err != nil {
		t.Fatalf("cherry-pick integration failed: %v", err)
	}
	if res.Strategy != StrategyCherryPick {
		t.Errorf("expected StrategyCherryPick, got %s", res.Strategy)
	}

	if _, err := os.Stat(filepath.Join(repo, "cp_file.txt")); os.IsNotExist(err) {
		t.Errorf("cp_file.txt not found in master after cherry-pick")
	}
}

// TestCleanupWithPermissionGate verifies that permission gates are respected on cleanup.
func TestCleanupWithPermissionGate(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-perm")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-perm",
		Agent:       "root/1",
		Branch:      "feature-perm",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = wt.MarkCompleted("done")

	// Gate denies
	denyGate := PermissionFunc(func(ctx context.Context, action, target string) error {
		if action == "cleanup" {
			return errors.New("cleanup denied by security policy")
		}
		return nil
	})

	err = Cleanup(ctx, repo, wtDir, CleanupOptions{PermGate: denyGate})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got: %v", err)
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("worktree was deleted despite permission gate denial")
	}

	// Gate allows
	allowGate := PermissionFunc(func(ctx context.Context, action, target string) error {
		return nil
	})
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{PermGate: allowGate, DeleteBranch: true})
	if err != nil {
		t.Fatalf("cleanup failed with allow gate: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after allowed cleanup")
	}
}

// TestSessionRecordConversion verifies metadata mapping to and from session records.
func TestSessionRecordConversion(t *testing.T) {
	meta := Metadata{
		Repo:            "/home/user/src/repo",
		WorktreeDir:     "/tmp/wt-123",
		BaseCommit:      "abc1234",
		Branch:          "feature-test",
		Run:             "run-999",
		Agent:           "root/2",
		Task:            "test task",
		Lifecycle:       LifecycleActive,
		DirtyBasePolicy: PolicyClean,
		CreatedAt:       "2026-10-09T22:00:00Z",
		UpdatedAt:       "2026-10-09T22:05:00Z",
	}

	rec := meta.ToSessionRecord()
	if rec.Type != "worktree" {
		t.Errorf("expected type worktree, got %s", rec.Type)
	}
	if rec.Run != "run-999" || rec.Agent != "root/2" {
		t.Errorf("unexpected run/agent: %s/%s", rec.Run, rec.Agent)
	}
	if rec.Cwd != "/tmp/wt-123" {
		t.Errorf("unexpected cwd: %s", rec.Cwd)
	}
	if rec.Status != string(LifecycleActive) {
		t.Errorf("unexpected status: %s", rec.Status)
	}

	reconstructed, ok := FromSessionRecord(rec)
	if !ok {
		t.Fatalf("expected FromSessionRecord to succeed")
	}
	if reconstructed.Run != meta.Run || reconstructed.Agent != meta.Agent || reconstructed.WorktreeDir != meta.WorktreeDir {
		t.Errorf("reconstructed metadata mismatch: %+v", reconstructed)
	}

	// Non-worktree record
	_, ok = FromSessionRecord(session.Record{Type: "message"})
	if ok {
		t.Errorf("expected FromSessionRecord to return false for non-worktree record")
	}
}

// TestListWorktrees verifies listing managed worktrees.
func TestListWorktrees(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wt1, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-l1",
		Agent:       "root/1",
		Branch:      "branch-l1",
		WorktreeDir: filepath.Join(t.TempDir(), "wt-l1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Cleanup(ctx, repo, wt1.Dir, CleanupOptions{Force: true, DeleteBranch: true}) }()

	wt2, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-l2",
		Agent:       "root/2",
		Branch:      "branch-l2",
		WorktreeDir: filepath.Join(t.TempDir(), "wt-l2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Cleanup(ctx, repo, wt2.Dir, CleanupOptions{Force: true, DeleteBranch: true}) }()

	list, err := List(ctx, repo)
	if err != nil {
		t.Fatalf("list error: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("expected 2 worktrees, got %d", len(list))
	}
}

// TestLockContextCancellation verifies that waiting on a locked repo respects context cancellation.
func TestLockContextCancellation(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	gitCommonDir, err := FindGitCommonDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	// Acquire lock first
	lock1, err := LockRepo(ctx, gitCommonDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock1.Unlock() }()

	// Try acquiring lock with cancelled context
	cancellableCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()

	_, err = LockRepo(cancellableCtx, gitCommonDir)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation/deadline error, got %v", err)
	}
}

// TestAllocateValidationAndGate verifies validation rules and permission gate for allocation.
func TestAllocateValidationAndGate(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	// Missing Run/Agent
	_, err := Allocate(ctx, repo, AllocateOptions{
		Agent: "root/1",
	})
	if err == nil {
		t.Errorf("expected error for missing run")
	}

	_, err = Allocate(ctx, repo, AllocateOptions{
		Run: "run-1",
	})
	if err == nil {
		t.Errorf("expected error for missing agent")
	}

	// PermGate denial
	denyGate := PermissionFunc(func(ctx context.Context, action, target string) error {
		return errors.New("allocate denied")
	})
	_, err = Allocate(ctx, repo, AllocateOptions{
		Run:      "run-1",
		Agent:    "root/1",
		PermGate: denyGate,
	})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}
}

// TestIntegrateWorktreeDirty verifies that a worktree with uncommitted changes cannot be integrated.
func TestIntegrateWorktreeDirty(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-dirty")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-dirty",
		Agent:       "root/1",
		Branch:      "branch-dirty",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Make uncommitted change in worktree
	if err := os.WriteFile(filepath.Join(wtDir, "uncommitted.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:  wt.Dir,
		TargetBranch: "master",
	})
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("expected ErrWorktreeDirty, got %v", err)
	}
}

// TestIntegratePermGateDenial verifies that integration is refused when permission gate denies.
func TestIntegratePermGateDenial(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-gate-deny")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-gate",
		Agent:       "root/1",
		Branch:      "branch-gate",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Commit a change in worktree
	if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wtDir, "git", "add", "file.txt")
	runCmd(t, wtDir, "git", "commit", "-m", "worktree commit")
	if err := wt.MarkCompleted("ready for integration"); err != nil {
		t.Fatal(err)
	}

	denyGate := PermissionFunc(func(ctx context.Context, action, target string) error {
		if action == "integrate" {
			return errors.New("integration blocked by security policy")
		}
		return nil
	})

	_, err = Integrate(ctx, repo, IntegrateOptions{
		WorktreeDir:  wt.Dir,
		TargetBranch: "master",
		PermGate:     denyGate,
	})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}

	// Target branch must remain without file.txt
	if _, err := os.Stat(filepath.Join(repo, "file.txt")); !os.IsNotExist(err) {
		t.Errorf("file.txt leaked into master despite gate denial")
	}
}

type mockPermGate struct {
	outcome string
	reason  string
}

func (m *mockPermGate) Check(ctx context.Context, c perm.Call) perm.Outcome {
	return perm.Outcome{Action: m.outcome, Reason: m.reason}
}

// TestFromPermGateAdapter tests adapting internal/perm.Gate to PermissionGate.
func TestFromPermGateAdapter(t *testing.T) {
	ctx := context.Background()

	// 1. Allowed gate
	allowGate := &mockPermGate{outcome: perm.Allow}
	pgAllow := FromPermGate(allowGate, "root/1", "/tmp/cwd")
	if err := pgAllow.CheckPermission(ctx, "test_action", "/tmp/target"); err != nil {
		t.Errorf("expected nil error for allow gate, got %v", err)
	}

	// 2. Denied gate
	denyGate := &mockPermGate{outcome: perm.Deny, reason: "forbidden by user"}
	pgDeny := FromPermGate(denyGate, "root/1", "/tmp/cwd")
	err := pgDeny.CheckPermission(ctx, "test_action", "/tmp/target")
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}

	// 3. Nil gate
	if FromPermGate(nil, "root/1", "/tmp/cwd") != nil {
		t.Errorf("expected nil PermissionGate for nil perm.Gate")
	}
}

// TestRecoverAndListWithOptions verifies permission gate support in List and Recover.
func TestRecoverAndListWithOptions(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-opts")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-opts",
		Agent:       "root/1",
		Branch:      "branch-opts",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Cleanup(ctx, repo, wt.Dir, CleanupOptions{Force: true, DeleteBranch: true}) }()

	denyGate := PermissionFunc(func(ctx context.Context, action, target string) error {
		return errors.New("denied")
	})

	// ListWithOptions denial
	_, err = ListWithOptions(ctx, repo, ListOptions{PermGate: denyGate})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("expected ErrPermissionDenied on ListWithOptions, got %v", err)
	}

	// RecoverWithOptions denial
	_, err = RecoverWithOptions(ctx, repo, wtDir, RecoverOptions{PermGate: denyGate})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("expected ErrPermissionDenied on RecoverWithOptions, got %v", err)
	}

	// Recover non-existent dir
	rep, err := Recover(ctx, repo, filepath.Join(t.TempDir(), "non-existent"))
	if err != nil {
		t.Fatalf("unexpected error recovering non-existent dir: %v", err)
	}
	if rep.WorktreeExists {
		t.Errorf("expected WorktreeExists=false for non-existent path")
	}
}

// TestStaticGitClassifier tests integration with the concurrently added exported static Git classifier.
func TestStaticGitClassifier(t *testing.T) {
	classifier := StaticGitClassifier{Cwd: "/test/repo"}

	// Shared ref mutating commands
	mutatingCmds := []string{
		"git push origin master",
		"git fetch origin",
		"git branch -D feature",
		"git branch new-branch",
		"git worktree add /tmp/wt master",
		"git worktree remove /tmp/wt",
		"git commit -m 'test'",
		"git merge feature",
	}
	for _, cmd := range mutatingCmds {
		if !classifier.IsSharedRefMutation(cmd) {
			t.Errorf("expected %q to be classified as shared ref / mutating", cmd)
		}
	}

	// Read-only commands
	readOnlyCmds := []string{
		"git status",
		"git diff",
		"git log --oneline",
		"git branch --show-current",
		"git branch --list",
		"git worktree list",
	}
	for _, cmd := range readOnlyCmds {
		if classifier.IsSharedRefMutation(cmd) {
			t.Errorf("expected %q to be classified as read-only, not shared ref mutation", cmd)
		}
	}
}

// TestWorktreeSessionRecordPersistence verifies that metadata is persisted as append-only session records
// with mode 0600, rather than an authoritative private Git JSON database, and can be loaded from session file.
func TestWorktreeSessionRecordPersistence(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	var callbackRecords []session.Record
	opts := AllocateOptions{
		Run:         "run-session-test",
		Agent:       "root/1",
		Task:        "test session persistence",
		Branch:      "branch-session",
		WorktreeDir: filepath.Join(t.TempDir(), "wt-session"),
		SessionCallback: func(r session.Record) error {
			callbackRecords = append(callbackRecords, r)
			return nil
		},
	}

	wt, err := Allocate(ctx, repo, opts)
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	sessionPath := wt.SessionFilePath()
	fi, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("session file not found: %v", err)
	}
	// Verify file mode is 0600
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("expected session file perm 0600, got %o", fi.Mode().Perm())
	}

	// Update metadata and verify append
	if err := wt.MarkCompleted("finished task"); err != nil {
		t.Fatalf("mark completed failed: %v", err)
	}

	if len(callbackRecords) < 2 {
		t.Fatalf("expected at least 2 callback records, got %d", len(callbackRecords))
	}

	// Load via Load and LoadFromSessionFile
	loaded, err := Load(wt.Dir)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.Meta.Lifecycle != LifecycleCompleted || loaded.Meta.Result != "finished task" {
		t.Errorf("unexpected loaded state: %+v", loaded.Meta)
	}
	if loaded.Meta.Run != "run-session-test" || loaded.Meta.Agent != "root/1" {
		t.Errorf("unexpected loaded owner: %s/%s", loaded.Meta.Run, loaded.Meta.Agent)
	}

	loadedDirect, err := LoadFromSessionFile(sessionPath)
	if err != nil {
		t.Fatalf("load from session file failed: %v", err)
	}
	if loadedDirect.Meta.Lifecycle != LifecycleCompleted {
		t.Errorf("unexpected loadedDirect state: %+v", loadedDirect.Meta)
	}
}

// TestPIDStartIdentityRecycling verifies that process start identity prevents recycled PIDs
// from being mistaken for surviving tracked processes.
func TestPIDStartIdentityRecycling(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-recycle")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-recycle",
		Agent:       "root/1",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// Start a real process to inspect its PID and start time
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	realStartTime, err := getProcessStartTime(pid)
	if err != nil {
		t.Fatalf("getProcessStartTime failed: %v", err)
	}

	// Track process
	if err := wt.TrackPID(pid); err != nil {
		t.Fatalf("track pid failed: %v", err)
	}
	if wt.Meta.PIDStartTimes[pid] != realStartTime {
		t.Errorf("expected start time %d, got %d", realStartTime, wt.Meta.PIDStartTimes[pid])
	}

	// Verify it is recognized as alive with correct identity
	if !isPIDAliveWithIdentity(pid, realStartTime) {
		t.Errorf("expected process to be alive with correct start time")
	}

	// Verify that with a mismatched (stale) start time, it is NOT considered alive
	if isPIDAliveWithIdentity(pid, realStartTime+99999) {
		t.Errorf("expected process to be considered dead when start time differs (recycled PID)")
	}
}

// TestFailClosedSurvivingProcessInspection verifies that if process inspection fails,
// worktree operations fail closed to prevent destroying files of active processes.
func TestFailClosedSurvivingProcessInspection(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-failclose")
	wt, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-failclose",
		Agent:       "root/1",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// Mock procReadDir to return an error simulating unreadable /proc
	origReadDir := procReadDir
	defer func() { procReadDir = origReadDir }()

	procReadDir = func(name string) ([]os.DirEntry, error) {
		return nil, errors.New("simulated /proc permission denied")
	}

	// 1. DetectActiveProcesses must return error
	_, err = DetectActiveProcesses(wtDir, nil)
	if err == nil {
		t.Errorf("expected error from DetectActiveProcesses when /proc unreadable")
	}

	// 2. HasActiveExecution must fail closed (return true)
	if !wt.HasActiveExecution() {
		t.Errorf("expected HasActiveExecution=true on inspection failure (fail closed)")
	}

	// 3. Cleanup must fail closed without deleting worktree
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{Force: true})
	if err == nil {
		t.Errorf("expected Cleanup to fail when process inspection fails")
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("worktree was deleted despite process inspection failure")
	}

	// 4. Recover must fail closed
	rep, err := Recover(ctx, repo, wtDir)
	if err == nil {
		t.Errorf("expected Recover to return error on inspection failure")
	}
	if rep.CanCleanup || rep.CanIntegrate {
		t.Errorf("expected CanCleanup=false and CanIntegrate=false on inspection failure")
	}
}

// TestRecoverCrashedWriterPreservesDirtyCheckout verifies that crashed writers can be recovered
// without deleting or altering dirty uncommitted files or commits ahead on disk.
func TestRecoverCrashedWriterPreservesDirtyCheckout(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-crashed")
	_, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-crashed",
		Agent:       "root/1",
		Branch:      "branch-crashed",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// Commit some work
	committedFile := filepath.Join(wtDir, "committed.txt")
	if err := os.WriteFile(committedFile, []byte("committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, wtDir, "git", "add", "committed.txt")
	runCmd(t, wtDir, "git", "commit", "-m", "work in progress")

	// Create uncommitted dirty changes
	dirtyFile := filepath.Join(wtDir, "dirty.txt")
	if err := os.WriteFile(dirtyFile, []byte("uncommitted dirty content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate writer process crash (writer died while LifecycleActive, no live PIDs)
	// Attempt normal cleanup without Force should fail because of active writer
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{})
	if !errors.Is(err, ErrActiveWriter) {
		t.Errorf("expected ErrActiveWriter before recovery, got: %v", err)
	}

	// Recover crashed writer
	recovered, err := RecoverCrashedWriter(ctx, repo, wtDir, "OOM killed")
	if err != nil {
		t.Fatalf("recover crashed writer failed: %v", err)
	}
	if recovered.Meta.Lifecycle != LifecycleFailed {
		t.Errorf("expected LifecycleFailed, got %s", recovered.Meta.Lifecycle)
	}

	// Verify dirty changes and committed files are PRESERVED on disk
	if data, err := os.ReadFile(dirtyFile); err != nil || string(data) != "uncommitted dirty content\n" {
		t.Errorf("dirty file was lost or corrupted during recovery: %v", err)
	}
	if data, err := os.ReadFile(committedFile); err != nil || string(data) != "committed work\n" {
		t.Errorf("committed file was lost or corrupted during recovery: %v", err)
	}

	// Cleanup without Force must STILL be rejected because dirty files exist
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{})
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("expected ErrWorktreeDirty when cleaning dirty worktree without Force, got: %v", err)
	}

	// Now cleanup with Force succeeds
	err = Cleanup(ctx, repo, wtDir, CleanupOptions{Force: true, DeleteBranch: true})
	if err != nil {
		t.Fatalf("forced cleanup failed: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after forced cleanup")
	}
}

// TestNoLiveHandoff verifies that an active worktree cannot be reused or handed off.
func TestNoLiveHandoff(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt-nohandoff")
	_, err := Allocate(ctx, repo, AllocateOptions{
		Run:         "run-orig",
		Agent:       "root/1",
		WorktreeDir: wtDir,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	// Start a background process and track it
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	wt, err := Load(wtDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = wt.TrackPID(cmd.Process.Pid)

	// Attempting to recover a live running worktree must return ErrNoHandoff
	_, err = RecoverCrashedWriter(ctx, repo, wtDir, "attempted takeover")
	if !errors.Is(err, ErrNoHandoff) {
		t.Errorf("expected ErrNoHandoff on recovering live worktree, got: %v", err)
	}

	// Attempting to allocate same directory must return ErrNoHandoff
	_, err = Allocate(ctx, repo, AllocateOptions{
		Run:         "run-new",
		Agent:       "root/2",
		WorktreeDir: wtDir,
	})
	if !errors.Is(err, ErrNoHandoff) {
		t.Errorf("expected ErrNoHandoff on allocating over active worktree, got: %v", err)
	}
}
