package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

func instrTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk := func(p, body string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("AGENTS.md", "ROOT-RULES")
	mk("web/AGENTS.md", "WEB-RULES: use tabs")
	mk("web/app/AGENTS.md", "APP-RULES: no globals")
	mk("web/app/main.js", "old")
	mk("web/other.js", "x")
	mk("api/AGENTS.md", "API-RULES")
	mk("api/server.go", "package api")
	mk("plain/file.txt", "no rules here")
	return dir
}

func instrAgent(t *testing.T, dir string, fp llm.Provider) *Agent {
	t.Helper()
	return New(Config{Provider: fp, Model: "m", Cwd: dir, Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), Context: ContextConfig{Instructions: true}})
}

func edit(path, old, nw string) llm.ToolCall {
	return llm.ToolCall{Name: "edit", Args: `{"path":"` + path + `","old_text":"` + old + `","new_text":"` + nw + `"}`}
}

func TestRootInstructionsJoinSystemPrompt(t *testing.T) {
	dir := instrTree(t)
	a := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: dir, System: "BASE", Context: ContextConfig{Instructions: true}})
	if !strings.Contains(a.cfg.System, "BASE") || !strings.Contains(a.cfg.System, "ROOT-RULES") {
		t.Fatalf("system = %q", a.cfg.System)
	}
	off := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: dir, System: "BASE"})
	if off.cfg.System != "BASE" {
		t.Fatalf("instructions off but system = %q", off.cfg.System)
	}
	os.Remove(filepath.Join(dir, "AGENTS.md"))
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("CLAUDE-RULES"), 0o644)
	b := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: dir, System: "BASE", Context: ContextConfig{Instructions: true}})
	if !strings.Contains(b.cfg.System, "CLAUDE-RULES") {
		t.Fatalf("fallback ignored: %q", b.cfg.System)
	}
}

// A first edit into a directory whose instructions were never shown does not
// run; the result carries them; re-issuing the same call then works.
func TestFirstMutationDefersToUnseenInstructions(t *testing.T) {
	dir := instrTree(t)
	a := instrAgent(t, dir, &fakeProvider{})
	out, stop, err := a.execute(context.Background(), edit("web/app/main.js", "old", "new"))
	if err != nil || stop {
		t.Fatal(err, stop)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "web/app/main.js"))
	if string(b) != "old" {
		t.Fatal("edit ran despite unseen instructions")
	}
	for _, want := range []string{"not executed", "web/AGENTS.md", "WEB-RULES", "web/app/AGENTS.md", "APP-RULES", "[Instructions from web/AGENTS.md]", "take precedence"} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "WEB-RULES") > strings.Index(out, "APP-RULES") {
		t.Error("more specific instructions must come later")
	}
	if strings.Contains(out, "ROOT-RULES") {
		t.Error("the cwd's own file belongs to the system prompt, not to each result")
	}
	out, _, _ = a.execute(context.Background(), edit("web/app/main.js", "old", "new"))
	b, _ = os.ReadFile(filepath.Join(dir, "web/app/main.js"))
	if string(b) != "new" || strings.Contains(out, "not executed") {
		t.Fatalf("re-issued edit failed: %q file=%q", out, b)
	}
	// Another file in a seen directory needs no re-announcement.
	out, _, _ = a.execute(context.Background(), edit("web/other.js", "x", "y"))
	if strings.Contains(out, "not executed") {
		t.Fatalf("seen instructions announced again: %s", out)
	}
}

func TestReadExecutesAndCarriesInstructions(t *testing.T) {
	dir := instrTree(t)
	a := instrAgent(t, dir, &fakeProvider{})
	out, _, _ := a.execute(context.Background(), llm.ToolCall{Name: "read", Args: `{"path":"api/server.go"}`})
	if !strings.Contains(out, "package api") || !strings.Contains(out, "[Instructions from api/AGENTS.md]\nAPI-RULES") {
		t.Fatalf("read = %q", out)
	}
	out, _, _ = a.execute(context.Background(), llm.ToolCall{Name: "read", Args: `{"path":"api/server.go"}`})
	if strings.Contains(out, "API-RULES") {
		t.Fatalf("announced twice: %q", out)
	}
	// An unrelated directory gets nothing.
	out, _, _ = a.execute(context.Background(), llm.ToolCall{Name: "read", Args: `{"path":"plain/file.txt"}`})
	if strings.Contains(out, "Instructions") {
		t.Fatalf("plain: %q", out)
	}
}

func TestSiblingDirectoriesAreIndependent(t *testing.T) {
	dir := instrTree(t)
	a := instrAgent(t, dir, &fakeProvider{})
	web, _, _ := a.execute(context.Background(), edit("web/other.js", "x", "y"))
	api, _, _ := a.execute(context.Background(), edit("api/server.go", "package", "package"))
	if !strings.Contains(web, "WEB-RULES") || strings.Contains(web, "API-RULES") ||
		!strings.Contains(api, "API-RULES") || strings.Contains(api, "WEB-RULES") {
		t.Fatalf("web=%q api=%q", web, api)
	}
}

func TestOutsideCwdAndEscapingSymlinksGetNothing(t *testing.T) {
	dir := instrTree(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("OUTSIDE-RULES"), 0o644)
	os.WriteFile(filepath.Join(outside, "f.txt"), []byte("z"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// An AGENTS.md that is itself a symlink out of the tree.
	os.MkdirAll(filepath.Join(dir, "sneaky"), 0o755)
	os.Symlink(filepath.Join(outside, "AGENTS.md"), filepath.Join(dir, "sneaky", "AGENTS.md"))
	os.WriteFile(filepath.Join(dir, "sneaky", "g.txt"), []byte("g"), 0o644)
	a := instrAgent(t, dir, &fakeProvider{})
	for _, p := range []string{filepath.Join(outside, "f.txt"), "link/f.txt", "../" + filepath.Base(outside) + "/f.txt", "sneaky/g.txt"} {
		if got := a.unseenInstructions(p); len(got) != 0 {
			t.Errorf("%s got instructions %+v", p, got)
		}
	}
	// A new file in a not-yet-existing subdirectory still resolves through its parent.
	if got := a.unseenInstructions("web/app/new/deeper/file.js"); len(got) != 2 {
		t.Errorf("nonexistent target: %+v", got)
	}
}

func TestEditedInstructionFileBecomesUnseenAgain(t *testing.T) {
	dir := instrTree(t)
	a := instrAgent(t, dir, &fakeProvider{})
	a.execute(context.Background(), llm.ToolCall{Name: "read", Args: `{"path":"api/server.go"}`})
	p := filepath.Join(dir, "api/AGENTS.md")
	os.WriteFile(p, []byte("API-RULES v2"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(p, later, later)
	out, _, _ := a.execute(context.Background(), llm.ToolCall{Name: "read", Args: `{"path":"api/server.go"}`})
	if !strings.Contains(out, "API-RULES v2") {
		t.Fatalf("edited file not re-shown: %q", out)
	}
}

// A read+edit batch stays contiguous and passes reconcileHistory unchanged.
func TestBatchStaysContiguous(t *testing.T) {
	dir := instrTree(t)
	fp := &fakeProvider{turns: [][]llm.Event{
		{
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "r", Name: "read", Args: `{"path":"web/other.js"}`}},
			{Type: llm.EventToolCall, Seq: 1, Call: &llm.ToolCall{ID: "e1", Name: "edit", Args: `{"path":"web/other.js","old_text":"x","new_text":"y"}`}},
			{Type: llm.EventToolCall, Seq: 2, Call: &llm.ToolCall{ID: "e2", Name: "edit", Args: `{"path":"web/app/main.js","old_text":"old","new_text":"new"}`}},
			{Type: llm.EventDone, Stop: "tool_calls"},
		},
		{{Type: llm.EventTextDelta, TextDelta: "done"}, {Type: llm.EventDone, Stop: "stop"}},
	}}
	a := instrAgent(t, dir, fp)
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	h := a.History()
	// user, assistant(3 calls), 3 results, assistant
	if len(h) != 6 || h[2].ToolCallID != "r" || h[3].ToolCallID != "e1" || h[4].ToolCallID != "e2" {
		t.Fatalf("history shape: %d messages", len(h))
	}
	if r := reconcileHistory(h); len(r) != len(h) {
		t.Fatalf("reconcile added stubs: %d → %d", len(h), len(r))
	}
	// The read showed web's rules, so the first edit ran; the second (app/) deferred.
	b, _ := os.ReadFile(filepath.Join(dir, "web/other.js"))
	m, _ := os.ReadFile(filepath.Join(dir, "web/app/main.js"))
	if string(b) != "y" || string(m) != "old" || !strings.Contains(h[4].Text, "APP-RULES") || !strings.Contains(h[2].Text, "WEB-RULES") {
		t.Fatalf("other=%q main=%q\n%s", b, m, h[4].Text)
	}
}

// Resume rebuilds the seen set from the archive; compaction keeps the rules.
func TestResumeAndCompactionKeepInstructions(t *testing.T) {
	dir := instrTree(t)
	first := instrAgent(t, dir, &fakeProvider{})
	read, _, _ := first.execute(context.Background(), llm.ToolCall{Name: "read", Args: `{"path":"web/app/main.js"}`})
	hist := []llm.Message{
		{Role: llm.RoleUser, Text: "q"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "r", Name: "read", Args: `{"path":"web/app/main.js"}`}}},
		{Role: llm.RoleTool, ToolCallID: "r", ToolName: "read", Text: read},
	}
	var display []session.Record
	for i := range hist {
		display = append(display, session.Record{Type: "message", Message: &hist[i]})
	}
	b := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: dir, History: hist, Archive: display,
		Tools: tools.NewRegistry(tools.All()...), Context: ContextConfig{Instructions: true}})
	if got := b.unseenInstructions("web/app/main.js"); len(got) != 0 {
		t.Fatalf("resumed agent re-announces: %+v", got)
	}
	var seed []llm.Message
	b.cfg.OnCompact = func(h []llm.Message) { seed = h }
	b.ApplyCompaction([]llm.Message{{Role: llm.RoleUser, Text: "SUMMARY"}}, llm.Usage{})
	if len(seed) != 1 || !strings.Contains(seed[0].Text, "SUMMARY") || !strings.Contains(seed[0].Text, "APP-RULES") ||
		!strings.Contains(seed[0].Text, "WEB-RULES") {
		t.Fatalf("manual compaction dropped the instructions: %q", seed[0].Text)
	}
	// Auto-compaction seed too.
	text := "Original task:\nx\n\nSummary:\nS" + b.instructionTail()
	if !strings.Contains(text, "[Instructions from web/app/AGENTS.md]") {
		t.Fatal("tail lacks the marker")
	}
}

func TestInstructionsOffMeansNothingChanges(t *testing.T) {
	dir := instrTree(t)
	a := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: dir, Tools: tools.NewRegistry(tools.All()...)})
	out, _, _ := a.execute(context.Background(), edit("web/other.js", "x", "y"))
	b, _ := os.ReadFile(filepath.Join(dir, "web/other.js"))
	if string(b) != "y" || strings.Contains(out, "not executed") {
		t.Fatalf("instructions=false still gated: %q", out)
	}
}
