package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/tools"
)

func TestOneShotIsToolLessByConstruction(t *testing.T) {
	fp := &fakeProvider{turns: [][]llm.Event{{{Type: llm.EventTextDelta, TextDelta: "ok"}, {Type: llm.EventDone, Stop: "stop"}}}}
	a := New(Config{Provider: fp, Model: "m", Cwd: t.TempDir(), OneShot: true, AutoCompact: 0.7,
		Tools: tools.NewRegistry(tools.All()...), MaxTurns: 50,
		Context: ContextConfig{Ladder: "jev", Instructions: true, CompactThreshold: 10},
		Spawn:   func(context.Context, *Agent, []Task) string { return "" }})
	if _, err := a.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if n := len(fp.requests[0].Tools); n != 0 {
		t.Fatalf("one-shot offered %d tools", n)
	}
	if a.cfg.Context.ladderOn() || a.cfg.Context.Instructions || a.cfg.AutoCompact != 0 || a.cfg.MaxTurns != 1 {
		t.Fatalf("context machinery left on: %+v", a.cfg)
	}
}

func TestOneShotModelFallbackRules(t *testing.T) {
	old := resolveRoute
	t.Cleanup(func() { resolveRoute = old })
	conv := &fakeProvider{turns: [][]llm.Event{{{Type: llm.EventTextDelta, TextDelta: "from conversation"}, {Type: llm.EventDone, Stop: "stop"}}}}
	a := New(Config{Provider: conv, Model: "conv", Cwd: t.TempDir(), Trace: &strings.Builder{}})

	// A ref that cannot be resolved falls back to the conversation model.
	resolveRoute = func(string) (llm.Provider, string, error) { return nil, "", errors.New("no credentials") }
	text, _, err := a.oneShot(context.Background(), "p/missing", "sys", "go")
	if err != nil || text != "from conversation" {
		t.Fatalf("unresolvable fallback: %q %v", text, err)
	}

	// A resolvable ref that fails at runtime is reported, not retried elsewhere.
	conv.turns = [][]llm.Event{{{Type: llm.EventTextDelta, TextDelta: "must not be used"}, {Type: llm.EventDone, Stop: "stop"}}}
	bad := &fakeProvider{} // no scripted turns: Stream errors
	resolveRoute = func(string) (llm.Provider, string, error) { return bad, "cheap", nil }
	text, _, err = a.oneShot(context.Background(), "p/cheap", "sys", "go")
	if err == nil || strings.Contains(text, "must not") || len(conv.requests) != 1 {
		t.Fatalf("runtime failure was masked: %q %v (conversation requests %d)", text, err, len(conv.requests))
	}
}
