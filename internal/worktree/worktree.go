package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unix/ugh/internal/session"
)

// Worktree represents a harness-managed Git worktree and its metadata.
type Worktree struct {
	Meta            Metadata                   `json:"meta"`
	RepoRoot        string                     `json:"repo_root"`
	Dir             string                     `json:"dir"`
	SessionStore    *session.Store             `json:"-"`
	SessionCallback func(session.Record) error `json:"-"`
	SessionFile     string                     `json:"session_file,omitempty"`
}

// findWorktreeGitDir resolves the private Git directory for a worktree.
func findWorktreeGitDir(worktreeDir string) (string, error) {
	dotGit := filepath.Join(worktreeDir, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return dotGit, nil
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, "gitdir:") {
		target := strings.TrimSpace(strings.TrimPrefix(text, "gitdir:"))
		if !filepath.IsAbs(target) {
			target = filepath.Clean(filepath.Join(worktreeDir, target))
		}
		return target, nil
	}
	return "", fmt.Errorf("invalid .git in %s", worktreeDir)
}

// SessionFilePath returns the session file path (.jsonl) for this worktree.
func (w *Worktree) SessionFilePath() string {
	if w.SessionFile != "" {
		return w.SessionFile
	}
	if w.Dir != "" {
		return filepath.Join(w.Dir, ".ugh-session.jsonl")
	}
	return ""
}

// SaveMeta persists the worktree's metadata to session records.
// It appends to the session file (mode 0600, fsync) and any registered Store or callback,
// avoiding any authoritative private Git JSON ownership database.
func (w *Worktree) SaveMeta() error {
	if w.Dir == "" {
		return fmt.Errorf("cannot save metadata: worktree directory not set")
	}
	w.Meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec := w.Meta.ToSessionRecord()

	if w.SessionCallback != nil {
		if err := w.SessionCallback(rec); err != nil {
			return fmt.Errorf("session callback: %w", err)
		}
	}
	if w.SessionStore != nil {
		if err := w.SessionStore.Append(rec); err != nil {
			return fmt.Errorf("session store append: %w", err)
		}
	}

	p := w.SessionFilePath()
	if p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("marshal session record: %w", err)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("open session file %s: %w", p, err)
		}
		_ = f.Chmod(0o600)
		if _, err := f.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			return fmt.Errorf("write session file: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("sync session file: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close session file: %w", err)
		}
	}
	return nil
}

// MarkActive marks the worktree as actively executing.
func (w *Worktree) MarkActive() error {
	w.Meta.Lifecycle = LifecycleActive
	return w.SaveMeta()
}

// MarkCompleted marks the worktree as successfully completed.
func (w *Worktree) MarkCompleted(result string) error {
	w.Meta.Lifecycle = LifecycleCompleted
	w.Meta.Result = result
	return w.SaveMeta()
}

// MarkFailed marks the worktree as failed, preserving work on disk.
func (w *Worktree) MarkFailed(errMsg string) error {
	w.Meta.Lifecycle = LifecycleFailed
	w.Meta.Result = errMsg
	return w.SaveMeta()
}

// MarkCancelled marks the worktree as cancelled, preserving work on disk.
func (w *Worktree) MarkCancelled(reason string) error {
	w.Meta.Lifecycle = LifecycleCancelled
	w.Meta.Result = reason
	return w.SaveMeta()
}

// TrackPID registers a process ID with this worktree for liveness tracking with start time identity.
func (w *Worktree) TrackPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if w.Meta.PIDStartTimes == nil {
		w.Meta.PIDStartTimes = make(map[int]uint64)
	}
	st, _ := getProcessStartTime(pid)
	if st > 0 {
		w.Meta.PIDStartTimes[pid] = st
	}
	for _, p := range w.Meta.ActivePIDs {
		if p == pid {
			return w.SaveMeta()
		}
	}
	w.Meta.ActivePIDs = append(w.Meta.ActivePIDs, pid)
	return w.SaveMeta()
}

// ActiveProcesses returns a list of live PIDs associated with this worktree.
func (w *Worktree) ActiveProcesses() []int {
	procs, _ := DetectActiveProcesses(w.Dir, w.Meta.ActivePIDs, w.Meta.PIDStartTimes)
	return procs
}

// HasActiveExecution reports whether the worktree is active or has surviving processes.
// Fails closed (returning true) if surviving processes cannot be inspected.
func (w *Worktree) HasActiveExecution() bool {
	if w.Meta.Lifecycle == LifecycleActive || w.Meta.Lifecycle == LifecycleAllocated {
		return true
	}
	procs, err := DetectActiveProcesses(w.Dir, w.Meta.ActivePIDs, w.Meta.PIDStartTimes)
	if err != nil {
		return true // fail closed
	}
	return len(procs) > 0
}

// Status inspects the worktree and returns a diagnostic recovery report.
func (w *Worktree) Status(ctx context.Context) (*RecoveryReport, error) {
	return Recover(ctx, w.RepoRoot, w.Dir)
}

// sanitize replaces characters unsuitable for git refnames and paths.
func sanitize(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ":", "_")
	return r.Replace(s)
}

// LoadFromSessionFile loads a Worktree from a session JSONL file.
func LoadFromSessionFile(path string) (*Worktree, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read session file %s: %v", ErrWorktreeNotFound, path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var latestRecord *session.Record
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r session.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Type == "worktree" {
			latestRecord = &r
		}
	}
	if latestRecord == nil {
		return nil, fmt.Errorf("%w: no worktree session record in %s", ErrWorktreeNotFound, path)
	}
	meta, ok := FromSessionRecord(*latestRecord)
	if !ok || meta == nil {
		return nil, fmt.Errorf("%w: invalid worktree record in %s", ErrWorktreeNotFound, path)
	}
	return &Worktree{
		Meta:        *meta,
		RepoRoot:    meta.Repo,
		Dir:         meta.WorktreeDir,
		SessionFile: path,
	}, nil
}

// Load loads a Worktree from an existing worktree directory.
// It loads from the session metadata file (.ugh-session.jsonl) first, falling back to legacy JSON files if present.
func Load(worktreeDir string) (*Worktree, error) {
	worktreeDir = filepath.Clean(worktreeDir)
	sessionFile := filepath.Join(worktreeDir, ".ugh-session.jsonl")
	if _, err := os.Stat(sessionFile); err == nil {
		return LoadFromSessionFile(sessionFile)
	}

	// Fallback for legacy .ugh-worktree.json files
	var b []byte
	var err error
	if gitDir, gErr := findWorktreeGitDir(worktreeDir); gErr == nil {
		b, err = os.ReadFile(filepath.Join(gitDir, "ugh-worktree.json"))
	}
	if err != nil || len(b) == 0 {
		metaPath := filepath.Join(worktreeDir, ".ugh-worktree.json")
		b, err = os.ReadFile(metaPath)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s (%v)", ErrWorktreeNotFound, worktreeDir, err)
	}
	var meta Metadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal worktree metadata: %w", err)
	}
	return &Worktree{
		Meta:        meta,
		RepoRoot:    meta.Repo,
		Dir:         worktreeDir,
		SessionFile: sessionFile,
	}, nil
}

// Allocate allocates a fresh isolated worktree on a separate feature branch and directory.
// It verifies base cleanliness: if parent is dirty, allocation is rejected unless the caller
// explicitly discloses dirty changes absent (DiscloseDirtyAbsent=true) or authorizes a snapshot (AuthorizeSnapshot=true).
// All shared Git repository operations are serialized via flock on the shared Git common directory with O_CLOEXEC.
func Allocate(ctx context.Context, repoRoot string, opts AllocateOptions) (*Worktree, error) {
	if opts.Run == "" || opts.Agent == "" {
		return nil, fmt.Errorf("allocate requires run and agent identity")
	}

	repoRoot = filepath.Clean(repoRoot)
	gitCommonDir, err := FindGitCommonDir(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("find git common dir: %w", err)
	}

	lock, err := LockRepo(ctx, gitCommonDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()

	// Check permission gate if provided
	if opts.PermGate != nil {
		if err := opts.PermGate.CheckPermission(ctx, "allocate", repoRoot); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}

	// Determine cleanliness of parent repository
	parentDirty, err := isDirty(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("check parent repo status: %w", err)
	}

	var baseCommit string
	var policy DirtyBasePolicy
	var validation string

	if parentDirty {
		if !opts.DiscloseDirtyAbsent && !opts.AuthorizeSnapshot {
			return nil, ErrDirtyParentNotDisclosed
		}
		if opts.AuthorizeSnapshot {
			snap, snapErr := createStashSnapshot(ctx, repoRoot)
			if snapErr != nil {
				return nil, fmt.Errorf("create parent snapshot: %w", snapErr)
			}
			baseCommit = snap
			policy = PolicySnapshotAuthorized
			validation = "parent dirty changes snapshotted with authorization"
		} else {
			// DiscloseDirtyAbsent
			if opts.BaseCommit != "" {
				baseCommit = opts.BaseCommit
			} else {
				headCommit, headErr := getCommit(ctx, repoRoot, "HEAD")
				if headErr != nil {
					return nil, fmt.Errorf("get parent HEAD commit: %w", headErr)
				}
				baseCommit = headCommit
			}
			policy = PolicyDirtyDisclosedAbsent
			validation = "parent dirty changes disclosed absent at allocation"
		}
	} else {
		if opts.BaseCommit != "" {
			baseCommit = opts.BaseCommit
		} else {
			headCommit, headErr := getCommit(ctx, repoRoot, "HEAD")
			if headErr != nil {
				return nil, fmt.Errorf("get parent HEAD commit: %w", headErr)
			}
			baseCommit = headCommit
		}
		policy = PolicyClean
		validation = "clean committed base"
	}

	// Resolve branch name
	branch := opts.Branch
	if branch == "" {
		branch = fmt.Sprintf("worktree/%s/%s", sanitize(opts.Run), sanitize(opts.Agent))
	}
	if branchExists(ctx, repoRoot, branch) {
		return nil, fmt.Errorf("%w: branch %s already exists", ErrWorktreeExists, branch)
	}

	// Resolve worktree directory
	worktreeDir := opts.WorktreeDir
	if worktreeDir == "" {
		worktreeDir = filepath.Join(os.TempDir(), "ugh-worktrees", fmt.Sprintf("%s-%s", sanitize(opts.Run), sanitize(opts.Agent)))
	}
	worktreeDir = filepath.Clean(worktreeDir)

	if fi, statErr := os.Stat(worktreeDir); statErr == nil {
		if fi.IsDir() {
			// Check if existing worktree has active execution
			existing, loadErr := Load(worktreeDir)
			if loadErr == nil && existing.HasActiveExecution() {
				return nil, fmt.Errorf("%w: existing worktree %s has active writer/execution (%w)", ErrNoHandoff, worktreeDir, ErrActiveWriter)
			}
		}
		return nil, fmt.Errorf("%w: directory %s already exists", ErrWorktreeExists, worktreeDir)
	}

	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent dir: %w", err)
	}

	// Run git worktree add
	_, err = runGitCmd(ctx, repoRoot, "worktree", "add", "-b", branch, worktreeDir, baseCommit)
	if err != nil {
		return nil, fmt.Errorf("git worktree add failed: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	meta := Metadata{
		Repo:            repoRoot,
		WorktreeDir:     worktreeDir,
		BaseCommit:      baseCommit,
		Branch:          branch,
		Run:             opts.Run,
		Agent:           opts.Agent,
		Task:            opts.Task,
		Lifecycle:       LifecycleActive,
		DirtyBasePolicy: policy,
		Validation:      validation,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	sessionFile := opts.SessionFile
	if sessionFile == "" {
		sessionFile = filepath.Join(worktreeDir, ".ugh-session.jsonl")
	}

	wt := &Worktree{
		Meta:            meta,
		RepoRoot:        repoRoot,
		Dir:             worktreeDir,
		SessionStore:    opts.Store,
		SessionCallback: opts.SessionCallback,
		SessionFile:     sessionFile,
	}

	if err := wt.SaveMeta(); err != nil {
		// Attempt rollback
		_, _ = runGitCmd(ctx, repoRoot, "worktree", "remove", "--force", worktreeDir)
		_, _ = runGitCmd(ctx, repoRoot, "branch", "-D", branch)
		return nil, fmt.Errorf("save initial metadata: %w", err)
	}

	return wt, nil
}

// Integrate performs an explicit, narrow integration of worktree changes into the target branch.
// It verifies under lock that the target branch exists, matches ExpectedTargetCommit (rejecting destination changes),
// and has clean working state. Immediately prior to Git execution, it evaluates the permission gate.
// If merge/cherry-pick produces conflicts, Git operations are immediately aborted and ErrConflict is returned (no auto-merge).
func Integrate(ctx context.Context, repoRoot string, opts IntegrateOptions) (*IntegrateResult, error) {
	if opts.WorktreeDir == "" {
		return nil, fmt.Errorf("integrate: worktree directory required")
	}
	if opts.TargetBranch == "" {
		return nil, fmt.Errorf("integrate: target branch required")
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = StrategyMerge
	}

	repoRoot = filepath.Clean(repoRoot)
	gitCommonDir, err := FindGitCommonDir(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("find git common dir: %w", err)
	}

	sourceCommonDir, err := FindGitCommonDir(ctx, opts.WorktreeDir)
	if err != nil {
		return nil, fmt.Errorf("source worktree repository: %w", err)
	}
	targetCanonical, targetErr := filepath.EvalSymlinks(gitCommonDir)
	sourceCanonical, sourceErr := filepath.EvalSymlinks(sourceCommonDir)
	if targetErr != nil || sourceErr != nil || targetCanonical != sourceCanonical {
		return nil, fmt.Errorf("source worktree does not belong to the target repository")
	}
	lock, err := LockRepo(ctx, gitCommonDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()

	// 1. Check worktree working tree cleanliness
	wtDirty, err := isDirty(ctx, opts.WorktreeDir)
	if err != nil {
		return nil, fmt.Errorf("check worktree dirty status: %w", err)
	}
	if wtDirty {
		return nil, ErrWorktreeDirty
	}

	wtCommit, err := getCommit(ctx, opts.WorktreeDir, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("get worktree commit: %w", err)
	}
	wtBranch, _ := getCurrentBranch(ctx, opts.WorktreeDir)

	// 2. Check target branch in repository
	targetCommit, err := getCommit(ctx, repoRoot, opts.TargetBranch)
	if err != nil {
		return nil, fmt.Errorf("get target branch commit (%s): %w", opts.TargetBranch, err)
	}

	// 3. Recheck expected commit (changed destination rejection)
	if opts.ExpectedTargetCommit != "" && targetCommit != opts.ExpectedTargetCommit {
		return nil, fmt.Errorf("%w: target branch %s is at %s, expected %s",
			ErrTargetCommitMismatch, opts.TargetBranch, targetCommit, opts.ExpectedTargetCommit)
	}

	// 4. Check repo dirty status
	repoDirty, err := isDirty(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("check repo dirty status: %w", err)
	}
	if repoDirty {
		return nil, ErrTargetDirty
	}

	// 5. Ensure target branch is checked out in repoRoot
	currBranch, err := getCurrentBranch(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("get current branch: %w", err)
	}
	if currBranch != opts.TargetBranch {
		return nil, fmt.Errorf("%w: target %s is not the selected branch %s", ErrTargetCommitMismatch, opts.TargetBranch, currBranch)
	}

	// 6. Recheck target branch dirty status immediately before gate & Git command
	repoDirty, err = isDirty(ctx, repoRoot)
	if err != nil || repoDirty {
		return nil, ErrTargetDirty
	}

	checkInactive := func() error {
		wt, err := Load(opts.WorktreeDir)
		if err != nil {
			return fmt.Errorf("load managed worktree: %w", err)
		}
		if wt.HasActiveExecution() {
			return ErrActiveWriter
		}
		return nil
	}
	if err := checkInactive(); err != nil {
		return nil, err
	}

	// 7. Evaluate permission gate immediately before running Git merge or cherry-pick
	if opts.PermGate != nil {
		if err := opts.PermGate.CheckPermission(ctx, "integrate", opts.WorktreeDir); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}

	if err := checkInactive(); err != nil {
		return nil, err
	}

	// A human permission prompt can take arbitrarily long. Recheck both
	// snapshots after it, and never integrate a moving source branch ref.
	afterTarget, err := getCommit(ctx, repoRoot, opts.TargetBranch)
	if err != nil || afterTarget != targetCommit {
		return nil, ErrTargetCommitMismatch
	}
	afterBranch, err := getCurrentBranch(ctx, repoRoot)
	if err != nil || afterBranch != opts.TargetBranch {
		return nil, ErrTargetCommitMismatch
	}
	if dirty, err := isDirty(ctx, repoRoot); err != nil || dirty {
		return nil, ErrTargetDirty
	}
	afterSource, err := getCommit(ctx, opts.WorktreeDir, "HEAD")
	if err != nil || afterSource != wtCommit {
		return nil, fmt.Errorf("worktree source changed during permission approval")
	}
	if dirty, err := isDirty(ctx, opts.WorktreeDir); err != nil || dirty {
		return nil, ErrWorktreeDirty
	}

	// 8. Execute integration
	switch strategy {
	case StrategyCherryPick:
		commits, err := getCommitsBetween(ctx, repoRoot, targetCommit, wtCommit)
		if err != nil {
			return nil, fmt.Errorf("list commits to cherry-pick: %w", err)
		}
		if len(commits) == 0 {
			return &IntegrateResult{
				TargetBranch: opts.TargetBranch,
				PriorCommit:  targetCommit,
				NewCommit:    targetCommit,
				Strategy:     StrategyCherryPick,
				Message:      "already up to date",
			}, nil
		}
		for _, commit := range commits {
			_, cpErr := runGitCmd(ctx, repoRoot, "cherry-pick", commit)
			if cpErr != nil {
				conflicted := getConflictedFiles(ctx, repoRoot)
				_, _ = runGitCmd(ctx, repoRoot, "cherry-pick", "--abort")
				return nil, &ConflictError{
					Strategy:        StrategyCherryPick,
					ConflictedFiles: conflicted,
					Output:          cpErr.Error(),
				}
			}
		}

	case StrategyMerge:
		// Attempt merge --no-ff
		msg := fmt.Sprintf("Integrate %s", wtBranch)
		_, mergeErr := runGitCmd(ctx, repoRoot, "merge", "--no-ff", "-m", msg, wtCommit)
		if mergeErr != nil {
			conflicted := getConflictedFiles(ctx, repoRoot)
			_, _ = runGitCmd(ctx, repoRoot, "merge", "--abort")
			return nil, &ConflictError{
				Strategy:        StrategyMerge,
				ConflictedFiles: conflicted,
				Output:          mergeErr.Error(),
			}
		}
	default:
		return nil, fmt.Errorf("unsupported integration strategy: %s", strategy)
	}

	newCommit, err := getCommit(ctx, repoRoot, opts.TargetBranch)
	if err != nil {
		return nil, fmt.Errorf("get new commit: %w", err)
	}

	// Update metadata in worktree if present
	wt, loadErr := Load(opts.WorktreeDir)
	if loadErr == nil {
		wt.Meta.Lifecycle = LifecycleIntegrated
		wt.Meta.Result = fmt.Sprintf("integrated into %s at %s", opts.TargetBranch, newCommit)
		_ = wt.SaveMeta()
	}

	return &IntegrateResult{
		TargetBranch: opts.TargetBranch,
		PriorCommit:  targetCommit,
		NewCommit:    newCommit,
		Strategy:     strategy,
		Message:      "integrated successfully",
	}, nil
}

// Recover inspects an existing or unfinished worktree, determining its state,
// uncommitted changes, commits ahead, surviving processes, and integration eligibility.
func Recover(ctx context.Context, repoRoot, worktreeDir string) (*RecoveryReport, error) {
	return RecoverWithOptions(ctx, repoRoot, worktreeDir, RecoverOptions{})
}

// RecoverWithOptions inspects an existing or unfinished worktree with options and permission check.
func RecoverWithOptions(ctx context.Context, repoRoot, worktreeDir string, opts RecoverOptions) (*RecoveryReport, error) {
	if opts.PermGate != nil {
		if err := opts.PermGate.CheckPermission(ctx, "recover", worktreeDir); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}

	report := &RecoveryReport{
		WorktreeExists: false,
	}

	wt, err := Load(worktreeDir)
	if err == nil {
		report.Meta = wt.Meta
	} else {
		report.Meta = Metadata{
			Repo:        repoRoot,
			WorktreeDir: worktreeDir,
		}
	}

	if fi, statErr := os.Stat(worktreeDir); statErr == nil && fi.IsDir() {
		report.WorktreeExists = true
	}

	if report.WorktreeExists {
		dirty, dErr := isDirty(ctx, worktreeDir)
		if dErr == nil {
			report.IsClean = !dirty
		}
		uncommitted, uErr := getUncommittedFiles(ctx, worktreeDir)
		if uErr == nil {
			report.UncommittedFiles = uncommitted
		}
		if report.Meta.BaseCommit != "" {
			commits, cErr := getCommitsBetween(ctx, worktreeDir, report.Meta.BaseCommit, "HEAD")
			if cErr == nil {
				report.CommitsAhead = commits
			}
		}
	}

	if report.Meta.Branch != "" {
		report.BranchExists = branchExists(ctx, repoRoot, report.Meta.Branch)
	}

	activeProcs, procErr := DetectActiveProcesses(worktreeDir, report.Meta.ActivePIDs, report.Meta.PIDStartTimes)
	if procErr != nil {
		report.CanCleanup = false
		report.CanIntegrate = false
		return report, fmt.Errorf("inspect active processes: %w", procErr)
	}
	report.ActiveProcesses = activeProcs

	report.CanCleanup = len(activeProcs) == 0
	report.CanIntegrate = report.WorktreeExists && report.IsClean && len(report.CommitsAhead) > 0 &&
		len(activeProcs) == 0 && report.Meta.Lifecycle != LifecycleIntegrated && report.Meta.Lifecycle != LifecycleCleaned

	return report, nil
}

// List lists all managed worktrees for a repository.
func List(ctx context.Context, repoRoot string) ([]*Worktree, error) {
	return ListWithOptions(ctx, repoRoot, ListOptions{})
}

// ListWithOptions lists worktrees with optional permission checks.
func ListWithOptions(ctx context.Context, repoRoot string, opts ListOptions) ([]*Worktree, error) {
	if opts.PermGate != nil {
		if err := opts.PermGate.CheckPermission(ctx, "list", repoRoot); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}

	repoRoot = filepath.Clean(repoRoot)
	out, err := runGitCmd(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	var worktrees []*Worktree
	blocks := strings.Split(out, "\n\n")
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		var dir string
		for _, l := range lines {
			if strings.HasPrefix(l, "worktree ") {
				dir = filepath.Clean(strings.TrimSpace(strings.TrimPrefix(l, "worktree ")))
				break
			}
		}
		if dir == "" || dir == repoRoot {
			continue // Skip repo root
		}

		wt, err := Load(dir)
		if err == nil {
			worktrees = append(worktrees, wt)
		} else {
			// Unmanaged or partially cleaned worktree
			worktrees = append(worktrees, &Worktree{
				Meta: Metadata{
					Repo:        repoRoot,
					WorktreeDir: dir,
					Lifecycle:   LifecycleAllocated,
				},
				RepoRoot: repoRoot,
				Dir:      dir,
			})
		}
	}

	return worktrees, nil
}

// Cleanup performs explicit cleanup of a worktree.
// It refuses cleanup if the worktree has active execution or surviving processes.
// It evaluates the permission gate callback before removing any files or branches.
func Cleanup(ctx context.Context, repoRoot, worktreeDir string, opts CleanupOptions) error {
	worktreeDir = filepath.Clean(worktreeDir)
	repoRoot = filepath.Clean(repoRoot)

	// Load metadata
	wt, _ := Load(worktreeDir)
	var trackedPIDs []int
	var startTimes map[int]uint64
	var branch string
	var lifecycle LifecycleState
	if wt != nil {
		trackedPIDs = wt.Meta.ActivePIDs
		startTimes = wt.Meta.PIDStartTimes
		branch = wt.Meta.Branch
		lifecycle = wt.Meta.Lifecycle
	}

	// Check surviving processes; fail closed if inspection fails
	activePIDs, procErr := DetectActiveProcesses(worktreeDir, trackedPIDs, startTimes)
	if procErr != nil {
		return fmt.Errorf("cannot inspect surviving processes (fail closed): %w", procErr)
	}
	if len(activePIDs) > 0 {
		return fmt.Errorf("%w: surviving processes: %v", ErrActiveProcesses, activePIDs)
	}

	// Refuse cleanup if actively executing unless Force is set
	if lifecycle == LifecycleActive && !opts.Force {
		return fmt.Errorf("%w: worktree is active; mark completed/failed/cancelled or use Force", ErrActiveWriter)
	}

	// Refuse cleanup if dirty checkout exists unless Force is set
	dirty, dErr := isDirty(ctx, worktreeDir)
	if dErr == nil && dirty && !opts.Force {
		return fmt.Errorf("%w: worktree has uncommitted changes; preserve work or use Force", ErrWorktreeDirty)
	}

	// Check permission gate callback
	if opts.PermGate != nil {
		if err := opts.PermGate.CheckPermission(ctx, "cleanup", worktreeDir); err != nil {
			return fmt.Errorf("%w: %v", ErrPermissionDenied, err)
		}
	}

	gitCommonDir, err := FindGitCommonDir(ctx, repoRoot)
	if err != nil {
		return fmt.Errorf("find git common dir: %w", err)
	}

	lock, err := LockRepo(ctx, gitCommonDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	// Remove worktree via Git
	_, _ = runGitCmd(ctx, repoRoot, "worktree", "remove", "--force", worktreeDir)

	// Clean remaining directory if still present
	_ = os.RemoveAll(worktreeDir)

	// Delete branch if requested
	if opts.DeleteBranch && branch != "" {
		flag := "-d"
		if opts.Force {
			flag = "-D"
		}
		if _, err := runGitCmd(ctx, repoRoot, "branch", flag, branch); err != nil {
			return fmt.Errorf("worktree removed, branch %s preserved: %w", branch, err)
		}
	}

	// Prune git worktrees
	_, _ = runGitCmd(ctx, repoRoot, "worktree", "prune")

	if wt != nil {
		wt.Meta.Lifecycle = LifecycleCleaned
	}

	return nil
}

// RecoverCrashedWriter explicitly transitions an orphaned or crashed active worktree
// to LifecycleFailed without deleting or modifying the worktree's dirty checkout or branch.
// It refuses recovery if surviving active processes are detected (no live handoff).
func RecoverCrashedWriter(ctx context.Context, repoRoot, worktreeDir, reason string) (*Worktree, error) {
	worktreeDir = filepath.Clean(worktreeDir)
	repoRoot = filepath.Clean(repoRoot)

	wt, err := Load(worktreeDir)
	if err != nil {
		return nil, fmt.Errorf("load worktree for recovery: %w", err)
	}

	activePIDs, err := DetectActiveProcesses(worktreeDir, wt.Meta.ActivePIDs, wt.Meta.PIDStartTimes)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect surviving processes (fail closed): %w", err)
	}
	if len(activePIDs) > 0 {
		return nil, fmt.Errorf("%w: surviving processes %v still executing; cannot hand off or recover active writer", ErrNoHandoff, activePIDs)
	}

	if wt.Meta.Lifecycle != LifecycleActive && wt.Meta.Lifecycle != LifecycleAllocated {
		return wt, nil // Already in terminal state
	}

	if reason == "" {
		reason = "crashed writer recovered; preserved dirty checkout"
	} else {
		reason = "crashed writer recovered: " + reason
	}

	wt.Meta.Lifecycle = LifecycleFailed
	wt.Meta.Result = reason
	wt.Meta.ActivePIDs = nil
	if err := wt.SaveMeta(); err != nil {
		return nil, fmt.Errorf("save recovered metadata: %w", err)
	}
	return wt, nil
}

// RecoverCrashed marks a crashed active worktree as failed, clearing dead PIDs while
// preserving all dirty checkout state and commits ahead on disk.
// Returns ErrNoHandoff if surviving active processes are detected.
func (w *Worktree) RecoverCrashed(reason string) error {
	activePIDs, err := DetectActiveProcesses(w.Dir, w.Meta.ActivePIDs, w.Meta.PIDStartTimes)
	if err != nil {
		return fmt.Errorf("cannot inspect surviving processes (fail closed): %w", err)
	}
	if len(activePIDs) > 0 {
		return fmt.Errorf("%w: surviving processes %v still executing; cannot hand off or recover active writer", ErrNoHandoff, activePIDs)
	}
	if reason == "" {
		reason = "crashed writer recovered; preserved dirty checkout"
	}
	w.Meta.Lifecycle = LifecycleFailed
	w.Meta.Result = reason
	w.Meta.ActivePIDs = nil
	return w.SaveMeta()
}
