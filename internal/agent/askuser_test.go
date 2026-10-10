package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/tools"
)

func askArgs(t *testing.T, qs ...AskQuestion) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"questions": qs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func twoOpts(labels ...string) []AskOption {
	var out []AskOption
	for _, l := range labels {
		out = append(out, AskOption{Label: l, Description: "d"})
	}
	return out
}

func TestAskUserValidation(t *testing.T) {
	good := AskQuestion{Question: "Which?", Header: "Pick", Options: twoOpts("A", "B")}
	cases := map[string][]AskQuestion{
		"none":            nil,
		"five questions":  {good, good, good, good, good},
		"empty question":  {{Header: "h", Options: twoOpts("A", "B")}},
		"long header":     {{Question: "q?", Header: "thirteen char", Options: twoOpts("A", "B")}},
		"one option":      {{Question: "q?", Header: "h", Options: twoOpts("A")}},
		"seven options":   {{Question: "q?", Header: "h", Options: twoOpts("A", "B", "C", "D", "E", "F", "G")}},
		"reserved other":  {{Question: "q?", Header: "h", Options: twoOpts("A", "other")}},
		"duplicate label": {{Question: "q?", Header: "h", Options: twoOpts("A", "a")}},
		"empty label":     {{Question: "q?", Header: "h", Options: twoOpts("A", " ")}},
	}
	for name, qs := range cases {
		if err := validateAsk(qs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateAsk([]AskQuestion{good}); err != nil {
		t.Fatal(err)
	}
}

func askAgent(t *testing.T, ask func(context.Context, AskRequest) (AskAnswer, error)) *Agent {
	t.Helper()
	return New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir(), Agent: "root/2",
		Tools: tools.NewRegistry(), Context: ContextConfig{AskUser: ask}})
}

func TestAskUserToolResultAndHeadless(t *testing.T) {
	var got AskRequest
	a := askAgent(t, func(_ context.Context, r AskRequest) (AskAnswer, error) {
		got = r
		return AskAnswer{Replies: []AskReply{{Labels: []string{"A", "B"}}, {Other: "my own"}}}, nil
	})
	tool, ok := a.cfg.Tools.Get("ask_user")
	if !ok {
		t.Fatal("ask_user not offered with a UI attached")
	}
	q1 := AskQuestion{Question: "Which?", Header: "Pick", Options: twoOpts("A", "B"), MultiSelect: true}
	q2 := AskQuestion{Question: "Why?", Header: "Reason", Options: twoOpts("X", "Y")}
	out, err := tool.Run(context.Background(), "", askArgs(t, q1, q2))
	if err != nil {
		t.Fatal(err)
	}
	if out != "Which? → A, B\nWhy? → other: \"my own\"" || got.Agent != "root/2" || len(got.Questions) != 2 {
		t.Fatalf("out=%q req=%+v", out, got)
	}
	// Malformed arguments never reach the UI.
	got = AskRequest{}
	if _, err := tool.Run(context.Background(), "", json.RawMessage(`{"questions":"nope"}`)); err == nil || got.Agent != "" {
		t.Fatalf("malformed accepted: %v", err)
	}
	// Declined and UI errors read as "declined".
	a.cfg.Context.AskUser = func(context.Context, AskRequest) (AskAnswer, error) { return AskAnswer{Declined: true}, nil }
	if out, _ := tool.Run(context.Background(), "", askArgs(t, q1)); out != "declined" {
		t.Fatalf("declined = %q", out)
	}
	// Headless: no tool.
	b := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir(), Tools: tools.NewRegistry()})
	if _, ok := b.cfg.Tools.Get("ask_user"); ok {
		t.Fatal("ask_user offered without a UI")
	}
}

// ask_user waits past the 120 s file-tool timeout and stops when the run is cancelled.
func TestAskUserIsLongRunningAndCancellable(t *testing.T) {
	if !longTools["ask_user"] {
		t.Fatal("ask_user must not run under the file-tool timeout")
	}
	started := make(chan struct{})
	a := askAgent(t, func(ctx context.Context, _ AskRequest) (AskAnswer, error) {
		close(started)
		<-ctx.Done()
		return AskAnswer{Declined: true}, ctx.Err()
	})
	tool, _ := a.cfg.Tools.Get("ask_user")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.runTool(ctx, tool, "ask_user", askArgs(t, AskQuestion{Question: "q?", Header: "h", Options: twoOpts("A", "B")}))
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancel") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ask_user ignored cancellation")
	}
}

var _ = llm.Message{}
