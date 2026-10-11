package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/policy"
)

// grant stubs access evidence: listed providers are confirmed, all else has no credential.
func grant(t *testing.T, providers ...string) {
	t.Helper()
	old := routeAccess
	routeAccess = func(ref string) (policy.AccessStatus, string) {
		prov, _, _ := strings.Cut(ref, "/")
		for _, p := range providers {
			if p == prov {
				return policy.AccessGranted, "test"
			}
		}
		return policy.AccessNoCredential, "test: no credential"
	}
	t.Cleanup(func() { routeAccess = old })
}

// fakeRouter replaces the decision clients: answers maps an instruction prefix to a pick.
func fakeRouter(t *testing.T, answers map[string]string, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := decide
	decide = func(_ context.Context, _ config.Config, _, instr string, opts map[string]string) (decided, error) {
		calls.Add(1)
		if err != nil {
			return decided{}, err
		}
		for prefix, v := range answers {
			if strings.HasPrefix(instr, prefix) {
				if _, ok := opts[v]; !ok {
					t.Errorf("router asked to pick %q which is not an offered option %v", v, opts)
				}
				return decided{Value: v, Router: "jev"}, nil
			}
		}
		t.Errorf("unexpected router question %q", instr)
		return decided{}, errors.New("unexpected")
	}
	t.Cleanup(func() { decide = old })
	return &calls
}

func refs(rs []policy.Route) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Ref()
	}
	return strings.Join(out, ",")
}

func TestManagedPlanFollowsPolicyOrder(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go", "openrouter")
	fakeRouter(t, map[string]string{"Which role": "scout", "Choose complexity": "low", "Choose the thinking": "low"}, nil)
	r := routerRuntime()
	c, err := r.planManaged(context.Background(), "find x", "scout", testRoles)
	if err != nil {
		t.Fatal(err)
	}
	// MiMo Flash (Go first), Luna (Go-Responses unsupported → OpenRouter), Haiku, DeepSeek.
	want := "opencode-go/mimo-v2.6-flash,openrouter/xiaomi/mimo-v2.6-flash,openrouter/openai/gpt-6-luna," +
		"openrouter/anthropic/claude-haiku-5.5,opencode-go/claude-haiku-5-5,opencode-go/deepseek-v4.1-flash,openrouter/deepseek/deepseek-v4.1-flash"
	if got := refs(c.Routes); got != want {
		t.Fatalf("chain\n got %s\nwant %s", got, want)
	}
	if c.Router != "jev" || c.Tier != policy.TierLow || c.Thinking == "" {
		t.Fatalf("choice = %+v", c)
	}
}

func TestManagedPlanOffersOnlyEligibleTiers(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go") // Go: MiMo Flash/Pro, Haiku — and MiMo Pro makes high eligible
	var offered []string
	old := decide
	decide = func(_ context.Context, _ config.Config, _, instr string, opts map[string]string) (decided, error) {
		if strings.HasPrefix(instr, "Choose complexity") {
			for k := range opts {
				offered = append(offered, k)
			}
		}
		for k := range opts {
			return decided{Value: k, Router: "jev"}, nil
		}
		return decided{}, errors.New("no opts")
	}
	defer func() { decide = old }()
	r := routerRuntime()
	if _, err := r.planManaged(context.Background(), "x", "worker", testRoles); err != nil {
		t.Fatal(err)
	}
	if len(offered) != 2 {
		t.Fatalf("offered tiers = %v, want low+high (MiMo Pro on Go)", offered)
	}
	// Without high-tier routes only low is offered and the warning is recorded.
	routeAccess = func(ref string) (policy.AccessStatus, string) {
		if strings.Contains(ref, "mimo-v2.6-flash") {
			return policy.AccessGranted, "test"
		}
		return policy.AccessNoCredential, "test"
	}
	offered = nil
	c, err := r.planManaged(context.Background(), "x", "worker", testRoles)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 0 || c.Tier != policy.TierLow || !strings.Contains(strings.Join(c.Notes, ";"), "limited to low tier") {
		t.Fatalf("offered=%v choice=%+v", offered, c)
	}
}

func TestUnknownAccessIsNotAutomaticDelegation(t *testing.T) {
	routingEnv(t)
	old := routeAccess
	routeAccess = func(string) (policy.AccessStatus, string) { return policy.AccessUnknown, "credential present" }
	defer func() { routeAccess = old }()
	fakeRouter(t, map[string]string{"Choose complexity": "low", "Choose the thinking": "low"}, nil)
	r := routerRuntime()
	if _, err := r.planManaged(context.Background(), "x", "worker", testRoles); err == nil || !strings.Contains(err.Error(), "trust_credentials") {
		t.Fatalf("err = %v, want actionable unknown-access error", err)
	}
	r.Config.Routing.TrustCredentials = true
	if c, err := r.planManaged(context.Background(), "x", "worker", testRoles); err != nil || len(c.Routes) == 0 {
		t.Fatalf("opt-in: %+v %v", c, err)
	}
}

func TestRouterOutageReturnsStructuredResultThenTraditional(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go", "openrouter")
	fakeRouter(t, nil, errors.New("jev: down; clef: down"))
	r := routerRuntime()
	parent := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: r.Tree, Runtime: r})
	res := r.startChild(context.Background(), parent, Task{Task: "do it"})
	if res.Status != "routing-unavailable" || !strings.Contains(res.Report, "Nothing was launched") || res.ID != "" {
		t.Fatalf("res = %+v", res)
	}
	if len(r.Tree.Snapshot().Children) != 0 {
		t.Fatal("a child was created for an unroutable spawn")
	}
	// Traditional mode: the schema now exposes tier/thinking; a tierless call is not defaulted.
	spec := spawnTool{agent: parent}.Spec()
	items := spec.Parameters["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if items["tier"] == nil || items["thinking"] == nil || items["model"] != nil {
		t.Fatal("traditional schema should expose tier/thinking and no model")
	}
	res = r.startChild(context.Background(), parent, Task{Task: "do it again"})
	if res.Status != "error" || !strings.Contains(res.Report, "tier selection is open") {
		t.Fatalf("tierless call in traditional mode: %+v", res)
	}
	if _, err := r.planTraditional(Task{Task: "x", Tier: "medium"}, "worker"); err == nil {
		t.Fatal("unknown tier accepted")
	}
	// The low tier runs its chain in order: MiMo Flash first.
	c, err := r.planTraditional(Task{Task: "x", Tier: "low", Thinking: "high"}, "scout")
	if err != nil || c.Mode != "traditional" || c.Tier != "low" || !strings.HasPrefix(refs(c.Routes), "opencode-go/mimo-v2.6-flash") {
		t.Fatalf("traditional plan: %+v %v", c, err)
	}
	// After the cooldown managed routing resumes and tier is rejected again.
	r.tradUntil = time.Now().Add(-time.Second)
	if spec := (spawnTool{agent: parent}).Spec(); spec.Parameters["properties"].(map[string]any)["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["tier"] != nil {
		t.Fatal("tier still exposed after cooldown")
	}
}

func TestManagedModeRejectsSpawnerChosenModel(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go")
	fakeRouter(t, nil, nil)
	r := routerRuntime()
	parent := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: r.Tree, Runtime: r})
	for _, task := range []Task{{Task: "x", Tier: "high"}, {Task: "x", Thinking: "high"}} {
		res := r.startChild(context.Background(), parent, task)
		if res.Status != "error" || !strings.Contains(res.Report, "chosen by the router") {
			t.Fatalf("%+v → %+v", task, res)
		}
	}
	if len(r.Tree.Snapshot().Children) != 0 {
		t.Fatal("rejected spawn created a child")
	}
}

func TestOraclePlanUsesAstraThenFable(t *testing.T) {
	routingEnv(t)
	grant(t, "codex", "openrouter")
	fakeRouter(t, map[string]string{"Choose the thinking": "high"}, nil)
	r := routerRuntime()
	c, err := r.planManaged(context.Background(), "second opinion", "oracle", append([]string{"oracle"}, testRoles...))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tier != policy.TierOracle || !strings.HasPrefix(refs(c.Routes), "codex/gpt-6-astra") || !strings.Contains(refs(c.Routes), "claude-fable-5.1") || strings.Contains(refs(c.Routes), "openrouter/openai") {
		t.Fatalf("oracle chain = %s", refs(c.Routes))
	}
}

func TestPreferredRouterFallbackIsRecorded(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go")
	old := decide
	decide = func(_ context.Context, _ config.Config, _, _ string, opts map[string]string) (decided, error) {
		for k := range opts {
			return decided{Value: k, Router: "clef", Fallback: "jev: timeout"}, nil
		}
		return decided{}, errors.New("none")
	}
	defer func() { decide = old }()
	r := routerRuntime()
	c, err := r.planManaged(context.Background(), "x", "scout", testRoles)
	if err != nil || c.Router != "clef" || c.Fallback != "jev: timeout" || !strings.Contains(c.describe(), "preferred router unavailable: jev: timeout") {
		t.Fatalf("%+v %v", c, err)
	}
}

// routerRuntime is a runtime in router mode (routingEnv defaults to table routing).
func routerRuntime() *Runtime {
	r := NewRuntime()
	r.Config.Routing.PickModel = "jev"
	return r
}

func TestStaleHighTierReroutesOverReducedSet(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go")
	old := decide
	var tierAsks int
	decide = func(_ context.Context, _ config.Config, _, instr string, opts map[string]string) (decided, error) {
		if strings.HasPrefix(instr, "Choose complexity") {
			tierAsks++
			if tierAsks == 1 {
				// High disappears between the decision and admission.
				routeAccess = func(ref string) (policy.AccessStatus, string) {
					if strings.Contains(ref, "mimo-v2.6-pro") || strings.Contains(ref, "sonnet") {
						return policy.AccessExhausted, "gone"
					}
					if strings.HasPrefix(ref, "opencode-go/") {
						return policy.AccessGranted, "ok"
					}
					return policy.AccessNoCredential, "none"
				}
				return decided{Value: "high", Router: "jev"}, nil
			}
			if _, ok := opts["high"]; ok {
				t.Error("rerouted decision still offers the vanished tier")
			}
		}
		for _, k := range []string{"low", "medium", "off"} {
			if _, ok := opts[k]; ok {
				return decided{Value: k, Router: "jev"}, nil
			}
		}
		for k := range opts {
			return decided{Value: k, Router: "jev"}, nil
		}
		return decided{}, errors.New("no opts")
	}
	defer func() { decide = old }()
	r := routerRuntime()
	c, err := r.planManaged(context.Background(), "x", "worker", testRoles)
	if err != nil || c.Tier != policy.TierLow || strings.Contains(refs(c.Routes), "pro") {
		t.Fatalf("%+v %v", c, err)
	}
}

var testRoles = []string{"scout", "worker", "planner", "reviewer", "log-parser"}

func TestTierChainFromConfig(t *testing.T) {
	cfg := config.Default()
	want := map[policy.Tier][]policy.Family{
		policy.TierLow:    {policy.FamilyMiMoFlash, policy.FamilyGPTLuna, policy.FamilyClaudeHaiku, policy.FamilyDeepSeekFlash},
		policy.TierHigh:   {policy.FamilyGPTSol, policy.FamilyMiMoPro, policy.FamilyClaudeSonnet},
		policy.TierOracle: {policy.FamilyGPTAstra, policy.FamilyClaudeFable},
	}
	for tier, fams := range want {
		if got := tierChain(cfg, tier); fmt.Sprint(got) != fmt.Sprint(fams) {
			t.Errorf("default %s chain = %v, want %v", tier, got, fams)
		}
	}
	cfg.Routing.Tiers = map[string]string{"high": "mimo-v2.6-pro", "low": "nope"}
	cfg.Routing.Fallback = map[string]string{"mimo-v2.6-pro": "claude-sonnet-5.5", "claude-sonnet-5.5": "mimo-v2.6-pro", "nope": "gpt-luna", "gpt-luna": ""}
	if got := tierChain(cfg, policy.TierHigh); fmt.Sprint(got) != fmt.Sprint([]policy.Family{policy.FamilyMiMoPro, policy.FamilyClaudeSonnet}) {
		t.Errorf("overridden high chain (with a cycle) = %v", got)
	}
	if got := tierChain(cfg, policy.TierLow); fmt.Sprint(got) != fmt.Sprint([]policy.Family{policy.FamilyGPTLuna}) {
		t.Errorf("unknown head skipped, empty link ends: %v", got)
	}
	if got := coordinatorChain("gpt-sol"); got[0] != policy.FamilyGPTSol || len(got) != len(policy.CouncilCoordinatorChain()) {
		t.Errorf("aggregator first = %v", got)
	}
}
