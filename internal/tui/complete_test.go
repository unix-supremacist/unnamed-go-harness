package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
)

func TestCompToken(t *testing.T) {
	if k, at := compToken("/mod"); k != "cmd" || at != -1 {
		t.Fatalf("cmd token: %q %d", k, at)
	}
	if k, _ := compToken("/model gpt"); k != "" {
		t.Fatalf("slash with space is not a command token: %q", k)
	}
	if k, at := compToken("fix @internal/comp"); k != "file" || at != 4 {
		t.Fatalf("file token: %q %d", k, at)
	}
	if k, _ := compToken("done @a b"); k != "" {
		t.Fatalf("token with space is not completable: %q", k)
	}
}

func TestCandidatesForSlash(t *testing.T) {
	cands := candidatesFor("cmd", ".", "/comp", -1)
	if len(cands) != 2 || cands[0] != "/compact" || cands[1] != "/compact-model" {
		t.Fatalf("slash candidates for /co: %v", cands)
	}
}

func TestCandidatesForFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644)
	cands := candidatesFor("file", dir, "@m", 0)
	if len(cands) != 1 || cands[0] != "main.go" {
		t.Fatalf("file candidates: %v", cands)
	}
	cands = candidatesFor("file", dir, "@", 0)
	if len(cands) != 2 { // .hidden is skipped for an empty prefix
		t.Fatalf("dir candidates: %v", cands)
	}
	if !strings.HasSuffix(cands[1], "sub/") {
		t.Fatalf("directories need a trailing slash: %v", cands)
	}
}

func TestTabCompleteCycle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "ab.go"), []byte("x"), 0o644)
	s := &State{}
	s.initTheme("dark")
	s.cwd = dir
	s.ta = textarea.New()
	s.ta.SetValue("@a")
	s.tabComplete() // completes the first candidate "a.go"
	if v := s.ta.Value(); v != "@a.go" {
		t.Fatalf("first completion = %q", v)
	}
	s.tabComplete() // same seed → cycles to "ab.go"
	if v := s.ta.Value(); v != "@ab.go" {
		t.Fatalf("cycled completion = %q", v)
	}
}

func TestTabCompleteNoMatchesDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644)
	for _, in := range []string{"look at @nosuchfile", "@missing/dir/x", "/nosuchcommand"} {
		s := &State{}
		s.initTheme("dark")
		s.cwd = dir
		s.ta = textarea.New()
		s.ta.SetValue(in)
		s.tabComplete()
		s.tabComplete() // a second tab on the same input must not panic either
		if v := s.ta.Value(); v != in {
			t.Errorf("%q changed to %q", in, v)
		}
		if !strings.Contains(s.compHint, "no matches") {
			t.Errorf("%q: hint = %q", in, s.compHint)
		}
	}
}

func TestCommandTableAliases(t *testing.T) {
	for alias, want := range map[string]string{
		"/clear": "/new", "/effort": "/thinking", "/continue": "/resume", "/models": "/model",
		"/cost": "/usage", "/quota": "/usage", "/approvals": "/mode", "/agents": "/tree",
		"/summarize": "/compact", "/h": "/help", "/?": "/help", "/exit": "/quit", "/status": "/routing",
	} {
		name, arg, ok := splitCommand(alias + " High Level")
		if !ok || name != want || arg != "High Level" {
			t.Errorf("%s → %q %q %v, want %s with args kept verbatim", alias, name, arg, ok, want)
		}
	}
	if _, _, ok := splitCommand("/nope"); ok {
		t.Error("unknown command resolved")
	}
	help := helpText()
	for _, c := range cmdTable {
		if !strings.Contains(help, c.name) {
			t.Errorf("help omits %s", c.name)
		}
	}
	found := map[string]bool{}
	for _, n := range slashNames() {
		found[n] = true
	}
	if !found["/clear"] || !found["/new"] {
		t.Error("completion omits aliases")
	}
}
