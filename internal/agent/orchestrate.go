package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/perm/static"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/worktree"
)

// Orchestrators delegate and supervise independent feature work; they never
// implement it themselves. Their tools are read-only plus spawn (added by New)
// and the worktree tools below.
const orchestratorPurpose = "Purpose: Split a large piece of work into independent features, delegate each to a worker, supervise them, and report what finished.\n" +
	"Typical tasks: a multi-feature change where the features touch different files and can proceed in parallel.\n" +
	"Not for: a single change (worker), research (scout), architecture advice (oracle)."

const orchestratorPrompt = `You are an orchestrator. Your job is to delegate and supervise, not to implement: you have no edit, write or shell tools. Split the assignment into independent feature tasks and spawn a worker for each. Every worker you spawn gets its own harness-managed git worktree on its own branch, started from the last commit (uncommitted changes in the parent checkout are NOT in the worktree); say so in each brief if it matters, and give each worker a self-contained brief with its acceptance checks. Do not run two workers on overlapping files. Wait for the workers, read their reports, and use worktree_status to see each worktree. Integrate a finished worktree only with worktree_integrate, only after its report and validation look right, and stop and report any conflict instead of resolving it. Never delete work: worktree_cleanup only for worktrees whose work is integrated or explicitly abandoned. Finish with a report of what each worker did, what was integrated, and what still needs the user.`

// canOrchestrate reports whether parent may spawn an orchestrator: the child
// must still be able to delegate (depth), and orchestrators do not nest.
func canOrchestrate(parent *Agent) bool {
	return parent.cfg.Node.Depth+1 < maxSpawnDepth && parent.cfg.Node.Role != "orchestrator"
}

// maxSpawnDepth is the depth at which spawn is no longer offered.
const maxSpawnDepth = 4

// spawnsChildren reports whether a node is given the spawn capability. A
// feature writer (a worker under an orchestrator) does not delegate: its
// children would share its worktree and break one-writer-per-worktree.
func spawnsChildren(n *Node) bool {
	switch n.Role {
	case "root", "orchestrator":
		return true
	case "worker":
		return n.Parent == nil || n.Parent.Role != "orchestrator"
	}
	return false
}

// allowedRoles lists the roles a parent may delegate to right now.
func allowedRoles(parent *Agent) []string {
	var out []string
	for _, id := range roleIDs {
		switch {
		case id == "oracle" && oracleDenied(parent):
		case id == "orchestrator" && !canOrchestrate(parent):
		default:
			out = append(out, id)
		}
	}
	return out
}

// isFeatureWriter: workers spawned by an orchestrator each get a fresh worktree.
func isFeatureWriter(parent *Agent, role string) bool {
	return role == "worker" && parent.cfg.Node.Role == "orchestrator"
}

func gitTop(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// allocateWriter gives a feature writer its own worktree. The base is the last
// commit; dirty parent changes are disclosed as absent, never silently copied.
func (r *Runtime) allocateWriter(ctx context.Context, node *Node, task, cwd string) (*worktree.Worktree, string, error) {
	top, err := gitTop(ctx, cwd)
	if err != nil {
		return nil, "", fmt.Errorf("feature writers need a git worktree: %w", err)
	}
	excludeMetadata(ctx, top)
	wt, err := worktree.Allocate(ctx, top, worktree.AllocateOptions{
		Run: r.RunID, Agent: node.ID, Task: task, DiscloseDirtyAbsent: true,
		WorktreeDir: r.worktreeDir(node.ID),
	})
	if err != nil {
		return nil, "", err
	}
	rel, _ := filepath.Rel(top, cwd)
	if rel == "." || strings.HasPrefix(rel, "..") {
		rel = ""
	}
	r.mu.Lock()
	if r.writers == nil {
		r.writers = map[string]*worktree.Worktree{}
	}
	r.writers[node.ID] = wt
	r.mu.Unlock()
	return wt, filepath.Join(wt.Dir, rel), nil
}

func (r *Runtime) writer(agent string) *worktree.Worktree {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writers[agent]
}

// runWorktree finds one of this run's worktrees by agent id or directory.
func (r *Runtime) runWorktree(ref string) (*worktree.Worktree, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, wt := range r.writers {
		if ref == id || filepath.Clean(ref) == wt.Dir {
			return wt, nil
		}
	}
	return nil, fmt.Errorf("%q is not a worktree of this run (worktree_status lists them)", ref)
}

// finishWriter records the lifecycle of a writer's worktree after a run. The
// work stays on disk whatever the outcome.
func (r *Runtime) finishWriter(node *Node, res *ChildResult) {
	wt := r.writer(node.ID)
	if wt == nil {
		return
	}
	summary := strings.TrimSpace(res.Report)
	if len(summary) > 300 {
		summary = summary[:300] + "…"
	}
	switch res.Status {
	case "ok":
		_ = wt.MarkCompleted(summary)
	case "cancelled":
		_ = wt.MarkCancelled("cancelled; work preserved")
	default:
		_ = wt.MarkFailed(res.Status + ": " + summary)
	}
	res.Report += fmt.Sprintf("\n\n(worktree %s · branch %s · base %s · now %s · parent changes were not copied; "+
		"unfinished work is kept, integrate with worktree_integrate, nothing is cleaned up automatically)",
		wt.Dir, wt.Meta.Branch, short(wt.Meta.BaseCommit), wt.Meta.Lifecycle)
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// ---- worktree tools (root and orchestrators) ----

type worktreeStatusTool struct{ agent *Agent }
type worktreeIntegrateTool struct{ agent *Agent }
type worktreeCleanupTool struct{ agent *Agent }

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func (worktreeStatusTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "worktree_status", Description: "List this run's managed feature worktrees: owner, branch, base commit, lifecycle, uncommitted files, commits ahead, and the target repository's current branch and commit.",
		Parameters: obj(map[string]any{})}
}
func (t worktreeStatusTool) Run(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
	rt := t.agent.rt
	if rt == nil {
		return "", fmt.Errorf("no managed worktrees in this session")
	}
	rt.mu.Lock()
	var list []*worktree.Worktree
	for _, wt := range rt.writers {
		list = append(list, wt)
	}
	rt.mu.Unlock()
	if len(list) == 0 {
		return "no managed worktrees in this run", nil
	}
	var b strings.Builder
	for _, wt := range list {
		rep, err := wt.Status(ctx)
		if err != nil {
			fmt.Fprintf(&b, "- %s: status error: %v\n", wt.Dir, err)
			continue
		}
		head, _ := exec.CommandContext(ctx, "git", "-C", wt.RepoRoot, "rev-parse", "--abbrev-ref", "HEAD").Output()
		tip, _ := exec.CommandContext(ctx, "git", "-C", wt.RepoRoot, "rev-parse", "HEAD").Output()
		fmt.Fprintf(&b, "- agent %s · dir %s · branch %s · base %s · %s · clean=%v · ahead=%d · uncommitted=%d · active processes=%d\n  target repo %s is on %s at %s\n",
			wt.Meta.Agent, wt.Dir, wt.Meta.Branch, short(wt.Meta.BaseCommit), rep.Meta.Lifecycle, rep.IsClean,
			len(rep.CommitsAhead), len(rep.UncommittedFiles), len(rep.ActiveProcesses),
			wt.RepoRoot, strings.TrimSpace(string(head)), strings.TrimSpace(string(tip)))
	}
	return b.String(), nil
}

func (worktreeIntegrateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "worktree_integrate", Description: "Merge a finished feature worktree's branch into a target branch. Refuses while its writer is active, when the worktree is dirty, or when the target branch is not at expected_commit (get it from worktree_status). A conflict stops here and is reported; it is never resolved automatically.",
		Parameters: obj(map[string]any{
			"worktree":        map[string]any{"type": "string", "description": "agent id or directory from worktree_status"},
			"target_branch":   map[string]any{"type": "string"},
			"expected_commit": map[string]any{"type": "string", "description": "the commit the target branch must still be at"},
		}, "worktree", "target_branch", "expected_commit")}
}
func (t worktreeIntegrateTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var in struct{ Worktree, TargetBranch, ExpectedCommit string }
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	in.Worktree, in.TargetBranch, in.ExpectedCommit = m["worktree"], m["target_branch"], m["expected_commit"]
	if in.ExpectedCommit == "" {
		return "", fmt.Errorf("expected_commit is required: call worktree_status first")
	}
	if t.agent.rt == nil {
		return "", fmt.Errorf("no managed worktrees in this session")
	}
	wt, err := t.agent.rt.runWorktree(in.Worktree)
	if err != nil {
		return "", err
	}
	res, err := worktree.Integrate(ctx, wt.RepoRoot, worktree.IntegrateOptions{WorktreeDir: wt.Dir,
		TargetBranch: in.TargetBranch, ExpectedTargetCommit: in.ExpectedCommit, Strategy: worktree.StrategyMerge})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("integrated %s into %s: %s → %s", wt.Meta.Branch, res.TargetBranch, short(res.PriorCommit), short(res.NewCommit)), nil
}

func (worktreeCleanupTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "worktree_cleanup", Description: "Remove a finished or abandoned worktree. Refuses while its writer or tracked processes are active, and keeps the branch unless delete_branch is set (which refuses unmerged commits unless force).",
		Parameters: obj(map[string]any{
			"worktree":      map[string]any{"type": "string"},
			"delete_branch": map[string]any{"type": "boolean"},
			"force":         map[string]any{"type": "boolean", "description": "discard uncommitted work and unmerged commits"},
		}, "worktree")}
}
func (t worktreeCleanupTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var in struct {
		Worktree     string `json:"worktree"`
		DeleteBranch bool   `json:"delete_branch"`
		Force        bool   `json:"force"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", err
	}
	if t.agent.rt == nil {
		return "", fmt.Errorf("no managed worktrees in this session")
	}
	wt, err := t.agent.rt.runWorktree(in.Worktree)
	if err != nil {
		return "", err
	}
	if err := worktree.Cleanup(ctx, wt.RepoRoot, wt.Dir, worktree.CleanupOptions{DeleteBranch: in.DeleteBranch, Force: in.Force}); err != nil {
		return "", err
	}
	return "removed worktree " + wt.Dir, nil
}

// oracleDenied is the non-prompting check used when listing role options.
func oracleDenied(parent *Agent) bool {
	_, err := oracleCheck(councilCfgOf(parent), parent)
	return err != nil
}

// excludeMetadata keeps the library's per-worktree metadata files out of
// `git add .` (they live in the worktree root), so a writer's branch never
// carries them into the destination on integration.
func excludeMetadata(ctx context.Context, top string) {
	out, err := exec.CommandContext(ctx, "git", "-C", top, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(top, common)
	}
	path := filepath.Join(common, "info", "exclude")
	existing, _ := os.ReadFile(path)
	var add string
	for _, name := range []string{".ugh-session.jsonl", ".ugh-worktree.json"} {
		if !strings.Contains(string(existing), name) {
			add += name + "\n"
		}
	}
	if add == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		add = "\n" + add
	}
	_, _ = f.WriteString(add)
}

// ownerViolations applies the owner-only git boundary to a feature writer's
// bash call. Other agents are unrestricted here (the permission gate still applies).
func (a *Agent) ownerViolations(call perm.Call) []string {
	if call.Tool != "bash" || a.rt == nil {
		return nil
	}
	wt := a.rt.writer(a.cfg.Node.ID)
	if wt == nil {
		return nil
	}
	return static.GitOutsideOwnerViolations(call.Command, wt.Dir)
}

// worktreeDir is where an agent's managed worktree lives: [worktree] dir, else
// <state home>/ugh/worktrees (persistent, unlike the temp dir).
func (r *Runtime) worktreeDir(agent string) string {
	base := r.Config.Worktree.Dir
	if base == "" {
		state, err := session.StateDir() // .../ugh/sessions
		if err != nil {
			return "" // library default
		}
		base = filepath.Join(filepath.Dir(state), "worktrees")
	}
	return filepath.Join(base, r.RunID+"-"+strings.ReplaceAll(agent, "/", "_"))
}
