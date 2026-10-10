package main

import (
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
)

func TestWatchRendersLiveStatusAndActivity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	run := "r1"
	add := func(meta session.Record, msgs ...llm.Message) *session.Store {
		meta.Run = run
		s, err := session.Create("/w", meta)
		if err != nil {
			t.Fatal(err)
		}
		for i := range msgs {
			_ = s.Append(session.Record{Type: "message", Message: &msgs[i]})
		}
		return s
	}
	user := llm.Message{Role: llm.RoleUser, Text: "do it"}
	root := add(session.Record{Model: "opencode-go/mimo-v2.6-pro"}, user,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "spawn", Args: `{"tasks":[{"task":"a"},{"task":"b"}]}`}}})
	add(session.Record{Agent: "root/worker-1", Parent: "root", Model: "codex/gpt-6.1-sol"}, user,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "bash", Args: `{"command":"go test ./..."}`}}})
	add(session.Record{Agent: "root/worker-2", Parent: "root", Model: "opencode-go/glm-5.3"}, user,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "read", Args: `{"path":"a.go"}`}}},
		llm.Message{Role: llm.RoleTool, ToolName: "read", Text: "package a\n\nfunc A() {}"})
	done := add(session.Record{Agent: "root/worker-2/scout-3", Parent: "root/worker-2", Model: "x/flash"}, user)
	_ = done.Append(session.Record{Type: "result", Status: "turn-out", Turns: 2})
	add(session.Record{Agent: "root/worker-4", Parent: "root", Model: "x/flash"})
	other, _ := session.Create("/w", session.Record{Agent: "other/x", Run: "r2"})
	other.Close()
	_ = root

	tails := map[string]*tail{}
	for _, p := range runFiles(run, time.Time{}) {
		tails[p] = &tail{path: p}
		if err := tails[p].poll(nil); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	renderWatch(&b, run, tails, time.Now(), 200)
	out := b.String()
	if !strings.Contains(out, "1 thinking · 1 tool · 1 waiting · 1 queued · 1 turn-out") {
		t.Errorf("summary line wrong:\n%s", out)
	}
	// id → expected status, and the activity line under it ("" = none).
	for id, want := range map[string][2]string{
		"root":                  {"waiting", "→ spawn 2 tasks"},
		"root/worker-1":         {"tool", "→ bash go test ./..."},
		"root/worker-2":         {"thinking", "after read: package a func A() {}"},
		"root/worker-2/scout-3": {"turn-out", ""},
		"root/worker-4":         {"queued", ""},
	} {
		lines := strings.Split(out, "\n")
		found := false
		for i, l := range lines {
			f := strings.Fields(l)
			if len(f) < 2 || f[0] != id {
				continue
			}
			found = true
			if f[1] != want[0] {
				t.Errorf("%s status = %s, want %s", id, f[1], want[0])
			}
			if want[1] != "" && (i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != want[1]) {
				t.Errorf("%s activity = %q, want %q", id, lines[i+1], want[1])
			}
		}
		if !found {
			t.Errorf("%s missing from:\n%s", id, out)
		}
	}
	if strings.Contains(out, "other/x") {
		t.Error("another run leaked in")
	}
}

func TestTailReadsIncrementallyAcrossPartialLines(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, err := session.Create("/w", session.Record{Run: "r"})
	if err != nil {
		t.Fatal(err)
	}
	tl := &tail{path: s.Path()}
	n := 0
	count := func(session.Record) { n++ }
	_ = tl.poll(count)
	m := llm.Message{Role: llm.RoleAssistant, Text: "hi"}
	_ = s.Append(session.Record{Type: "message", Message: &m})
	_ = tl.poll(count)
	_ = tl.poll(count) // nothing new
	if n != 2 || tl.turns != 1 {
		t.Fatalf("records = %d, turns = %d", n, tl.turns)
	}
}

func TestPrintRecordTruncatesToolOutput(t *testing.T) {
	var b strings.Builder
	m := llm.Message{Role: llm.RoleTool, ToolName: "bash", Text: "1\n2\n3\n4\n5\n6\n7\n8"}
	printRecord(&b, session.Record{Type: "message", Message: &m}, false)
	if !strings.Contains(b.String(), "← bash:\n    1\n") || !strings.Contains(b.String(), "(2 more lines, --full shows all)") {
		t.Errorf("out = %q", b.String())
	}
}

func TestParseInterleavedFlagsAfterPositional(t *testing.T) {
	for _, args := range [][]string{{"root/worker-3", "-f"}, {"-f", "root/worker-3"}, {"root/worker-3", "-f", "--full"}} {
		fs := flag.NewFlagSet("log", flag.ContinueOnError)
		follow := fs.Bool("f", false, "")
		fs.Bool("full", false, "")
		pos, err := parseInterleaved(fs, args)
		if err != nil || len(pos) != 1 || pos[0] != "root/worker-3" || !*follow {
			t.Errorf("%v → pos=%v follow=%v err=%v", args, pos, *follow, err)
		}
	}
}

func TestSendTargetsRunningAgentsAndSubtrees(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mk := func(agent, parent string, done bool) string {
		s, _ := session.Create("/w", session.Record{Run: "r9", Agent: agent, Parent: parent})
		if done {
			_ = s.Append(session.Record{Type: "result", Status: "ok"})
		}
		s.Close()
		return s.Path()
	}
	rootFile := mk("", "", false)
	mk("root/worker-1", "root", false)
	mk("root/worker-1/scout-2", "root/worker-1", false)
	mk("root/worker-1/scout-3", "root/worker-1", true)
	if err := sendCmd([]string{"--session", rootFile, "--subtree", "root/worker-1", "check", "the", "blocks"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"root/worker-1": 1, "root/worker-1/scout-2": 1, "root/worker-1/scout-3": 0, "root": 0} {
		p, _ := session.InboxPath("r9", id)
		msgs, _ := session.ReadInbox(p, 0)
		if len(msgs) != want || (want == 1 && msgs[0].Text != "check the blocks") {
			t.Errorf("%s inbox = %+v", id, msgs)
		}
	}
	if err := sendCmd([]string{"--session", rootFile, "root/worker-1/scout-3", "hi"}); err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Errorf("finished agent: %v", err)
	}
}

func TestWatchShowsWaitingOnChildren(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := session.Create("/w", session.Record{Run: "rw", Agent: "root/worker-139", Parent: "root"})
	m := llm.Message{Role: llm.RoleAssistant, Text: "final report written"}
	_ = s.Append(session.Record{Type: "message", Message: &m})
	_ = s.Append(session.Record{Type: "status", Status: "waiting on root/worker-139/scout-142"})
	s.Close()
	tl := &tail{path: s.Path()}
	_ = tl.poll(nil)
	if tl.status() != "waiting" || tl.activity() != "waiting on root/worker-139/scout-142" {
		t.Fatalf("status=%q activity=%q", tl.status(), tl.activity())
	}
}
