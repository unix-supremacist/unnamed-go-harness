package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

// growProvider reports prompt tokens proportional to the history length,
// calls ls until turn `answerAt`, and answers summarizer requests.
type growProvider struct {
	mu        sync.Mutex
	turns     int
	answerAt  int
	summaries int
	lastMsgs  []llm.Message
}

func (p *growProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan llm.Event, 4)
	defer close(ch)
	if req.System == CompactSystem {
		p.summaries++
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "SUMMARY of the early work"}
		return ch, nil
	}
	p.turns++
	p.lastMsgs = req.Messages
	ch <- llm.Event{Type: llm.EventUsage, Usage: &llm.Usage{In: 100 * len(req.Messages)}}
	if p.turns >= p.answerAt {
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "all done"}
		return ch, nil
	}
	ch <- llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "t" + strings.Repeat("x", p.turns), Name: "ls", Args: "{}"}}
	return ch, nil
}

func TestAutoCompactionShrinksHistoryKeepsTaskAndTail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	sess, err := session.Create(dir, session.Record{Model: "m", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	p := &growProvider{answerAt: 12}
	var compacted []llm.Message
	a := New(Config{Provider: p, Model: "m", MaxTurns: 40, Cwd: dir, Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), AutoCompact: 0.5, ContextWindow: 1000,
		OnMessage: func(m llm.Message) { _ = sess.Append(session.Record{Type: "message", Message: &m}) },
		OnCompact: func(h []llm.Message) {
			compacted = h
			_ = sess.Append(session.Record{Type: "compaction", Seed: h})
		}})
	final, err := a.Run(context.Background(), "THE ORIGINAL TASK")
	if err != nil || final != "all done" {
		t.Fatalf("final=%q err=%v", final, err)
	}
	if p.summaries == 0 || a.Compactions == 0 || compacted == nil {
		t.Fatalf("no compaction: summaries=%d compactions=%d", p.summaries, a.Compactions)
	}
	first := p.lastMsgs[0]
	if first.Role != llm.RoleUser || !strings.Contains(first.Text, "Original task:\nTHE ORIGINAL TASK") || !strings.Contains(first.Text, "SUMMARY of the early work") {
		t.Fatalf("seed message = %+v", first)
	}
	// Every 5 messages of growth crosses 0.5 × 1000 tokens again, so the
	// prompt the model sees stays bounded.
	if n := len(p.lastMsgs); n > compactKeep+6 {
		t.Errorf("last request carried %d messages", n)
	}
	for i, m := range p.lastMsgs {
		if m.Role == llm.RoleTool && (i == 0 || p.lastMsgs[i-1].Role == llm.RoleUser) {
			t.Errorf("orphaned tool result at %d", i)
		}
	}
	sess.Close()
	loaded, err := session.Load(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	// Resume sees the compacted history plus what followed it.
	if !strings.Contains(loaded.Messages[0].Text, "SUMMARY") || loaded.Messages[len(loaded.Messages)-1].Text != "all done" {
		t.Fatalf("resume history starts %q ends %q", loaded.Messages[0].Text, loaded.Messages[len(loaded.Messages)-1].Text)
	}
	if len(loaded.Messages) >= 1+12*2 {
		t.Errorf("resume history not compacted: %d messages", len(loaded.Messages))
	}
}

func TestAutoCompactionDisabledAndUnknownWindow(t *testing.T) {
	for _, cfg := range []Config{{AutoCompact: 0, ContextWindow: 100}, {AutoCompact: 0.5, Model: "no-such-model"}} {
		p := &growProvider{answerAt: 8}
		cfg.Provider, cfg.MaxTurns, cfg.Cwd, cfg.Trace = p, 20, t.TempDir(), &strings.Builder{}
		if cfg.Model == "" {
			cfg.Model = "m"
		}
		cfg.Tools = tools.NewRegistry(tools.All()...)
		a := New(cfg)
		if _, err := a.Run(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
		if p.summaries != 0 {
			t.Errorf("%+v: compacted %d times", cfg.AutoCompact, p.summaries)
		}
	}
}

func TestCompactionLimitResolution(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mk := func(cfg Config) *Agent {
		cfg.Provider, cfg.Model, cfg.Cwd = &growProvider{}, "p/m", t.TempDir()
		return New(cfg)
	}
	cases := []struct {
		name   string
		cfg    Config
		ref    string
		want   int
		wantOK bool
	}{
		{"absolute only, unknown window", Config{AutoCompact: 0.7, Context: ContextConfig{CompactThreshold: 100000}}, "p/m", 100000, true},
		{"auto_compact=0 disables everything", Config{AutoCompact: 0, ContextWindow: 50000, Context: ContextConfig{CompactThreshold: 100000}}, "p/m", 0, false},
		{"fraction beats a larger absolute", Config{AutoCompact: 0.5, ContextWindow: 120000, Context: ContextConfig{CompactThreshold: 100000}}, "p/m", 60000, true},
		{"absolute beats a larger fraction", Config{AutoCompact: 0.9, ContextWindow: 1000000, Context: ContextConfig{CompactThreshold: 100000}}, "p/m", 100000, true},
		{"headroom is the hard limit", Config{AutoCompact: 0.99, ContextWindow: 30000}, "p/m", 14000, true},
		{"per-model override wins over threshold", Config{AutoCompact: 0.7, Context: ContextConfig{CompactThreshold: 100000, CompactThresholds: map[string]int{"p/m": 40000}}}, "p/m", 40000, true},
		{"override 0 removes the absolute limit", Config{AutoCompact: 0.7, ContextWindow: 200000, Context: ContextConfig{CompactThreshold: 100000, CompactThresholds: map[string]int{"p/m": 0}}}, "p/m", 140000, true},
		{"no limit defined", Config{AutoCompact: 0.7}, "p/m", 0, false},
		{"one-shot never compacts", Config{AutoCompact: 0.7, OneShot: true, Context: ContextConfig{CompactThreshold: 5}}, "p/m", 0, false},
	}
	for _, c := range cases {
		got, ok := mk(c.cfg).limitFor(c.ref)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: limit = %d,%v want %d,%v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

// quietProvider answers summarizer requests and then scripted tool turns, and
// reports no usage at all, so only the preflight byte estimate can notice size.
type quietProvider struct {
	mu        sync.Mutex
	big       string // path of the large file to read on turn 2
	summaries int
	requests  [][]llm.Message
	n         int
}

func (p *quietProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan llm.Event, 4)
	defer close(ch)
	if req.System == CompactSystem {
		p.summaries++
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "SUMMARY"}
		return ch, nil
	}
	p.n++
	p.requests = append(p.requests, req.Messages)
	switch {
	case p.n < 10:
		ch <- llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "t" + strings.Repeat("x", p.n), Name: "ls", Args: "{}"}}
	case p.n == 10:
		ch <- llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "big", Name: "read", Args: `{"path":"` + p.big + `"}`}}
	default:
		ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "done"}
	}
	return ch, nil
}

// A huge fresh tool result is measured before the next request goes out, even
// though no usage has been reported for it yet.
func TestHugeFreshResultIsCaughtBeforeItIsSent(t *testing.T) {
	dir := t.TempDir()
	big := dir + "/big.txt"
	if err := os.WriteFile(big, []byte(strings.Repeat("0123456789abcdef\n", 1400)), 0o644); err != nil { // ~23 KiB
		t.Fatal(err)
	}
	p := &quietProvider{big: big}
	a := New(Config{Provider: p, Model: "m", MaxTurns: 30, Cwd: dir, Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), AutoCompact: 0.7,
		Context: ContextConfig{CompactThreshold: 4000}}) // ~14 KB of prompt
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if p.summaries == 0 {
		t.Fatal("never compacted")
	}
	// The request right after the big read already carries the summary seed,
	// i.e. compaction ran before it was sent rather than a turn later.
	after := p.requests[10]
	if !strings.Contains(after[0].Text, "SUMMARY") {
		t.Fatalf("request after the big read was not compacted: first message %q", after[0].Text)
	}
}

// Failover to a smaller window compacts for the new model instead of skipping it.
func TestFailoverToSmallerWindowCompacts(t *testing.T) {
	dir := t.TempDir()
	p := &quietProvider{big: dir + "/none"}
	old := resolveRoute
	t.Cleanup(func() { resolveRoute = old })
	resolveRoute = func(ref string) (llm.Provider, string, error) { return p, "small", nil }
	a := New(Config{Provider: p, Model: "big", MaxTurns: 30, Cwd: dir, Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), AutoCompact: 0.7,
		Routes: []string{"other:small"}})
	a.hist = append(a.hist, llm.Message{Role: llm.RoleUser, Text: "task"})
	for i := 0; i < 8; i++ {
		id := "k" + strings.Repeat("x", i)
		a.hist = append(a.hist,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "ls", Args: "{}"}}},
			llm.Message{Role: llm.RoleTool, ToolCallID: id, ToolName: "ls", Text: strings.Repeat("y", 3000)})
	}
	if est := a.estimateTokens(a.hist, nil); est < 5000 {
		t.Fatalf("test history too small: %d", est)
	}
	a.cfg.ContextWindow = 0
	a.limitCache = map[string]limitResult{"other/small": {limit: 3000, ok: true}}
	if _, _, err := a.streamRoutes(context.Background(), a.hist, nil); err != nil {
		// The fake keeps asking for tools; only compaction before the request matters.
		t.Logf("stream: %v", err)
	}
	if p.summaries == 0 || a.Compactions == 0 {
		t.Fatalf("failover did not compact (summaries=%d)", p.summaries)
	}
}

// Missing usage keeps the calibrated ratio instead of resetting or zeroing it.
func TestMissingUsageKeepsRatio(t *testing.T) {
	a := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir()})
	a.tokPerByte, a.lastReqBytes = 0.5, 1000
	// A usage event without prompt tokens (In == 0) must not touch the ratio.
	fp := &fakeProvider{turns: [][]llm.Event{{
		{Type: llm.EventUsage, Usage: &llm.Usage{Out: 5}},
		{Type: llm.EventTextDelta, TextDelta: "ok"}, {Type: llm.EventDone, Stop: "stop"}}}}
	a.cfg.Provider = fp
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if a.tokPerByte != 0.5 {
		t.Fatalf("ratio = %v after a usage report with no prompt tokens", a.tokPerByte)
	}
	b := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir()})
	if b.tokPerByte != 0 || b.estimateTokens([]llm.Message{{Role: llm.RoleUser, Text: strings.Repeat("a", 3500)}}, nil) != 1000 {
		t.Fatalf("default ratio should be 1/3.5: %d", b.estimateTokens([]llm.Message{{Role: llm.RoleUser, Text: strings.Repeat("a", 3500)}}, nil))
	}
}

// A compressed view below the limit does not compact even though the raw
// history (and the previous request) was over it.
func TestCompressedViewBelowLimitDoesNotCompact(t *testing.T) {
	jev := &fakeJev{choose: func(int, string) string { return "hide" }}
	p := &quietProvider{big: "none"}
	hist, disp := ladderHistory(30) // 30 results of ~4 KiB ≈ 120 KiB raw
	a := New(Config{Provider: p, Model: "m", MaxTurns: 3, Cwd: t.TempDir(), History: hist, Archive: disp, Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), AutoCompact: 0.7,
		Context: ContextConfig{Ladder: "jev", KeepRecent: 2, MinChunkBytes: 2048, MinSavings: 1, Jev: jev, CompactThreshold: 20000}})
	a.lastIn = 90000 // the previous request was far over the limit
	raw := a.estimateTokens(a.hist, nil)
	if raw < 20000 {
		t.Fatalf("test setup: raw history only %d tokens", raw)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil && !errors.Is(err, ErrTurnOut) {
		t.Fatal(err)
	}
	if p.summaries != 0 || a.Compactions != 0 {
		t.Fatalf("compacted despite the compressed view fitting (summaries=%d)", p.summaries)
	}
}
