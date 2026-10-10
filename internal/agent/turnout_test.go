package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/tools"
)

// loopProvider calls `ls` every turn. With answerWhenNudged it replies in
// text once the turn-out nudge is in the history; with rejectToolless it
// fails requests that offer no tools.
type loopProvider struct {
	mu               sync.Mutex
	requests         []llm.Request
	answerWhenNudged bool
	rejectToolless   bool
	n                int
}

func (p *loopProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	p.n++
	if p.rejectToolless && len(req.Tools) == 0 {
		return nil, errors.New("HTTP 400: tool history requires tools")
	}
	nudged := false
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser && m.Text == turnOutNudge {
			nudged = true
		}
	}
	ch := make(chan llm.Event, 4)
	if p.answerWhenNudged && nudged {
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "final report: half done, state in NOTES.md"}
	} else {
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "still working"}
		ch <- llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "c" + string(rune('a'+p.n%26)), Name: "ls", Args: "{}"}}
	}
	close(ch)
	return ch, nil
}

func newLoopAgent(t *testing.T, p llm.Provider, maxTurns int) *Agent {
	return New(Config{Provider: p, Model: "m", MaxTurns: maxTurns, Cwd: t.TempDir(),
		Tools: tools.NewRegistry(tools.All()...), Trace: &strings.Builder{}})
}

func TestTurnOutReportWhenModelNeverStops(t *testing.T) {
	p := &loopProvider{}
	a := newLoopAgent(t, p, 6)
	final, err := a.Run(context.Background(), "do the thing")
	var to *TurnOutError
	if !errors.As(err, &to) || !errors.Is(err, ErrTurnOut) || to.Answered || to.Turns != 6 {
		t.Fatalf("err = %v (%+v)", err, to)
	}
	for _, want := range []string{"[TURN-OUT]", "6-turn budget", "Last assistant text:\nstill working", "Tool use: 5 calls (ls 5)", "- ls {}"} {
		if !strings.Contains(final, want) {
			t.Errorf("report missing %q:\n%s", want, final)
		}
	}
	if len(p.requests) != 6 {
		t.Fatalf("requests = %d", len(p.requests))
	}
	// The nudge lands two turns before the end; the last turn offers no tools.
	if hasNudge(p.requests[2].Messages) || !hasNudge(p.requests[3].Messages) {
		t.Errorf("nudge should first appear in request 4 of 6 (MaxTurns-2)")
	}
	if len(p.requests[4].Tools) == 0 || len(p.requests[5].Tools) != 0 {
		t.Errorf("tools: request 5 = %d, request 6 = %d (want >0, 0)", len(p.requests[4].Tools), len(p.requests[5].Tools))
	}
	if a.Node.Status != "turn-out" {
		t.Errorf("status = %q", a.Node.Status)
	}
	// The dropped final-turn call must not linger unanswered in history.
	last := a.History()[len(a.History())-1]
	if last.Role != llm.RoleAssistant || len(last.ToolCalls) != 0 {
		t.Errorf("last history message = %+v", last)
	}
}

func TestTurnOutAnsweredAfterNudge(t *testing.T) {
	p := &loopProvider{answerWhenNudged: true}
	a := newLoopAgent(t, p, 10)
	final, err := a.Run(context.Background(), "do the thing")
	var to *TurnOutError
	if !errors.As(err, &to) || !to.Answered {
		t.Fatalf("err = %v", err)
	}
	if final != "[TURN-OUT] final report: half done, state in NOTES.md" || len(p.requests) != 8 {
		t.Fatalf("final = %q after %d requests", final, len(p.requests))
	}
}

func TestTurnOutToollessRejectedRetriesWithTools(t *testing.T) {
	p := &loopProvider{rejectToolless: true}
	a := newLoopAgent(t, p, 3)
	final, err := a.Run(context.Background(), "x")
	if !errors.Is(err, ErrTurnOut) || !strings.Contains(final, "[TURN-OUT]") {
		t.Fatalf("final=%q err=%v", final, err)
	}
	if got := a.ToolCount; got != 2 {
		t.Errorf("tools run = %d, want 2 (final-turn call dropped)", got)
	}
}

func TestDeadlineTriggersTurnOut(t *testing.T) {
	p := &loopProvider{answerWhenNudged: true}
	a := New(Config{Provider: p, Model: "m", MaxTurns: 50, Cwd: t.TempDir(), Deadline: time.Now().Add(-time.Second),
		Tools: tools.NewRegistry(tools.All()...), Trace: &strings.Builder{}})
	final, err := a.Run(context.Background(), "x")
	if !errors.Is(err, ErrTurnOut) || len(p.requests) != 1 || len(p.requests[0].Tools) != 0 || !strings.HasPrefix(final, "[TURN-OUT] final report") {
		t.Fatalf("final=%q err=%v requests=%d", final, err, len(p.requests))
	}
}

func hasNudge(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Text == turnOutNudge {
			return true
		}
	}
	return false
}

// markupProvider calls ls while tools are offered and, once they are not,
// writes its tool call as text markup (seen live from glm-5.3-flash).
type markupProvider struct{ requests []llm.Request }

func (p *markupProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.Event, 2)
	if len(req.Tools) == 0 {
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "Found three files. Reading them.<tool_call>read<arg_key>path</arg_key><arg_value>f1.go</arg_value></tool_call>"}
	} else {
		ch <- llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "x", Name: "ls", Args: "{}"}}
	}
	close(ch)
	return ch, nil
}

func TestFinalTurnToolMarkupIsATurnOut(t *testing.T) {
	p := &markupProvider{}
	a := newLoopAgent(t, p, 2)
	final, err := a.Run(context.Background(), "x")
	var to *TurnOutError
	if !errors.As(err, &to) || to.Answered {
		t.Fatalf("err = %v, want unanswered turn-out", err)
	}
	if !strings.Contains(final, "Last assistant text:\nFound three files. Reading them.") || strings.Contains(final, "<tool_call>") {
		t.Errorf("report = %q", final)
	}
	// Tiny budgets are nudged from their first turn (MaxTurns-2, at least 1).
	if !hasNudge(p.requests[0].Messages) {
		t.Error("2-turn budget should be nudged on turn 1")
	}
}

func TestToollessAgentIsNeverNudged(t *testing.T) {
	p := &fakeProvider{turns: [][]llm.Event{{{Type: llm.EventTextDelta, TextDelta: "the summary"}}}}
	a := New(Config{Provider: p, Model: "m", MaxTurns: 1, Cwd: t.TempDir(), Trace: &strings.Builder{}})
	final, err := a.Run(context.Background(), "summarize")
	if err != nil || hasNudge(p.requests[0].Messages) || final != "the summary" {
		t.Fatalf("final=%q err=%v", final, err)
	}
}

// flakyProvider fails its first `fails` requests with err, then answers.
type flakyProvider struct {
	fails, n int
	err      error
}

func (p *flakyProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, error) {
	p.n++
	ch := make(chan llm.Event, 2)
	if p.n <= p.fails {
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "partial "}
		ch <- llm.Event{Type: llm.EventError, Err: p.err}
	} else {
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "answer"}
	}
	close(ch)
	return ch, nil
}

func TestTransientStreamFailuresRetryTheTurn(t *testing.T) {
	old := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryBackoff = old }()
	for _, c := range []struct {
		err     error
		fails   int
		wantOK  bool
		wantReq int
	}{
		{errors.New("stream stalled: no data for 3m0s"), 2, true, 3},
		{errors.New("chat: stream read: context deadline exceeded"), 1, true, 2},
		{errors.New("chat: HTTP 503: overloaded"), 3, false, 3},
		{errors.New("chat: HTTP 400: bad request"), 1, false, 1},
	} {
		p := &flakyProvider{fails: c.fails, err: c.err}
		a := New(Config{Provider: p, Model: "m", Cwd: t.TempDir(), Trace: &strings.Builder{}})
		final, err := a.Run(context.Background(), "x")
		if (err == nil) != c.wantOK || p.n != c.wantReq {
			t.Errorf("%v: final=%q err=%v requests=%d", c.err, final, err, p.n)
		}
		if c.wantOK && (final != "answer" || len(a.History()) != 2) {
			t.Errorf("%v: partial output leaked: final=%q history=%d", c.err, final, len(a.History()))
		}
	}
}

func TestToolCallGenerationReportsProgress(t *testing.T) {
	p := &fakeProvider{turns: [][]llm.Event{
		{
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{ID: "c1", Name: "ls"}},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{Args: `{"pa`}},
			{Type: llm.EventToolCall, Seq: 0, Call: &llm.ToolCall{Args: `th":"."}`}},
		},
		{{Type: llm.EventTextDelta, TextDelta: "done"}},
	}}
	var got []int
	a := New(Config{Provider: p, Model: "m", Cwd: t.TempDir(), Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...),
		Events: func(ev Event) {
			if ev.Kind == "tool_args" {
				if ev.Call.Name != "ls" {
					t.Errorf("tool_args name = %q", ev.Call.Name)
				}
				got = append(got, ev.Chars)
			}
		}})
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2] != len(`{"path":"."}`) {
		t.Errorf("progress = %v", got)
	}
	if a.Node.Writing != "" {
		t.Errorf("writing marker not cleared: %q", a.Node.Writing)
	}
}

// stuckTool never returns on its own, like a read of a FIFO.
type stuckTool struct{}

func (stuckTool) Spec() llm.ToolSpec { return llm.ToolSpec{Name: "grep"} }
func (stuckTool) Run(context.Context, string, json.RawMessage) (string, error) {
	select {} // ignores its context entirely
}

func TestStuckToolTimesOutAndAgentContinues(t *testing.T) {
	p := &fakeProvider{turns: [][]llm.Event{
		{{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "g1", Name: "grep", Args: `{"pattern":"cmd_map"}`}}},
		{{Type: llm.EventTextDelta, TextDelta: "narrowed it down"}},
	}}
	a := New(Config{Provider: p, Model: "m", Cwd: t.TempDir(), Trace: &strings.Builder{},
		Tools: tools.NewRegistry(stuckTool{}), ToolTimeout: 100 * time.Millisecond})
	start := time.Now()
	final, err := a.Run(context.Background(), "find cmd_map")
	if err != nil || final != "narrowed it down" || time.Since(start) > 2*time.Second {
		t.Fatalf("final=%q err=%v after %s", final, err, time.Since(start))
	}
	if !hasText(p.requests[1].Messages, "grep timed out after 100ms; narrow the path or pattern") {
		t.Error("the model was not told about the timeout")
	}
}
