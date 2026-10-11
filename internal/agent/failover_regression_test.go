package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
)

// A Codex hiccup before any output used to move a Luna agent to OpenRouter
// Luna for the rest of its life. Now the route is retried once, the
// failover lasts only for transientCooldown, and Codex is preferred again.
func TestTransientFailoverIsRetriedAndNotSticky(t *testing.T) {
	routingEnv(t)
	oldBackoff := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryBackoff = oldBackoff })
	codex := &funcProvider{script: func(n int, req llm.Request) []llm.Event {
		if n <= 2 {
			return []llm.Event{{Type: llm.EventError, Err: &llm.HTTPError{Provider: "codex", StatusCode: 503, Body: "overloaded"}}}
		}
		return []llm.Event{say("on codex")}
	}}
	or := &funcProvider{script: func(int, llm.Request) []llm.Event { return []llm.Event{say("on openrouter")} }}
	old := resolveRoute
	resolveRoute = func(ref string) (llm.Provider, string, error) {
		if strings.HasPrefix(ref, "openrouter/") {
			return or, "openai/gpt-6-luna", nil
		}
		return codex, "gpt-6-luna", nil
	}
	t.Cleanup(func() { resolveRoute = old })
	tree := &Tree{}
	node := tree.Child(tree.Root("root", "m", ""), "reviewer", "codex/gpt-6-luna", t.TempDir())
	a := New(Config{Provider: codex, Model: "gpt-6-luna", Tree: tree, Node: node, Agent: node.ID, Cwd: t.TempDir(), MaxTurns: 10,
		Routes: []string{"codex:gpt-6-luna", "openrouter:openai/gpt-6-luna"}, Trace: &strings.Builder{}})

	if final, err := a.Run(context.Background(), "one"); err != nil || final != "on openrouter" || codex.n != 2 {
		t.Fatalf("first turn: final=%q err=%v codex requests=%d (want one retry, then failover)", final, err, codex.n)
	}
	if final, _ := a.Run(context.Background(), "two"); final != "on openrouter" || codex.n != 2 {
		t.Fatalf("during cooldown: final=%q codex requests=%d", final, codex.n)
	}
	for r := range a.routeCooldown {
		a.routeCooldown[r] = time.Now().Add(-time.Second)
	}
	if final, _ := a.Run(context.Background(), "three"); final != "on codex" || node.Model != "codex/gpt-6-luna" {
		t.Fatalf("after cooldown: final=%q model=%s", final, node.Model)
	}
}
