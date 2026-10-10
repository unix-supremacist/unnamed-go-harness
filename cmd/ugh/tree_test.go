package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
)

func TestTreeFromRunLinkedSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mk := func(cwd string, meta session.Record, result *session.Record) string {
		s, err := session.Create(cwd, meta)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		m := llm.Message{Role: llm.RoleUser, Text: "task"}
		_ = s.Append(session.Record{Type: "message", Message: &m})
		if result != nil {
			_ = s.Append(*result)
		}
		return s.Path()
	}
	run := "20261006-120000-abc"
	root := mk("/a", session.Record{Model: "opencode-go/mimo-v2.6-pro", Run: run},
		&session.Record{Type: "result", Status: "ok", Turns: 30, Usage: &llm.Usage{In: 120000, Out: 8000}})
	mk("/a", session.Record{Model: "opencode-go/glm-5.3", Run: run, Agent: "root/worker-1", Parent: "root"},
		&session.Record{Type: "result", Status: "turn-out", Turns: 80, Model: "openrouter/z-ai/glm-5.3"})
	mk("/b", session.Record{Model: "opencode-go/qwen3.8-flash", Run: run, Agent: "root/worker-1/scout-2", Parent: "root/worker-1"}, nil)
	mk("/a", session.Record{Model: "x/other", Run: "another-run"}, nil)

	base, _ := session.StateDir()
	paths, _ := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
	nodes, err := runNodes(paths, run)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := printTree(&b, run, nodes); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"run 20261006-120000-abc · 3 agents",
		"\nroot  opencode-go/mimo-v2.6-pro  ok  turns 30  tok 120.0k/8.0k  " + root,
		"\n  root/worker-1  openrouter/z-ai/glm-5.3  turn-out  turns 80",
		"\n    root/worker-1/scout-2  opencode-go/qwen3.8-flash  unfinished  turns 0",
		"1 ok · 1 turn-out · 1 unfinished",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "x/other") {
		t.Errorf("other run leaked into the tree:\n%s", out)
	}
}
