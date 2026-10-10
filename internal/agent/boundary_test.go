package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/llm"
)

// lingeringProvider emits an error but keeps its producer alive until the
// request context is cancelled, then takes a while to wind down.
type lingeringProvider struct {
	stopped atomic.Int64 // unix nanos when the producer finished
}

func (p *lingeringProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event)
	go func() {
		defer close(ch)
		select {
		case ch <- llm.Event{Type: llm.EventError, Err: errors.New("boom")}:
		case <-ctx.Done():
		}
		<-ctx.Done()
		time.Sleep(80 * time.Millisecond) // winding down after cancellation
		p.stopped.Store(time.Now().UnixNano())
	}()
	return ch, nil
}

func testAdmission(t *testing.T, prio admission.Priority) Admission {
	t.Helper()
	return Admission{Manager: admission.NewManager(admission.WithBaseDir(t.TempDir())), Priority: prio, Purpose: "test"}
}

func TestAdmissionHeldUntilErroringProducerStops(t *testing.T) {
	ad := testAdmission(t, admission.PriorityRoot)
	p := &lingeringProvider{}
	a := New(Config{Provider: p, Model: "gpt-6.1-sol", ProviderName: "codex", Admission: ad, Cwd: t.TempDir()})

	var firstFree atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
			if a.cfg.Node == nil {
				continue
			}
			h, err := ad.Manager.Acquire(context.Background(), admission.Request{Provider: "codex", Model: "gpt-6.1-sol", Priority: admission.PriorityChild})
			if err == nil {
				if firstFree.Load() == 0 {
					firstFree.Store(time.Now().UnixNano())
				}
				_ = h.Close()
				if p.stopped.Load() != 0 {
					return
				}
				continue
			}
		}
	}()
	if _, err := a.Run(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Run err = %v, want boom", err)
	}
	<-done
	stopped, free := p.stopped.Load(), firstFree.Load()
	if stopped == 0 || free == 0 {
		t.Fatalf("stopped=%d free=%d", stopped, free)
	}
	if free < stopped {
		t.Fatalf("admission released %s before the producer stopped", time.Duration(stopped-free))
	}
}

func TestChildCapacityFallsToNextRoute(t *testing.T) {
	ad := testAdmission(t, admission.PriorityChild)
	// Occupy the single basic-plan slot.
	h, err := ad.Manager.Acquire(context.Background(), admission.Request{Provider: "codex", Model: "gpt-6.1-sol", Priority: admission.PriorityRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	codex := &funcProvider{script: func(int, llm.Request) []llm.Event { t.Error("codex must not be reached"); return nil }}
	mimo := &funcProvider{script: func(int, llm.Request) []llm.Event { return []llm.Event{say("on mimo")} }}
	old := resolveRoute
	resolveRoute = func(ref string) (llm.Provider, string, error) {
		if strings.HasPrefix(ref, "opencode-go/") {
			return mimo, "mimo-v2.6-pro", nil
		}
		return codex, "gpt-6.1-sol", nil
	}
	defer func() { resolveRoute = old }()
	tree := &Tree{}
	node := tree.Child(tree.Root("root", "m", ""), "worker", "codex/gpt-6.1-sol", t.TempDir())
	a := New(Config{Provider: codex, Model: "gpt-6.1-sol", Tree: tree, Node: node, Agent: node.ID, Cwd: t.TempDir(),
		Admission: ad, Routes: []string{"codex:gpt-6.1-sol", "opencode-go:mimo-v2.6-pro"}, Trace: &strings.Builder{}})
	final, err := a.Run(context.Background(), "work")
	if err != nil || final != "on mimo" || node.Model != "opencode-go/mimo-v2.6-pro" {
		t.Fatalf("final=%q err=%v model=%s", final, err, node.Model)
	}
}

func TestNonCodexRootConsumesNoSlot(t *testing.T) {
	ad := testAdmission(t, admission.PriorityRoot)
	h, err := ad.Manager.Acquire(context.Background(), admission.Request{Provider: "codex", Model: "gpt-6.1-sol", Priority: admission.PriorityRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	p := &funcProvider{script: func(int, llm.Request) []llm.Event { return []llm.Event{say("ok")} }}
	a := New(Config{Provider: p, Model: "openai/gpt-6.1-sol", ProviderName: "openrouter", Admission: ad, Cwd: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if final, err := a.Run(ctx, "hi"); err != nil || final != "ok" {
		t.Fatalf("final=%q err=%v", final, err)
	}
}

func TestChildAdmissionNeverInheritsExemption(t *testing.T) {
	parent := Admission{Priority: admission.PriorityRoot, Exemption: admission.ExemptionCouncilCoordinatorSol}
	c := childAdmission(parent, "delegated")
	if c.Exemption != admission.ExemptionNone || c.Priority != admission.PriorityChild {
		t.Fatalf("child admission = %+v", c)
	}
}

func TestAdaptHistoryDropsForeignReasoningKeepsToolCalls(t *testing.T) {
	calls := []llm.ToolCall{{ID: "c1", Name: "write", Args: `{"path":"a"}`}}
	hist := []llm.Message{
		{Role: llm.RoleUser, Text: "go"},
		{Role: llm.RoleAssistant, Text: "writing", ToolCalls: calls, Native: `[{"type":"reasoning","encrypted_content":"x"}]`, Reasoning: "think", Route: "codex/gpt-6.1-sol"},
		{Role: llm.RoleTool, ToolCallID: "c1", Text: "ok"},
	}
	got := adaptHistory(hist, "openrouter/anthropic/claude-sonnet-5.5")
	if got[1].Native != "" || got[1].Reasoning != "" || len(got[1].ToolCalls) != 1 || got[1].Text != "writing" {
		t.Fatalf("adapted turn = %+v", got[1])
	}
	if hist[1].Native == "" {
		t.Fatal("source history was modified")
	}
	if same := adaptHistory(hist, "codex/gpt-6.1-sol"); same[1].Native == "" {
		t.Fatal("same-route history must keep its native payload")
	}
}
