package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/perm/static"
	"github.com/unix/ugh/internal/session"
)

// LifecycleState defines the phases of a worktree's lifecycle.
type LifecycleState string

const (
	LifecycleAllocated  LifecycleState = "allocated"
	LifecycleActive     LifecycleState = "active"
	LifecycleCompleted  LifecycleState = "completed"
	LifecycleFailed     LifecycleState = "failed"
	LifecycleCancelled  LifecycleState = "cancelled"
	LifecycleIntegrated LifecycleState = "integrated"
	LifecycleCleaned    LifecycleState = "cleaned"
)

// DirtyBasePolicy describes how the parent repository's dirty state was handled during allocation.
type DirtyBasePolicy string

const (
	PolicyClean                DirtyBasePolicy = "clean"
	PolicyDirtyDisclosedAbsent DirtyBasePolicy = "dirty_disclosed_absent"
	PolicySnapshotAuthorized   DirtyBasePolicy = "snapshot_authorized"
)

// IntegrateStrategy specifies how worktree changes should be integrated into the target branch.
type IntegrateStrategy string

const (
	StrategyMerge      IntegrateStrategy = "merge"
	StrategyCherryPick IntegrateStrategy = "cherry-pick"
)

// Standard typed errors.
var (
	ErrActiveWriter            = errors.New("worktree has an active writer")
	ErrActiveProcesses         = errors.New("worktree has surviving active processes")
	ErrWorktreeExists          = errors.New("worktree directory or branch already exists")
	ErrWorktreeNotFound        = errors.New("worktree not found")
	ErrNoHandoff               = errors.New("worktree handoff or reuse refused while active execution or uncleaned state exists")
	ErrDirtyParentNotDisclosed = errors.New("parent repository has uncommitted changes; allocation requires explicit disclosure that dirty changes are absent or authorization for snapshot")
	ErrTargetCommitMismatch    = errors.New("target branch commit does not match expected commit: changed destination rejection")
	ErrTargetDirty             = errors.New("target repository has uncommitted changes: integration rejected")
	ErrWorktreeDirty           = errors.New("worktree working copy has uncommitted changes")
	ErrConflict                = errors.New("integration encountered merge conflict: automatic merge stopped")
	ErrPermissionDenied        = errors.New("operation denied by permission gate")
)

// ConflictError gives detailed information about a conflict during integration.
type ConflictError struct {
	Strategy        IntegrateStrategy `json:"strategy"`
	ConflictedFiles []string          `json:"conflicted_files"`
	Output          string            `json:"output"`
}

func (e *ConflictError) Error() string {
	if len(e.ConflictedFiles) > 0 {
		return fmt.Sprintf("integration conflict (%s) in files: %v: auto-merge stopped", e.Strategy, e.ConflictedFiles)
	}
	return fmt.Sprintf("integration conflict (%s): %s: auto-merge stopped", e.Strategy, e.Output)
}

func (e *ConflictError) Unwrap() error {
	return ErrConflict
}

// Metadata holds all lifecycle, ownership, validation, and result information for a worktree.
// It is designed to be stored as a lightweight JSON record or mapped to session records,
// avoiding any external ownership database.
type Metadata struct {
	Repo            string          `json:"repo"`
	WorktreeDir     string          `json:"worktree_dir"`
	BaseCommit      string          `json:"base_commit"`
	Branch          string          `json:"branch"`
	Run             string          `json:"run"`
	Agent           string          `json:"agent"`
	Task            string          `json:"task"`
	Lifecycle       LifecycleState  `json:"lifecycle"`
	Validation      string          `json:"validation,omitempty"`
	Result          string          `json:"result,omitempty"`
	DirtyBasePolicy DirtyBasePolicy `json:"dirty_base_policy,omitempty"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	ActivePIDs      []int           `json:"active_pids,omitempty"`
	PIDStartTimes   map[int]uint64  `json:"pid_start_times,omitempty"`
}

// ToSessionRecord formats this metadata as a session.Record suitable for existing session logs.
// The full Metadata payload is encoded in System to guarantee 100% round-trip fidelity
// without creating an external ownership database.
func (m *Metadata) ToSessionRecord() session.Record {
	status := string(m.Lifecycle)
	errStr := ""
	if m.Lifecycle == LifecycleFailed || m.Lifecycle == LifecycleCancelled {
		errStr = m.Result
	}
	sysBytes, _ := json.Marshal(m)
	return session.Record{
		Type:   "worktree",
		TS:     m.UpdatedAt,
		Run:    m.Run,
		Agent:  m.Agent,
		Cwd:    m.WorktreeDir,
		Status: status,
		Error:  errStr,
		System: string(sysBytes),
	}
}

// FromSessionRecord reconstructs metadata from a session.Record.
func FromSessionRecord(r session.Record) (*Metadata, bool) {
	if r.Type != "worktree" {
		return nil, false
	}
	var m Metadata
	if r.System != "" {
		if err := json.Unmarshal([]byte(r.System), &m); err == nil {
			if r.Run != "" {
				m.Run = r.Run
			}
			if r.Agent != "" {
				m.Agent = r.Agent
			}
			if r.Cwd != "" {
				m.WorktreeDir = r.Cwd
			}
			if r.Status != "" {
				m.Lifecycle = LifecycleState(r.Status)
			}
			if r.Error != "" {
				m.Result = r.Error
			}
			if r.TS != "" {
				m.UpdatedAt = r.TS
			}
			return &m, true
		}
	}
	return &Metadata{
		Run:         r.Run,
		Agent:       r.Agent,
		WorktreeDir: r.Cwd,
		Lifecycle:   LifecycleState(r.Status),
		Result:      r.Error,
		UpdatedAt:   r.TS,
	}, true
}

// PermissionGate checks if an action (e.g. "cleanup", "integrate", "recover") is permitted.
type PermissionGate interface {
	CheckPermission(ctx context.Context, action string, target string) error
}

// PermissionFunc adapts a function to PermissionGate.
type PermissionFunc func(ctx context.Context, action string, target string) error

func (f PermissionFunc) CheckPermission(ctx context.Context, action string, target string) error {
	return f(ctx, action, target)
}

// FromPermGate adapts an internal/perm.Gate to PermissionGate.
func FromPermGate(gate perm.Gate, agent, cwd string) PermissionGate {
	if gate == nil {
		return nil
	}
	return PermissionFunc(func(ctx context.Context, action string, target string) error {
		call := perm.Call{
			Agent: agent,
			Cwd:   cwd,
			Tool:  "worktree_" + action,
			Args:  json.RawMessage(fmt.Sprintf(`{"action":%q,"target":%q}`, action, target)),
		}
		outcome := gate.Check(ctx, call)
		if outcome.Action != perm.Allow {
			if outcome.Reason != "" {
				return fmt.Errorf("%w: %s (%s)", ErrPermissionDenied, outcome.Reason, outcome.Action)
			}
			return fmt.Errorf("%w: action %s on %s was %s", ErrPermissionDenied, action, target, outcome.Action)
		}
		return nil
	})
}

// StaticGitClassifier classifies git commands with internal/perm/static.ParseGitCommand.
type StaticGitClassifier struct {
	Cwd string
}

// IsSharedRefMutation checks whether the git command string modifies shared refs, worktrees, or network remotes,
// or escapes the worktree directory via -C.
func (s StaticGitClassifier) IsSharedRefMutation(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	parsed, ok := static.ParseGitCommand(fields, s.Cwd)
	if !ok || parsed == nil {
		return false
	}
	if parsed.MutatesRefs || parsed.Network || parsed.WorktreeOp {
		return true
	}
	if parsed.TargetDir != "" && s.Cwd != "" {
		cleanTarget := filepath.Clean(parsed.TargetDir)
		cleanCwd := filepath.Clean(s.Cwd)
		if cleanTarget != cleanCwd && !strings.HasPrefix(cleanTarget, cleanCwd+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// AllocateOptions specifies parameters for creating a new worktree.
type AllocateOptions struct {
	Run                 string                     // Owner run ID (required)
	Agent               string                     // Owner agent ID (required, e.g. "root/1")
	Task                string                     // Task description
	Branch              string                     // Feature branch name (auto-generated if empty)
	WorktreeDir         string                     // Worktree directory (auto-generated if empty)
	BaseCommit          string                     // Base commit (HEAD if empty)
	DiscloseDirtyAbsent bool                       // Explicit disclosure that dirty parent changes will be absent
	AuthorizeSnapshot   bool                       // Explicit authorization to snapshot dirty parent changes into base
	PermGate            PermissionGate             // Optional permission gate
	Store               *session.Store             // Optional session store to append to
	SessionCallback     func(session.Record) error // Optional callback to record session metadata
	SessionFile         string                     // Optional session file path (.jsonl)
}

// IntegrateOptions specifies parameters for integrating worktree changes.
type IntegrateOptions struct {
	WorktreeDir          string            // Path to worktree to integrate (required)
	TargetBranch         string            // Target branch name in repository (e.g. "master")
	ExpectedTargetCommit string            // Expected commit of target branch (rejects if changed)
	Strategy             IntegrateStrategy // StrategyMerge (default) or StrategyCherryPick
	PermGate             PermissionGate    // Permission gate callback checked immediately before Git operations
}

// IntegrateResult returns the outcome of an integration operation.
type IntegrateResult struct {
	TargetBranch string            `json:"target_branch"`
	PriorCommit  string            `json:"prior_commit"`
	NewCommit    string            `json:"new_commit"`
	Strategy     IntegrateStrategy `json:"strategy"`
	Message      string            `json:"message"`
}

// CleanupOptions controls worktree cleanup.
type CleanupOptions struct {
	DeleteBranch bool           // Whether to also delete the feature branch
	Force        bool           // If true, removes worktree even if uncommitted changes exist (active execution still refused)
	PermGate     PermissionGate // Permission gate callback checked before removal
}

// ListOptions controls worktree listing.
type ListOptions struct {
	PermGate PermissionGate
}

// RecoverOptions controls worktree recovery inspection.
type RecoverOptions struct {
	PermGate PermissionGate
}

// RecoveryReport provides a full diagnostic summary of an unfinished or existing worktree.
type RecoveryReport struct {
	Meta             Metadata `json:"meta"`
	WorktreeExists   bool     `json:"worktree_exists"`
	BranchExists     bool     `json:"branch_exists"`
	IsClean          bool     `json:"is_clean"`
	UncommittedFiles []string `json:"uncommitted_files,omitempty"`
	CommitsAhead     []string `json:"commits_ahead,omitempty"`
	ActiveProcesses  []int    `json:"active_processes,omitempty"`
	CanIntegrate     bool     `json:"can_integrate"`
	CanCleanup       bool     `json:"can_cleanup"`
}
