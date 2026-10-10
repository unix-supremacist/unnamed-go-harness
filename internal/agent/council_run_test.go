package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/policy"
)

// councilHarness fakes every route: behaviour is keyed by the stage's system prompt.
type councilHarness struct {
	mu      sync.Mutex
	calls   map[string]int      // stage → requests
	byRoute map[string][]string // stage → refs used
	failRef func(ref, stage string) error
}

func stageOf(sys string) string {
	switch {
	case strings.HasPrefix(sys, "Write exactly N"):
		return "angles"
	case strings.HasPrefix(sys, "Investigate the core"):
		return "panel"
	case strings.HasPrefix(sys, "Synthesize the supplied"):
		return "synthesis"
	case strings.HasPrefix(sys, "Gather evidence"):
		return "prescout"
	}
	return "other"
}

func (h *councilHarness) install(t *testing.T) {
	t.Helper()
	h.calls, h.byRoute = map[string]int{}, map[string][]string{}
	old := resolveRoute
	resolveRoute = func(ref string) (llm.Provider, string, error) {
		return &funcProvider{script: func(_ int, req llm.Request) []llm.Event {
			st := stageOf(req.System)
			h.mu.Lock()
			h.calls[st]++
			h.byRoute[st] = append(h.byRoute[st], ref)
			fail := h.failRef
			h.mu.Unlock()
			if fail != nil {
				if err := fail(ref, st); err != nil {
					return []llm.Event{{Type: llm.EventError, Err: err}}
				}
			}
			switch st {
			case "angles":
				return []llm.Event{say("1. Investigate the first approach in depth\n2. Investigate the second approach in depth\n3. Investigate the third approach in depth\n4. Investigate the fourth approach in depth\n5. Investigate the fifth approach in depth")}
			case "panel":
				return []llm.Event{say("## Answer\nreport from " + ref)}
			}
			return []llm.Event{say("RESULT " + st)}
		}}, ref[strings.Index(ref, "/")+1:], nil
	}
	t.Cleanup(func() { resolveRoute = old })
}

// councilRoot builds a root agent with a task, runtime, and admission in a temp dir.
func councilRoot(t *testing.T, gate perm.Gate) (*Agent, *Runtime) {
	t.Helper()
	r := routerRuntime()
	tree := &Tree{}
	a := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: t.TempDir(), Tree: tree, Runtime: r, Gate: gate,
		Admission: testAdmission(t, admission.PriorityRoot), Spawn: r.Spawn, StartChild: r.StartChild})
	a.task = "design the cache"
	return a, r
}

func allProviders(t *testing.T) {
	grant(t, "opencode-go", "openrouter", "opencode-zen", "codex")
}

func TestCouncilCompletesWithStableCoverageAndCappedFamilies(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	fakeRouter(t, nil, nil)
	h := &councilHarness{}
	h.install(t)
	root, _ := councilRoot(t, nil)
	res, err := councilRun(context.Background(), root, "Which cache?", []byte(`5`), false, councilOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != StateCompleted || res.Strategy != string(StrategyNormal) || res.Report != "RESULT synthesis" {
		t.Fatalf("result = %+v", res)
	}
	// 5 angles: 4 uncapped families × 5 + 3 capped families × 3 seats each.
	per := map[PanelFamily]int{}
	angles := map[string]bool{}
	for _, s := range res.Plan.AllocatedSeats {
		per[s.Family]++
		angles[s.AngleID] = true
	}
	if len(res.Plan.AllocatedSeats) != 4*5+3*3 || len(angles) != 5 {
		t.Fatalf("seats=%d angles=%d", len(res.Plan.AllocatedSeats), len(angles))
	}
	for _, f := range CappedFamilies {
		if per[f] != 3 {
			t.Errorf("capped family %s has %d seats, want 3", f, per[f])
		}
	}
	if res.Assessment.DistinctFamilies != 7 || res.Assessment.CoveredAngles != 5 {
		t.Fatalf("assessment = %+v", res.Assessment)
	}
	// Coordinator = MiMo Pro (first of the chain); panel seats never ran on it.
	for _, ref := range h.byRoute["synthesis"] {
		if !strings.Contains(ref, "mimo-v2.6-pro") {
			t.Errorf("synthesis ran on %s, want MiMo Pro", ref)
		}
	}
	// Gemini must never route through Go.
	for _, ref := range h.byRoute["panel"] {
		if strings.HasPrefix(ref, "opencode-go/") && strings.Contains(ref, "gemini") {
			t.Errorf("gemini routed via go: %s", ref)
		}
	}
	stored := root.council(res.ID)
	if stored == nil || stored.Outcome != StateCompleted || stored.ReportRevisionHash == "" || len(stored.RetainedReports) != len(res.Plan.AllocatedSeats) {
		t.Fatalf("stored = %+v", stored)
	}
	if !strings.Contains(res.Render(), "Council "+res.ID+" · outcome: completed") {
		t.Fatalf("render = %s", res.Render())
	}
}

func TestCouncilUnavailableBeforeSpendThenDirectOracleAllowed(t *testing.T) {
	routingEnv(t)
	grant(t, "opencode-go") // Go: MiMo Flash, DeepSeek, GLM, Qwen, Haiku — Qwen/GLM/DeepSeek/MiMo/Haiku = 5 families? restrict below
	old := routeAccess
	routeAccess = func(ref string) (policy.AccessStatus, string) {
		if strings.HasPrefix(ref, "opencode-go/mimo-v2.6-flash") || strings.HasPrefix(ref, "opencode-go/deepseek") || strings.HasPrefix(ref, "codex/gpt-6-astra") {
			return policy.AccessGranted, "ok"
		}
		return policy.AccessNoCredential, "none"
	}
	defer func() { routeAccess = old }()
	fakeRouter(t, nil, nil)
	h := &councilHarness{}
	h.install(t)
	root, r := councilRoot(t, nil)
	res, err := councilRun(context.Background(), root, "Which cache?", nil, true, councilOpts{})
	if err == nil || !strings.Contains(err.Error(), "unavailable before any spend") || res.Outcome != StateUnavailableBeforeSpend {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	for stage, n := range h.calls {
		if n != 0 {
			t.Fatalf("paid work happened before the preflight failed: %s ×%d", stage, n)
		}
	}
	// With no council possible, a direct Astra oracle is allowed under normal eligibility.
	if err := oracleGate(context.Background(), r.Config, root); err != nil {
		t.Fatalf("oracle after unavailable council: %v", err)
	}
}

func TestOracleBlockedWhileCouncilIsAvailableAndForOtherTasks(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	fakeRouter(t, nil, nil)
	h := &councilHarness{}
	h.install(t)
	root, r := councilRoot(t, nil)
	if err := oracleGate(context.Background(), r.Config, root); err == nil || !strings.Contains(err.Error(), "oracles come after council") {
		t.Fatalf("oracle before any council: %v", err)
	}
	if _, err := councilRun(context.Background(), root, "Which cache?", nil, false, councilOpts{}); err != nil {
		t.Fatal(err)
	}
	// A completed council: escalation needs the human, and headless refuses.
	err := oracleGate(context.Background(), r.Config, root)
	if !errors.Is(err, ErrApprovalNotGranted) {
		t.Fatalf("headless escalation: %v", err)
	}
	// With an approving human it passes — for this task only.
	root.cfg.Gate = approvingGate{allow: true}
	if err := oracleGate(context.Background(), r.Config, root); err != nil {
		t.Fatalf("approved escalation: %v", err)
	}
	root.task = "an unrelated question"
	if err := oracleGate(context.Background(), r.Config, root); err == nil || !strings.Contains(err.Error(), "oracles come after council") {
		t.Fatalf("council on question A must not authorize an oracle on question B: %v", err)
	}
}

type approvingGate struct{ allow bool }

func (approvingGate) Check(context.Context, perm.Call) perm.Outcome {
	return perm.Outcome{Action: perm.Allow, Reason: "auto-allow must not count as approval"}
}
func (g approvingGate) Approve(perm.Call) bool { return g.allow }

func TestCouncilPanelFailureRetainsEvidenceWithoutOracleEscalation(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	fakeRouter(t, nil, nil)
	h := &councilHarness{failRef: func(ref, stage string) error {
		// Only MiMo Flash and DeepSeek panels survive: below the three-family quorum.
		if stage == "panel" && !strings.Contains(ref, "mimo-v2.6-flash") && !strings.Contains(ref, "deepseek") {
			return errors.New("HTTP 400: rejected")
		}
		return nil
	}}
	h.install(t)
	root, r := councilRoot(t, approvingGate{allow: true})
	res, err := councilRun(context.Background(), root, "Which cache?", []byte(`2`), false, councilOpts{})
	if err == nil || res.Outcome != StatePanelFailed || !strings.Contains(err.Error(), "no automatic oracle escalation") {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	m := root.council(res.ID)
	if m == nil || len(m.RetainedReports) == 0 {
		t.Fatalf("evidence not retained: %+v", m)
	}
	if h.calls["synthesis"] != 0 {
		t.Fatal("a non-viable panel must not be synthesized")
	}
	if err := oracleGate(context.Background(), r.Config, root); err == nil || !strings.Contains(err.Error(), "not automatic") {
		t.Fatalf("panel-failed council must not unlock an oracle even with a human available: %v", err)
	}
}

func TestSynthesisFailureRetriesFromRetainedReportsOnly(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	fakeRouter(t, nil, nil)
	fail := true
	h := &councilHarness{}
	h.failRef = func(ref, stage string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if stage == "synthesis" && fail {
			return errors.New("HTTP 400: rejected")
		}
		return nil
	}
	h.install(t)
	root, _ := councilRoot(t, nil)
	res, err := councilRun(context.Background(), root, "Which cache?", []byte(`2`), false, councilOpts{})
	if err == nil || res.Outcome != StateSynthesisFailed || !strings.Contains(err.Error(), "retry with council") {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	panelBefore := h.calls["panel"]
	h.mu.Lock()
	fail = false
	h.mu.Unlock()
	res2, err := councilRun(context.Background(), root, "ignored", nil, false, councilOpts{Retry: res.ID})
	if err != nil || res2.Report != "RESULT synthesis" || res2.Outcome != StateCompleted {
		t.Fatalf("retry: %v %+v", err, res2)
	}
	if h.calls["panel"] != panelBefore || h.calls["angles"] != 1 {
		t.Fatalf("retry re-ran panel/angles: panel %d→%d angles %d", panelBefore, h.calls["panel"], h.calls["angles"])
	}
	// A retry for another task is refused.
	root.task = "something else"
	if _, err := councilRun(context.Background(), root, "x", nil, false, councilOpts{Retry: res.ID}); err == nil {
		t.Fatal("retry across tasks must be refused")
	}
}

// noChainAccess grants everything except the normal coordinator chain.
func noChainAccess(t *testing.T) {
	old := routeAccess
	routeAccess = func(ref string) (policy.AccessStatus, string) {
		for _, bad := range []string{"mimo-v2.6-pro", "gpt-6.1-sol", "claude-sonnet"} {
			if strings.Contains(ref, bad) {
				return policy.AccessNoCredential, "none"
			}
		}
		return policy.AccessGranted, "ok"
	}
	t.Cleanup(func() { routeAccess = old })
}

func TestCouncilStrategyRouterChoiceAndExplicitFallback(t *testing.T) {
	routingEnv(t)
	noChainAccess(t)
	h := &councilHarness{}
	h.install(t)

	fakeRouter(t, map[string]string{"The normal council": "economical"}, nil)
	root, _ := councilRoot(t, nil)
	res, err := councilRun(context.Background(), root, "Which cache?", []byte(`2`), false, councilOpts{})
	if err != nil || res.Strategy != string(StrategyEconomical) {
		t.Fatalf("router-chosen economical: %v %+v", err, res)
	}
	for _, ref := range h.byRoute["synthesis"] {
		if !strings.Contains(ref, "haiku") {
			t.Errorf("economical synthesis ran on %s, want Haiku first", ref)
		}
	}

	// No router: stop before spending, open explicit selection, and honor it only then.
	fakeRouter(t, nil, errors.New("jev: down; clef: down"))
	root2, r2 := councilRoot(t, nil)
	calls := h.calls["panel"]
	_, err = councilRun(context.Background(), root2, "Which cache?", []byte(`2`), false, councilOpts{})
	var need *StrategyRequiredError
	if !errors.As(err, &need) || h.calls["panel"] != calls {
		t.Fatalf("err=%v panel calls %d→%d", err, calls, h.calls["panel"])
	}
	if !r2.traditional() {
		t.Fatal("traditional selection did not open")
	}
	res, err = councilRun(context.Background(), root2, "Which cache?", []byte(`2`), false, councilOpts{Strategy: "oracle-led"})
	if err != nil || res.Strategy != string(StrategyOracleLed) {
		t.Fatalf("explicit oracle-led: %v %+v", err, res)
	}
	for _, ref := range h.byRoute["synthesis"][len(h.byRoute["synthesis"])-1:] {
		if !strings.Contains(ref, "astra") {
			t.Errorf("oracle-led synthesis ran on %s, want Astra first", ref)
		}
	}
	// A managed (router-available) call cannot smuggle a strategy.
	r2.tradUntil = r2.tradUntil.AddDate(-1, 0, 0)
	if _, err := councilRun(context.Background(), root2, "Which cache?", []byte(`2`), false, councilOpts{Strategy: "oracle-led"}); err == nil {
		t.Fatal("strategy accepted while routers are available")
	}
}

func TestCouncilSolCoordinatorUsesExemptBucketPanelsDoNot(t *testing.T) {
	routingEnv(t)
	old := routeAccess
	routeAccess = func(ref string) (policy.AccessStatus, string) {
		if strings.Contains(ref, "mimo-v2.6-pro") { // chain: skip MiMo Pro, land on Sol
			return policy.AccessNoCredential, "none"
		}
		return policy.AccessGranted, "ok"
	}
	defer func() { routeAccess = old }()
	fakeRouter(t, nil, nil)
	h := &councilHarness{}
	h.install(t)
	root, _ := councilRoot(t, nil)
	// Occupy the whole combined Sol/Astra pool: an ordinary Sol child would be refused.
	held, err := root.cfg.Admission.Manager.Acquire(context.Background(), admission.Request{Provider: "codex", Model: "gpt-6.1-sol", Priority: admission.PriorityRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := root.cfg.Admission.Manager.Acquire(context.Background(), admission.Request{Provider: "codex", Model: "gpt-6.1-sol", Priority: admission.PriorityChild}); !errors.Is(err, admission.ErrCapacityExceeded) {
		t.Fatalf("pool should be full: %v", err)
	}
	res, err := councilRun(context.Background(), root, "Which cache?", []byte(`2`), false, councilOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range h.byRoute["synthesis"] {
		if ref != "codex/gpt-6.1-sol" {
			t.Errorf("synthesis ran on %s, want the exempt Codex Sol coordinator", ref)
		}
	}
	if res.Outcome != StateCompleted {
		t.Fatalf("outcome = %s", res.Outcome)
	}
}

func TestCouncilOversizedRequestRejectedBeforeSpend(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	fakeRouter(t, nil, nil)
	h := &councilHarness{}
	h.install(t)
	root, _ := councilRoot(t, nil)
	_, err := councilRun(context.Background(), root, "Which cache?", []byte(`12`), false, councilOpts{})
	var big *OversizedRequestError
	if !errors.As(err, &big) {
		t.Fatalf("err = %v, want OversizedRequestError", err)
	}
	for stage, n := range h.calls {
		if n != 0 {
			t.Fatalf("spent before rejecting an oversized council: %s ×%d", stage, n)
		}
	}
}

func TestRouterPickedOracleStillNeedsApproval(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	writeRoleCards(t, "scout", "worker", "oracle")
	fakeRouter(t, map[string]string{"Which role": "oracle", "Choose the thinking": "high"}, nil)
	h := &councilHarness{}
	h.install(t)
	root, r := councilRoot(t, nil) // no approver: headless
	if _, err := councilRun(context.Background(), root, "Which cache?", []byte(`2`), false, councilOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{{Task: "second opinion"}, {Task: "second opinion", Role: "oracle"}} {
		res := r.startChild(context.Background(), root, task)
		if res.Status != "error" || !strings.Contains(res.Report, "needs your approval") {
			t.Fatalf("%+v → %+v", task, res)
		}
	}
	if len(r.Tree.Snapshot().Children) != 0 {
		t.Fatal("an unapproved oracle spawn created a child")
	}
}

func TestOnlyGenuineUnavailabilityUnlocksDirectOracle(t *testing.T) {
	routingEnv(t)
	allProviders(t)
	fakeRouter(t, nil, nil)
	h := &councilHarness{}
	h.install(t)
	root, r := councilRoot(t, nil)
	// A bad request (oversized) and a strategy smuggled into a managed call spend nothing and unlock nothing.
	if _, err := councilRun(context.Background(), root, "q", []byte(`12`), false, councilOpts{}); err == nil {
		t.Fatal("oversized council should fail")
	}
	if _, err := councilRun(context.Background(), root, "q", []byte(`2`), false, councilOpts{Strategy: "oracle-led"}); err == nil {
		t.Fatal("strategy accepted while the normal chain exists")
	}
	if err := oracleGate(context.Background(), r.Config, root); err == nil || !strings.Contains(err.Error(), "oracles come after council") {
		t.Fatalf("invalid council calls must not unlock a direct oracle: %v", err)
	}
	if len(root.councils) != 0 {
		t.Fatalf("recorded outcomes for requests that never ran: %v", root.councils)
	}
}
