package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preparedIntegration(t *testing.T) (string, *Worktree) {
	t.Helper()
	repo := initTestRepo(t)
	wt, err := Allocate(context.Background(), repo, AllocateOptions{Run: "regression", Agent: "root/1", Branch: "feature", WorktreeDir: filepath.Join(t.TempDir(), "wt")})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt.Dir, "feature.txt"), []byte("feature"), 0644)
	runCmd(t, wt.Dir, "git", "add", "feature.txt")
	runCmd(t, wt.Dir, "git", "commit", "-m", "feature")
	if err := wt.MarkCompleted("ready"); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

func TestIntegrationRechecksTargetAfterPermissionPrompt(t *testing.T) {
	repo, wt := preparedIntegration(t)
	gate := PermissionFunc(func(context.Context, string, string) error {
		os.WriteFile(filepath.Join(repo, "late.txt"), []byte("concurrent user change"), 0644)
		runCmd(t, repo, "git", "add", "late.txt")
		runCmd(t, repo, "git", "commit", "-m", "late change")
		return nil
	})
	_, err := Integrate(context.Background(), repo, IntegrateOptions{WorktreeDir: wt.Dir, TargetBranch: "master", PermGate: gate})
	if !errors.Is(err, ErrTargetCommitMismatch) {
		t.Fatalf("changed destination accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); !os.IsNotExist(err) {
		t.Fatal("feature integrated after destination changed")
	}
}

func TestIntegrationDoesNotCheckoutUnselectedDestination(t *testing.T) {
	repo, wt := preparedIntegration(t)
	runCmd(t, repo, "git", "branch", "other")
	_, err := Integrate(context.Background(), repo, IntegrateOptions{WorktreeDir: wt.Dir, TargetBranch: "other"})
	if !errors.Is(err, ErrTargetCommitMismatch) {
		t.Fatalf("unselected destination accepted: %v", err)
	}
	if strings.TrimSpace(runCmd(t, repo, "git", "branch", "--show-current")) != "master" {
		t.Fatal("integration changed selected branch")
	}
}

func TestIntegrationRejectsActiveProducerBeforeAndAfterApproval(t *testing.T) {
	repo, wt := preparedIntegration(t)
	wt.MarkActive()
	opts := IntegrateOptions{WorktreeDir: wt.Dir, TargetBranch: "master"}
	if _, err := Integrate(context.Background(), repo, opts); !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("active producer accepted: %v", err)
	}
	wt.MarkCompleted("ready")
	opts.PermGate = PermissionFunc(func(context.Context, string, string) error { return wt.MarkActive() })
	if _, err := Integrate(context.Background(), repo, opts); !errors.Is(err, ErrActiveWriter) {
		t.Fatalf("producer restarted during approval: %v", err)
	}
}

func TestCleanupPreservesUnmergedCommittedBranchWithoutForce(t *testing.T) {
	repo, wt := preparedIntegration(t)
	err := Cleanup(context.Background(), repo, wt.Dir, CleanupOptions{DeleteBranch: true})
	if err == nil {
		t.Fatal("unmerged branch deletion was not rejected")
	}
	if !branchExists(context.Background(), repo, "feature") {
		t.Fatal("unmerged commits were deleted")
	}
}
