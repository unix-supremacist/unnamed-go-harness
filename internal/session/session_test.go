package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/unix/ugh/internal/llm"
)

// Session files must round-trip: create → append messages → load returns
// the meta header and every message in order (resume correctness).
func TestCreateAppendLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := "/tmp/proj"
	store, err := Create(cwd, Record{Model: "opencode-go/m", Thinking: "low", System: "sys", Cwd: cwd, TS: "t0"})
	if err != nil {
		t.Fatal(err)
	}
	seedMsg := func(role llm.Role, text string) llm.Message { return llm.Message{Role: role, Text: text} }
	for _, m := range []llm.Message{
		seedMsg(llm.RoleUser, "hi"),
		{Role: llm.RoleAssistant, Text: "writing", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "write", Args: `{"path":"x"}`}}},
		{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "write", Text: "wrote"},
		seedMsg(llm.RoleAssistant, "done"),
	} {
		if err := store.Append(Record{Type: "message", Message: &m}); err != nil {
			t.Fatal(err)
		}
	}
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Meta.Model != "opencode-go/m" || loaded.Meta.Thinking != "low" || loaded.Meta.Cwd != cwd {
		t.Errorf("meta = %+v", loaded.Meta)
	}
	if len(loaded.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(loaded.Messages))
	}
	if loaded.Messages[1].ToolCalls[0].Name != "write" || loaded.Messages[2].ToolCallID != "c1" {
		t.Errorf("tool call round-trip = %+v", loaded.Messages[1])
	}

	// Open appends more to the same file (resume continues the session).
	store2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := seedMsg(llm.RoleUser, "again")
	if err := store2.Append(Record{Type: "message", Message: &m}); err != nil {
		t.Fatal(err)
	}
	store2.Close()
	loaded, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 5 || loaded.Messages[4].Text != "again" {
		t.Fatalf("after reopen: %d messages, last = %+v", len(loaded.Messages), loaded.Messages[4])
	}

	if got, err := Latest(cwd); err != nil || filepath.Base(got) != filepath.Base(path) {
		t.Errorf("Latest = %s, %v; want %s", got, err, path)
	}
	// a tore last line is tolerated
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"message"`)
	f.Close()
	if _, err := Load(path); err != nil {
		t.Errorf("tolerate torn tail: %v", err)
	}
}

func TestDirHashes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a, _ := Dir("/home/x/proj")
	b, _ := Dir("/home/x/other")
	if a == b {
		t.Error("different cwds must hash to different dirs")
	}
}

func TestInboxAppendAndIncrementalRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := SendInbox("r1", "root/worker-2", "first"); err != nil {
		t.Fatal(err)
	}
	p, _ := InboxPath("r1", "root/worker-2")
	msgs, off := ReadInbox(p, 0)
	if len(msgs) != 1 || msgs[0].Text != "first" {
		t.Fatalf("msgs = %+v", msgs)
	}
	// A partial line is not consumed until it is complete.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"text":"sec`)
	if msgs, off2 := ReadInbox(p, off); len(msgs) != 0 || off2 != off {
		t.Fatalf("partial read: %+v %d", msgs, off2)
	}
	f.WriteString("ond\"}\n")
	f.Close()
	if msgs, _ := ReadInbox(p, off); len(msgs) != 1 || msgs[0].Text != "second" {
		t.Fatalf("after completion: %+v", msgs)
	}
}

func TestListRootsExcludesChildrenBeforeCap(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := "/tmp/proj-roots"
	root, err := Create(cwd, Record{Model: "p/m", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	m := llm.Message{Role: llm.RoleUser, Text: "first   prompt\nhere"}
	_ = root.Append(Record{Type: "message", Message: &m})
	rootPath := root.Path()
	root.Close()
	for i := 0; i < 30; i++ { // newer children must not crowd the root out of a cap
		c, err := Create(cwd, Record{Model: "p/m", Cwd: cwd, Parent: "root", Agent: "root/1"})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	got, err := ListRoots(cwd, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != rootPath {
		t.Fatalf("roots = %+v", got)
	}
	if got[0].Title != "first prompt here" || got[0].Msgs != 1 {
		t.Fatalf("summary = %+v", got[0])
	}
	if p, err := Latest(cwd); err != nil || p != rootPath {
		t.Fatalf("Latest = %q, %v", p, err)
	}
}

func TestLoadDisplayKeepsCompactedHistory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := "/tmp/proj-display"
	s, _ := Create(cwd, Record{Model: "p/m", Cwd: cwd})
	for _, txt := range []string{"a", "b"} {
		m := llm.Message{Role: llm.RoleUser, Text: txt}
		_ = s.Append(Record{Type: "message", Message: &m})
	}
	_ = s.Append(Record{Type: "compaction", Seed: []llm.Message{{Role: llm.RoleUser, Text: "sum"}}})
	m := llm.Message{Role: llm.RoleUser, Text: "c"}
	_ = s.Append(Record{Type: "message", Message: &m})
	path := s.Path()
	s.Close()
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Messages) != 2 || len(l.Display) != 4 {
		t.Fatalf("messages=%d display=%d", len(l.Messages), len(l.Display))
	}
	if l.Display[2].Type != "compaction" || l.Display[0].Message.Text != "a" {
		t.Fatalf("display order wrong: %+v", l.Display)
	}
}
