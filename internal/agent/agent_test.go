package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/tools"
)

// fakeProvider replays scripted turns and records each request.
type fakeProvider struct {
	turns    [][]llm.Event
	requests []llm.Request
}

func (f *fakeProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	f.requests = append(f.requests, req)
	if len(f.turns) == 0 {
		return nil, errors.New("fake: no scripted turns left")
	}
	evs := f.turns[0]
	f.turns = f.turns[1:]
	ch := make(chan llm.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestRunExecutesToolAndContinues(t *testing.T) {
	dir := t.TempDir()
	fp := &fakeProvider{turns: [][]llm.Event{
		{
			{Type: llm.EventThinkingDelta, Thinking: "need to write"},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c1", Name: "write"}},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{Args: `{"path":"hello.txt",`}},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{Args: `"content":"hi"}`}},
			{Type: llm.EventUsage, Usage: &llm.Usage{In: 10, Out: 5}},
			{Type: llm.EventDone, Stop: "tool_calls"},
		},
		{
			{Type: llm.EventTextDelta, TextDelta: "done"},
			{Type: llm.EventUsage, Usage: &llm.Usage{In: 20, Out: 2, Cached: 3}},
			{Type: llm.EventDone, Stop: "stop"},
		},
	}}
	out := &strings.Builder{}
	a := New(Config{
		Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...),
		Cwd: dir, Out: out, Trace: &strings.Builder{},
	})
	final, err := a.Run(context.Background(), "create hello.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final != "done" {
		t.Errorf("final = %q, want done", final)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("tool did not run: %q err=%v", data, err)
	}
	if got := out.String(); got != "done\n" {
		t.Errorf("stdout = %q", got)
	}
	if a.Usage.In != 30 || a.Usage.Out != 7 || a.Usage.Cached != 3 {
		t.Errorf("usage = %+v", a.Usage)
	}
	if len(fp.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fp.requests))
	}
	// The second request carries the assistant tool call and its result.
	msgs := fp.requests[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("second request messages = %+v", msgs)
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Args != `{"path":"hello.txt","content":"hi"}` {
		t.Errorf("assistant message = %+v", msgs[1])
	}
	if msgs[2].Role != llm.RoleTool || msgs[2].ToolCallID != "c1" || !strings.Contains(msgs[2].Text, "wrote") {
		t.Errorf("tool message = %+v", msgs[2])
	}
	if len(fp.requests[0].Tools) != 7 {
		t.Errorf("tools = %d, want 7", len(fp.requests[0].Tools))
	}
}

func TestRunStopsWithoutToolCalls(t *testing.T) {
	fp := &fakeProvider{turns: [][]llm.Event{{
		{Type: llm.EventTextDelta, TextDelta: "hi"},
		{Type: llm.EventDone, Stop: "stop"},
	}}}
	a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...), Cwd: t.TempDir()})
	final, err := a.Run(context.Background(), "hi")
	if err != nil || final != "hi" {
		t.Fatalf("Run = %q, %v", final, err)
	}
}

func TestRunUnknownTool(t *testing.T) {
	dir := t.TempDir()
	fp := &fakeProvider{turns: [][]llm.Event{
		{{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c1", Name: "nope", Args: "{}"}}},
		{{Type: llm.EventTextDelta, TextDelta: "recovered"}, {Type: llm.EventDone, Stop: "stop"}},
	}}
	a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...), Cwd: dir})
	if final, err := a.Run(context.Background(), "x"); err != nil || final != "recovered" {
		t.Fatalf("Run = %q, %v", final, err)
	}
	if msg := fp.requests[1].Messages[2]; !strings.Contains(msg.Text, "unknown tool") {
		t.Errorf("tool result = %+v", msg)
	}
}

func TestRunTurnBudget(t *testing.T) {
	fp := &fakeProvider{turns: [][]llm.Event{
		{{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c1", Name: "ls", Args: "{}"}}},
		{{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c2", Name: "ls", Args: "{}"}}},
	}}
	a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...), Cwd: t.TempDir(), MaxTurns: 2})
	if _, err := a.Run(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "2 turns") {
		t.Fatalf("err = %v, want turn budget error", err)
	}
}

func TestRunPropagatesStreamError(t *testing.T) {
	fp := &fakeProvider{turns: [][]llm.Event{{{Type: llm.EventError, Err: errors.New("boom")}}}}
	a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...), Cwd: t.TempDir()})
	if _, err := a.Run(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
}

// gateStub is a scripted permission gate.
type gateStub struct {
	action string
	reason string
	calls  int
	last   perm.Call
}

func (g *gateStub) Check(_ context.Context, c perm.Call) perm.Outcome {
	g.calls++
	g.last = c
	return perm.Outcome{Action: g.action, Reason: g.reason}
}

// The gate must receive bash's command string (the static classifier judges
// real argv, not the raw args blob).
func TestGateReceivesBashCommand(t *testing.T) {
	g := &gateStub{action: perm.Allow}
	dir := t.TempDir()
	fp := &fakeProvider{turns: [][]llm.Event{
		{{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c1", Name: "bash", Args: `{"command":"rm -rf build"}`}},
			{Type: llm.EventDone, Stop: "tool_calls"}},
		{{Type: llm.EventTextDelta, TextDelta: "ok"}, {Type: llm.EventDone, Stop: "stop"}},
	}}
	a := New(Config{Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...),
		Cwd: dir, Gate: g})
	if _, err := a.Run(context.Background(), "clean"); err != nil {
		t.Fatal(err)
	}
	if g.last.Command != "rm -rf build" || g.last.Tool != "bash" || g.last.Cwd != dir {
		t.Errorf("gate call = %+v, want bash command in cwd", g.last)
	}
}

// scriptedRun drives one write-tool turn behind the given gate and returns
// the provider, the agent and the run error.
func scriptedRun(t *testing.T, gate perm.Gate) (*fakeProvider, *Agent, error) {
	t.Helper()
	dir := t.TempDir()
	fp := &fakeProvider{turns: [][]llm.Event{
		{
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c1", Name: "write"}},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{Args: `{"path":"hello.txt",`}},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{Args: `"content":"hi"}`}},
			{Type: llm.EventDone, Stop: "tool_calls"},
		},
		{{Type: llm.EventTextDelta, TextDelta: "ok"}, {Type: llm.EventDone, Stop: "stop"}},
	}}
	a := New(Config{
		Provider: fp, Model: "m", Tools: tools.NewRegistry(tools.All()...),
		Cwd: dir, Out: &strings.Builder{}, Trace: &strings.Builder{}, Gate: gate,
	})
	_, err := a.Run(context.Background(), "write hello.txt")
	return fp, a, err
}

// A denied tool gives the model feedback and the run continues.
func TestGateDenyFeedsModelAndContinues(t *testing.T) {
	g := &gateStub{action: perm.Deny, reason: "denied by user"}
	fp, a, err := scriptedRun(t, g)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g.calls == 0 {
		t.Fatal("gate never consulted")
	}
	if _, statErr := os.Stat(filepath.Join(a.cfg.Cwd, "hello.txt")); statErr == nil {
		t.Fatal("denied tool still ran")
	}
	if len(fp.requests) != 2 {
		t.Fatalf("provider saw %d turns, want 2 (denial fed back)", len(fp.requests))
	}
	msgs := fp.requests[1].Messages
	toolMsg := msgs[len(msgs)-1]
	if toolMsg.Role != llm.RoleTool || !strings.Contains(toolMsg.Text, "permission denied") {
		t.Errorf("tool result = %+v, want permission-denied feedback", toolMsg)
	}
}

// A stop halts the entire run: the tool never runs and no further turn happens.
func TestGateStopHaltsWholeRun(t *testing.T) {
	g := &gateStub{action: perm.Stop, reason: "static_normal: recursive rm on /"}
	fp, a, err := scriptedRun(t, g)
	if !errors.Is(err, perm.ErrStopped) {
		t.Fatalf("err = %v, want perm.ErrStopped", err)
	}
	if len(fp.requests) != 1 {
		t.Errorf("provider saw %d turns after stop, want 1", len(fp.requests))
	}
	if _, statErr := os.Stat(filepath.Join(a.cfg.Cwd, "hello.txt")); statErr == nil {
		t.Fatal("stopped tool still ran")
	}
}

// An allow (or no gate) runs the tool as before.
func TestGateAllowRunsTool(t *testing.T) {
	g := &gateStub{action: perm.Allow}
	_, a, err := scriptedRun(t, g)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.Cwd, "hello.txt")); err != nil {
		t.Errorf("allowed tool did not run: %v", err)
	}
	_, _, err = scriptedRun(t, nil)
	if err != nil {
		t.Fatalf("Run without gate: %v", err)
	}
}

// TestToollessAgentRuns covers the /compact summarizer shape: an agent with
// no tool registry must run without dereferencing nil (regression: /compact
// used to panic the TUI).
func TestToollessAgentRuns(t *testing.T) {
	fp := &fakeProvider{turns: [][]llm.Event{{
		{Type: llm.EventTextDelta, TextDelta: "summary"},
		{Type: llm.EventDone, Stop: "stop"},
	}}}
	a := New(Config{Provider: fp, Model: "m", Cwd: t.TempDir()})
	final, err := a.Run(context.Background(), "summarize")
	if err != nil || final != "summary" {
		t.Fatalf("Run = %q, %v", final, err)
	}
}

// specChanger declares a different schema after its first call, like spawn does
// when routing flips to traditional selection mid-run.
type specChanger struct{ n *int }

func (s specChanger) Spec() llm.ToolSpec {
	name := "probe_a"
	if *s.n > 0 {
		name = "probe_b"
	}
	return llm.ToolSpec{Name: name}
}
func (s specChanger) Run(context.Context, string, json.RawMessage) (string, error) {
	*s.n++
	return "ok", nil
}

func TestToolSpecsAreRefreshedEveryTurn(t *testing.T) {
	n := 0
	p := &funcProvider{script: func(turn int, _ llm.Request) []llm.Event {
		if turn == 1 {
			return []llm.Event{call("c1", "probe_a", "{}")}
		}
		return []llm.Event{say("done")}
	}}
	a := New(Config{Provider: p, Model: "m", Cwd: t.TempDir(), Tools: tools.NewRegistry(specChanger{&n})})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 || p.reqs[0].Tools[0].Name != "probe_a" || p.reqs[1].Tools[0].Name != "probe_b" {
		t.Fatalf("second turn did not see the refreshed schema: %+v", p.reqs)
	}
}
