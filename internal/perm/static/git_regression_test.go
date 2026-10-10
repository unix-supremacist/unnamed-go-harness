package static

import (
	"testing"

	"github.com/unix/ugh/internal/perm"
)

func TestGitConfigValuesAreNotActions(t *testing.T) {
	cwd := t.TempDir()
	for _, value := range []string{"get", "list", "set", "unset", "edit", "rename-section", "remove-section"} {
		for _, prefix := range []string{"git config user.name ", "git config --add user.name ", "git config set user.name "} {
			command := prefix + value
			t.Run(command, func(t *testing.T) {
				tier, _ := Triage(perm.Call{Tool: "bash", Cwd: cwd, Command: command})
				if tierRank[tier] < tierRank[Middle] {
					t.Fatalf("mutating config was classified %s", tier)
				}
			})
		}
	}
}

func TestGitExternalExecutionAndArchiveNetwork(t *testing.T) {
	cwd := t.TempDir()
	for _, command := range []string{
		"git archive --remote=ssh://example.test/repo HEAD",
		"git diff --textconv",
		"git cat-file --filters HEAD:README.md",
		"git grep --open-files-in-pager=sh needle",
		"git grep -Osh needle",
	} {
		t.Run(command, func(t *testing.T) {
			tier, _ := Triage(perm.Call{Tool: "bash", Cwd: cwd, Command: command})
			if tier != High {
				t.Fatalf("external execution/network classified %s", tier)
			}
		})
	}
}
