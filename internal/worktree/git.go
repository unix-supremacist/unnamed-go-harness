package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runGitCmd executes a git command in the specified directory.
func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	outStr := strings.TrimSpace(stdout.String())
	errStr := strings.TrimSpace(stderr.String())
	if err != nil {
		if errStr != "" {
			return outStr, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, errStr)
		}
		return outStr, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return outStr, nil
}

// isDirty checks if there are uncommitted changes (tracked or untracked) in dir.
func isDirty(ctx context.Context, dir string) (bool, error) {
	files, err := getUncommittedFiles(ctx, dir)
	if err != nil {
		return false, err
	}
	return len(files) > 0, nil
}

// getUncommittedFiles returns the list of changed files in dir according to git status,
// filtering out internal harness metadata files (.ugh-worktree.json, ugh-worktree.lock).
func getUncommittedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := runGitCmd(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var files []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// status output format: XY <path> or XY <old> -> <new>
		if len(line) > 3 {
			parts := strings.Split(line[3:], " -> ")
			fname := strings.TrimSpace(parts[len(parts)-1])
			if fname == ".ugh-session.jsonl" || fname == ".ugh-worktree.json" || fname == "ugh-worktree.lock" ||
				strings.HasSuffix(fname, "/.ugh-session.jsonl") || strings.HasSuffix(fname, "/.ugh-worktree.json") || strings.HasSuffix(fname, "/ugh-worktree.lock") {
				continue
			}
			files = append(files, fname)
		}
	}
	return files, nil
}

// getCommit returns the full commit hash for the given reference.
func getCommit(ctx context.Context, dir, ref string) (string, error) {
	out, err := runGitCmd(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// getCurrentBranch returns the current branch name of dir, or "HEAD" if detached.
func getCurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := runGitCmd(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	// Detached HEAD fallback
	return "HEAD", nil
}

// getCommitsBetween returns commit hashes in chronological order between base and target.
func getCommitsBetween(ctx context.Context, dir, base, target string) ([]string, error) {
	out, err := runGitCmd(ctx, dir, "rev-list", "--reverse", base+".."+target)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var commits []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// getConflictedFiles returns the list of files currently experiencing merge/cherry-pick conflicts.
func getConflictedFiles(ctx context.Context, dir string) []string {
	out, err := runGitCmd(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var files []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			files = append(files, l)
		}
	}
	return files
}

// createStashSnapshot generates a commit containing uncommitted changes without modifying
// HEAD, the working directory, or the reflog, using `git stash create`.
func createStashSnapshot(ctx context.Context, dir string) (string, error) {
	out, err := runGitCmd(ctx, dir, "stash", "create")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if commit == "" {
		// If git stash create outputs nothing, fallback to HEAD
		return getCommit(ctx, dir, "HEAD")
	}
	return commit, nil
}

// branchExists checks if a branch exists locally in the repository.
func branchExists(ctx context.Context, dir, branch string) bool {
	_, err := runGitCmd(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}
