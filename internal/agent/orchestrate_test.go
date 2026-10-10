package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/tools"
	"github.com/unix/ugh/internal/worktree"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writerEnv: routing env with a worker role card, granted Go access and a git repo
// whose parent checkout is dirty. Returns the runtime, an orchestrator agent and the repo.
func writerEnv(t *testing.T) (*Runtime, *Agent, string) {
	t.Helper()
	routingEnv(t)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	grant(t, "opencode-go")
	writeRoleCards(t, "worker", "scout", "oracle")
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-qm", "base")
	os.WriteFile(filepath.Join(repo, "parent-dirty.txt"), []byte("uncommitted\n"), 0o600)

	r := NewRuntime()
	prov := &funcProvider{script: func(n int, req llm.Request) []llm.Event {
		if req.Messages[len(req.Messages)-1].Role != llm.RoleTool { // each writer writes once, then reports
			return []llm.Event{call("w", "write", `{"path":"feature.txt","content":"feature\n"}`)}
		}
		return []llm.Event{say("feature done")}
	}}
	oldResolve := resolveRoute
	resolveRoute = func(string) (llm.Provider, string, error) { return prov, "mimo-v2.6-flash", nil }
	t.Cleanup(func() { resolveRoute = oldResolve })
	node := r.Tree.Child(r.Tree.Root("root", "m", repo), "orchestrator", "m", repo)
	orch := New(Config{Provider: prov, Model: "m", Cwd: repo, Tree: r.Tree, Node: node, Agent: node.ID, Runtime: r,
		Spawn: r.Spawn, StartChild: r.StartChild, Tools: tools.NewRegistry()})
	return r, orch, repo
}

func TestOrchestratorToolsAndRoleRules(t *testing.T) {
	r, orch, _ := writerEnv(t)
	for _, name := range []string{"spawn", "worktree_status", "worktree_integrate", "worktree_cleanup"} {
		if _, ok := orch.cfg.Tools.Get(name); !ok {
			t.Errorf("orchestrator missing %s", name)
		}
	}
	for _, name := range []string{"write", "edit", "bash", "council"} {
		if _, ok := orch.cfg.Tools.Get(name); ok {
			t.Errorf("orchestrator must not have %s", name)
		}
	}
	c, err := readCard("roles", "orchestrator")
	if err != nil || contains(c.Tools, "write") || contains(c.Tools, "bash") || c.Prompt == "" {
		t.Fatalf("orchestrator card = %+v %v", c, err)
	}
	if canOrchestrate(orch) {
		t.Fatal("orchestrators must not nest")
	}
	root := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: &Tree{}})
	if !canOrchestrate(root) {
		t.Fatal("root should be able to spawn an orchestrator")
	}
	deep := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: r.Tree,
		Node: r.Tree.Child(r.Tree.Child(r.Tree.Child(r.Tree.Root("root", "m", ""), "worker", "m", ""), "worker", "m", ""), "worker", "m", "")})
	if canOrchestrate(deep) {
		t.Fatal("an orchestrator at depth 4 could not delegate; it must not be offered")
	}
	if res := r.startChild(context.Background(), orch, Task{Task: "x", Role: "orchestrator"}); res.Status != "error" {
		t.Fatalf("nested orchestrator spawn: %+v", res)
	}
	if contains(allowedRoles(orch), "orchestrator") || contains(allowedRoles(orch), "oracle") {
		t.Fatalf("roles offered under an orchestrator: %v", allowedRoles(orch))
	}
}

func TestFeatureWritersGetSeparateWorktreesAndIntegrateSafely(t *testing.T) {
	r, orch, repo := writerEnv(t)
	ctx := context.Background()
	res1 := r.startChild(ctx, orch, Task{Task: "feature one", Role: "worker"})
	res2 := r.startChild(ctx, orch, Task{Task: "feature two", Role: "worker"})
	for _, res := range []ChildResult{res1, res2} {
		if res.Status != "ok" || !strings.Contains(res.Report, "worktree ") || !strings.Contains(res.Report, "parent changes were not copied") {
			t.Fatalf("result = %+v", res)
		}
	}
	wt1, wt2 := r.writer(res1.ID), r.writer(res2.ID)
	if wt1 == nil || wt2 == nil || wt1.Dir == wt2.Dir || wt1.Meta.Branch == wt2.Meta.Branch {
		t.Fatalf("writers share a worktree: %v %v", wt1, wt2)
	}
	if res1.Cwd != wt1.Dir {
		t.Fatalf("child cwd %s is not its worktree %s", res1.Cwd, wt1.Dir)
	}
	if _, err := os.Stat(filepath.Join(wt1.Dir, "parent-dirty.txt")); err == nil {
		t.Fatal("dirty parent changes were silently copied")
	}
	if b, err := os.ReadFile(filepath.Join(wt1.Dir, "feature.txt")); err != nil || string(b) != "feature\n" {
		t.Fatalf("writer output missing in its worktree: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err == nil {
		t.Fatal("writer touched the parent checkout")
	}
	reloaded, err := worktree.Load(wt1.Dir)
	if err != nil || reloaded.Meta.Lifecycle != worktree.LifecycleCompleted || reloaded.Meta.DirtyBasePolicy != worktree.PolicyDirtyDisclosedAbsent {
		t.Fatalf("recorded metadata = %+v %v", reloaded, err)
	}
	// Dirty worktrees are refused; commit then integrate against the right destination commit only.
	integ := worktreeIntegrateTool{agent: orch}
	args := func(commit string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"worktree": res1.ID, "target_branch": "main", "expected_commit": commit})
		return b
	}
	tip := gitIn(t, repo, "rev-parse", "main")
	if _, err := integ.Run(ctx, "", args(tip)); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty worktree integrate: %v", err)
	}
	gitIn(t, wt1.Dir, "add", ".")
	gitIn(t, wt1.Dir, "commit", "-qm", "feature one")
	if _, err := integ.Run(ctx, "", args("deadbeef")); err == nil {
		t.Fatal("integration with a stale expected commit must be refused")
	}
	// The parent checkout is dirty, so integration into it is refused rather than clobbering changes.
	if _, err := integ.Run(ctx, "", args(tip)); err == nil {
		t.Fatal("integration into a dirty destination must be refused")
	}
	os.Remove(filepath.Join(repo, "parent-dirty.txt"))
	if out, err := integ.Run(ctx, "", args(tip)); err != nil || !strings.Contains(out, "integrated") {
		t.Fatalf("integrate: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("integrated feature missing from main: %v", err)
	}
	if files := gitIn(t, repo, "ls-tree", "-r", "--name-only", "main"); files != "base.txt\nfeature.txt" {
		t.Fatalf("main tracks %q, want only base.txt and feature.txt (worktree metadata leaked)", files)
	}
	gitIn(t, wt2.Dir, "add", ".")
	gitIn(t, wt2.Dir, "commit", "-qm", "feature two")
	args2, _ := json.Marshal(map[string]string{"worktree": res2.ID, "target_branch": "main", "expected_commit": tip})
	if _, err := integ.Run(ctx, "", args2); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("destination changed since tip; integrate must refuse: %v", err)
	}
	if _, err := os.Stat(wt2.Dir); err != nil {
		t.Fatal("a worktree was cleaned up automatically")
	}
	if _, err := (worktreeCleanupTool{agent: orch}).Run(ctx, "", json.RawMessage(`{"worktree":"/not/ours"}`)); err == nil {
		t.Fatal("cleanup of a foreign path must be refused")
	}
	out, err := worktreeStatusTool{agent: orch}.Run(ctx, "", nil)
	if err != nil || !strings.Contains(out, wt1.Dir) || !strings.Contains(out, wt2.Dir) {
		t.Fatalf("status = %q %v", out, err)
	}
}

// writeRoleCards sets minimal role cards (read/write/bash tools) for the test.
func writeRoleCards(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		setCard(t, "roles", id, card{Tools: []string{"read", "write", "bash"}, Prompt: "you are " + id, Body: id + " role"})
	}
}

func TestFeatureWriterDoesNotDelegateIntoItsWorktree(t *testing.T) {
	r, orch, _ := writerEnv(t)
	res := r.startChild(context.Background(), orch, Task{Task: "feature", Role: "worker"})
	if res.Status != "ok" {
		t.Fatalf("%+v", res)
	}
	writer := r.parkedKids[res.ID].agent
	if _, ok := writer.cfg.Tools.Get("spawn"); ok {
		t.Fatal("a feature writer must not spawn children into its own worktree")
	}
	// A worker spawned by anything else keeps the capability.
	plain := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: r.Tree, Spawn: r.Spawn,
		Node: r.Tree.Child(r.Tree.Root("root", "m", ""), "worker", "m", "")})
	if _, ok := plain.cfg.Tools.Get("spawn"); !ok {
		t.Fatal("ordinary workers should still be able to spawn")
	}
}
