package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/provstate"
	"github.com/unix/ugh/internal/tools"
	"github.com/unix/ugh/internal/usage"
)

// routingEnv sets model cards and writes a config with table routing (no Jev
// calls) and no reserve watcher, and isolates the provider state file.
func routingEnv(t *testing.T) config.Config {
	t.Helper()
	routeEvidence.Range(func(k, _ any) bool { routeEvidence.Delete(k); return true }) // no evidence leaks between tests
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	provstate.SetPathForTest(filepath.Join(t.TempDir(), "providers.json"))
	for id, route := range map[string]string{"gpt-astra": "codex:gpt-6-astra", "gpt-sol": "codex:gpt-6.1-sol",
		"mimo-v2.6-pro": "opencode-go:mimo-v2.6-pro", "glm-5.3-flash": "opencode-go:glm-5.3-flash"} {
		setCard(t, "models", id, card{Routes: []string{route}, Thinking: []string{"low", "high"}})
	}
	os.MkdirAll(filepath.Join(cfgHome, "ugh"), 0o700)
	os.WriteFile(filepath.Join(cfgHome, "ugh", "config.toml"), []byte("[routing]\npick_model = \"table\"\n[limits]\nreserve = {}\n"), 0o600)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOracleOnlyAfterCouncil(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go", "openrouter", "opencode-zen", "codex") // a council could run
	r := NewRuntime()
	parent := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: r.Tree})
	res := r.startChild(context.Background(), parent, Task{Task: "second opinion", Role: "oracle"})
	if res.Status != "error" || !strings.Contains(res.Report, "oracles come after council") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunningChildSwitchesModelWhenProviderRunsOut(t *testing.T) {
	routingEnv(t)
	codex := &funcProvider{script: func(n int, req llm.Request) []llm.Event {
		if n == 1 {
			return []llm.Event{call("c1", "ls", "{}")}
		}
		return []llm.Event{{Type: llm.EventError, Err: errors.New(`codex: HTTP 429: {"type":"usage_limit_reached","resets_in_seconds":600}`)}}
	}}
	mimo := &funcProvider{script: func(n int, req llm.Request) []llm.Event {
		if !hasText(req.Messages, "do the work") {
			t.Error("fallback model lost the history")
		}
		return []llm.Event{say("finished on mimo")}
	}}
	old := resolveRoute
	resolveRoute = func(ref string) (llm.Provider, string, error) {
		if strings.HasPrefix(ref, "opencode-go/") {
			return mimo, "mimo-v2.6-pro", nil
		}
		return codex, "gpt-6.1-sol", nil
	}
	defer func() { resolveRoute = old }()
	tree := &Tree{}
	root := tree.Root("root", "m", "")
	node := tree.Child(root, "worker", "codex/gpt-6.1-sol", t.TempDir())
	a := New(Config{Provider: codex, Model: "gpt-6.1-sol", Tree: tree, Node: node, Agent: node.ID, Cwd: t.TempDir(), MaxTurns: 10,
		Routes: []string{"codex:gpt-6.1-sol", "opencode-go:mimo-v2.6-pro"}, Tools: tools.NewRegistry(tools.All()...), Trace: &strings.Builder{}})
	final, err := a.Run(context.Background(), "do the work")
	if err != nil || final != "finished on mimo" || node.Model != "opencode-go/mimo-v2.6-pro" {
		t.Fatalf("final=%q err=%v model=%s", final, err, node.Model)
	}
	if !provstate.Exhausted("codex") || codex.n != 2 {
		t.Fatalf("codex exhausted=%v requests=%d (a quota refusal must not be retried)", provstate.Exhausted("codex"), codex.n)
	}
	// The next turn of any agent skips codex without asking it again.
	if provstate.Usable("codex") {
		t.Fatal("exhausted route still usable")
	}
}

func TestApplyReserve(t *testing.T) {
	routingEnv(t)
	now := time.Now()
	secs := []usage.Section{{Provider: "codex", Windows: []usage.Window{{Label: "5h", Percent: 40}, {Label: "7d", Percent: 93, ResetsAt: now.Add(48 * time.Hour)}}}}
	applyReserve(secs, map[string]float64{"codex": 90}, now)
	st, ok := provstate.Get("codex")
	if !ok || st.Kind != "reserved" || !strings.Contains(st.Reason, "7d window at 93% ≥ reserve 90%") {
		t.Fatalf("state = %+v %v", st, ok)
	}
	secs[0].Windows[1].Percent = 50
	applyReserve(secs, map[string]float64{"codex": 90}, now)
	if !provstate.Usable("codex") {
		t.Fatal("reserve should lift once usage drops")
	}
}
