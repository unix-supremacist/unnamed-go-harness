package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAngles(t *testing.T) {
	text := "1. Should the aggregator default stay mimo-v2.6-pro instead of gpt-sol?\n2. What do the model cards say about route order for gpt-sol?\nNot numbered prose.\n3. Which providers expose static keys for the high tier card today?"
	got := parseAngles(text, 2, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 angles, got %d: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0].text, "Should the aggregator") {
		t.Fatalf("angle text wrong: %q", got[0].text)
	}
	pinned := parseAngles(text, 3, []string{"evidence-map", "adversarial", "conventions"})
	if len(pinned) != 3 || pinned[0].lens != "evidence-map" || pinned[2].lens != "conventions" {
		t.Fatalf("lens pinning wrong: %+v", pinned)
	}
}

func TestAngleSpec(t *testing.T) {
	if n, lenses := angleSpec(nil, 2); n != 2 || lenses != nil {
		t.Fatalf("default: %d %v", n, lenses)
	}
	if n, _ := angleSpec(json.RawMessage(`3`), 2); n != 3 {
		t.Fatalf("number: %d", n)
	}
	if n, lenses := angleSpec(json.RawMessage(`["evidence-map"]`), 2); n != 1 || lenses[0] != "evidence-map" {
		t.Fatalf("lens list: %d %v", n, lenses)
	}
}

func TestClampWords(t *testing.T) {
	if got := clampWords("one two three", 5); got != "one two three" {
		t.Fatalf("no truncation expected: %q", got)
	}
	if got := clampWords("a b c d", 2); !strings.HasSuffix(got, "…[truncated]") || !strings.HasPrefix(got, "a b") || strings.Contains(got, "c d") {
		t.Fatalf("truncation wrong: %q", got)
	}
}

func TestCouncilToolSpec(t *testing.T) {
	a := New(Config{})
	c := councilTool{agent: a}
	s := c.Spec()
	if s.Name != "council" || s.Parameters == nil {
		t.Fatalf("bad spec: %+v", s)
	}
	if _, err := c.Run(t.Context(), "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty question must error")
	}
	if len(a.councils) != 0 {
		t.Fatal("a council call that never started must record no outcome")
	}
	if _, err := c.Run(t.Context(), "", json.RawMessage(`invalid json`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
	if len(a.councils) != 0 {
		t.Fatal("a malformed council call must record no outcome")
	}
}

func TestSeatTaskAndSynthesis(t *testing.T) {
	task := seatTask("Q?", "angle one", "adversarial", "", 10)
	for _, want := range []string{"Core question: Q?", "Assigned lens: adversarial", "(none)", "2-3 targeted evidence checks", "read, ls, grep and find only"} {
		if !strings.Contains(task, want) {
			t.Fatalf("seat task missing %q", want)
		}
	}
	syn := synthesisTask("Q?", "", []string{"## P1\nreport"}, []string{"seat 2 failed"})
	for _, want := range []string{"Core question: Q?", "## P1", "seat 2 failed"} {
		if !strings.Contains(syn, want) {
			t.Fatalf("synthesis task missing %q", want)
		}
	}
}

// TestReadOnlyTools locks the council toolset: bounded read-only evidence
// checks only — no bash and no write/edit for any seat.
func TestReadOnlyTools(t *testing.T) {
	names := map[string]bool{}
	for _, s := range readOnlyTools().Specs() {
		names[s.Name] = true
	}
	for _, want := range []string{"read", "ls", "grep", "find"} {
		if !names[want] {
			t.Fatalf("council toolset missing %s: %v", want, names)
		}
	}
	for _, deny := range []string{"bash", "write", "edit"} {
		if names[deny] {
			t.Fatalf("council toolset must not offer %s", deny)
		}
	}
}

// TestPromptsBoundedAndNoEvalArtifacts locks the prompt seams: tool budget
// wording, no whole-repo searches and no reading of eval artifacts.
func TestPromptsBoundedAndNoEvalArtifacts(t *testing.T) {
	for name, p := range map[string]string{
		"prescout":  councilPrescoutPrompt,
		"panelist":  councilPanelistPrompt,
		"synthesis": councilSynthesisPrompt,
	} {
		if !strings.Contains(p, "Never read or cite") {
			t.Errorf("%s prompt must forbid eval artifacts", name)
		}
		if strings.Contains(p, "bash") {
			t.Errorf("%s prompt must not mention bash", name)
		}
	}
	if !strings.Contains(councilPanelistPrompt, "2-3 evidence checks") || !strings.Contains(councilPrescoutPrompt, "2-3 targeted evidence checks") {
		t.Fatal("prompts must state the tool budget")
	}
}
