package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

func twoReadCalls(idA, idB string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: idA, Name: "read", Args: `{"path":"a.txt"}`}},
		{Type: llm.EventToolCall, Seq: 1, Call: &llm.ToolCall{ID: idB, Name: "read", Args: `{"path":"b.txt"}`}},
		{Type: llm.EventDone, Stop: "tool_calls"},
	}
}

// Parallel calls with empty or repeated provider ids get unique ids, and the
// results pair with them.
func TestCallIDsAreUniquePerTurn(t *testing.T) {
	for _, ids := range [][2]string{{"", ""}, {"dup", "dup"}} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte("AAA"), 0o644)
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("BBB"), 0o644)
		fp := &fakeProvider{turns: [][]llm.Event{
			twoReadCalls(ids[0], ids[1]),
			{{Type: llm.EventTextDelta, TextDelta: "ok"}, {Type: llm.EventDone, Stop: "stop"}},
		}}
		a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...), Cwd: dir})
		if _, err := a.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		msgs := fp.requests[1].Messages
		calls := msgs[1].ToolCalls
		if len(calls) != 2 || calls[0].ID == calls[1].ID || calls[0].ID == "" || calls[1].ID == "" {
			t.Fatalf("ids %v → %+v", ids, calls)
		}
		if msgs[2].ToolCallID != calls[0].ID || !strings.Contains(msgs[2].Text, "AAA") ||
			msgs[3].ToolCallID != calls[1].ID || !strings.Contains(msgs[3].Text, "BBB") {
			t.Fatalf("results do not pair: %+v", msgs[2:])
		}
		if e, ok := a.arch.get(2); !ok || e.CallID != calls[1].ID || e.Tool != "read" || !strings.Contains(e.Args, "b.txt") {
			t.Fatalf("archive entry 2 = %+v", e)
		}
	}
}

// A renamed id drops the provider-native replay payload, which would still
// carry the old id.
func TestRenamedCallIDDropsNative(t *testing.T) {
	calls := []llm.ToolCall{{ID: "x"}, {ID: "x"}}
	if !uniqueCallIDs(calls, 3) || calls[1].ID != "ugh_3_1" {
		t.Fatalf("calls = %+v", calls)
	}
	if uniqueCallIDs([]llm.ToolCall{{ID: "a"}, {ID: "b"}}, 1) {
		t.Fatal("distinct ids must be left alone")
	}
}

// Ordinals count tool results only and survive compaction and resume.
func TestArchiveSurvivesCompactionAndResume(t *testing.T) {
	display := []session.Record{
		{Type: "message", Message: &llm.Message{Role: llm.RoleUser, Text: "q"}},
		{Type: "message", Message: &llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "k", Name: "grep", Args: `{"pattern":"x"}`}}}},
		{Type: "message", Message: &llm.Message{Role: llm.RoleTool, ToolCallID: "k", ToolName: "grep", Text: "match one"}},
		{Type: "compaction", Seed: []llm.Message{{Role: llm.RoleUser, Text: "summary"}}},
		{Type: "message", Message: &llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "k", Name: "read", Args: `{"path":"f"}`}}}},
		{Type: "message", Message: &llm.Message{Role: llm.RoleTool, ToolCallID: "k", ToolName: "read", Text: "file text"}},
	}
	a := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir(), Archive: display,
		History: []llm.Message{{Role: llm.RoleUser, Text: "summary"}}})
	if a.arch.count() != 2 || a.arch.next() != 3 {
		t.Fatalf("count=%d next=%d", a.arch.count(), a.arch.next())
	}
	e1, _ := a.arch.get(1)
	e2, _ := a.arch.get(2)
	if e1.Text != "match one" || e1.Args != `{"pattern":"x"}` || e2.Text != "file text" || e2.Tool != "read" {
		t.Fatalf("entries %+v %+v", e1, e2)
	}
	// Without records, history tool results still advance the counter.
	b := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir(),
		History: []llm.Message{{Role: llm.RoleUser, Text: "q"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "z", Name: "ls"}}},
			{Role: llm.RoleTool, ToolCallID: "z", Text: "r"}}})
	if b.arch.next() != 2 {
		t.Fatalf("next = %d", b.arch.next())
	}
}

// A trimmed bash result keeps its full text in the blob dir under its alias.
func TestBashBlobNamedByOrdinal(t *testing.T) {
	dir, blobs := t.TempDir(), filepath.Join(t.TempDir(), "s.blobs")
	fp := &fakeProvider{turns: [][]llm.Event{
		{{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "call_0", Name: "bash", Args: `{"command":"seq 1 2000; echo THE-END"}`}},
			{Type: llm.EventDone, Stop: "tool_calls"}},
		{{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "call_0", Name: "bash", Args: `{"command":"seq 1 3000; echo SECOND"}`}},
			{Type: llm.EventDone, Stop: "tool_calls"}},
		{{Type: llm.EventTextDelta, TextDelta: "ok"}, {Type: llm.EventDone, Stop: "stop"}},
	}}
	a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...), Cwd: dir, BlobDir: blobs})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// The provider reused "call_0" across turns; blobs must not collide.
	b1, err1 := os.ReadFile(filepath.Join(blobs, "c1.txt"))
	b2, err2 := os.ReadFile(filepath.Join(blobs, "c2.txt"))
	if err1 != nil || err2 != nil || !strings.Contains(string(b1), "THE-END") || !strings.Contains(string(b2), "SECOND") {
		t.Fatalf("blobs: %v %v", err1, err2)
	}
	if e, _ := a.arch.get(1); e.Blob == "" || !strings.HasSuffix(e.Blob, "c1.txt") {
		t.Fatalf("entry 1 = %+v", e)
	}
}

func writeFile(path, text string) error { return os.WriteFile(path, []byte(text), 0o644) }
