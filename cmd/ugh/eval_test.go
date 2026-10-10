package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/config"
)

func TestReadEvalQuestion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.md")
	os.WriteFile(path, []byte("# Title\n\nquestion: What and why?\nmore question text.\nreference: Because.\n"), 0644)
	q, err := readEvalQuestion(path)
	if err != nil {
		t.Fatal(err)
	}
	if q.title != "Title" || !strings.Contains(q.question, "more question text.") || q.reference != "Because." {
		t.Fatalf("parsed %+v", q)
	}
}

func TestReadEvalQuestionRequiresKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.md")
	os.WriteFile(path, []byte("# Only a title\n"), 0644)
	if _, err := readEvalQuestion(path); err == nil {
		t.Fatal("missing question/reference must error")
	}
}

func TestReadEvalQuestionAngles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.md")
	os.WriteFile(path, []byte("# T\n\nquestion: q\nangles: evidence-map, alternatives\nreference: r\n"), 0644)
	q, err := readEvalQuestion(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(q.angles) != `["evidence-map","alternatives"]` {
		t.Fatalf("angles = %s", q.angles)
	}
	os.WriteFile(path, []byte("# T\n\nquestion: q\nangles: bogus-lens\nreference: r\n"), 0644)
	if _, err := readEvalQuestion(path); err == nil {
		t.Fatal("unknown lens must error")
	}
}

func TestEvalScopeExcludesEvalArtifacts(t *testing.T) {
	for _, want := range []string{"evals/", "reference answers", "old/"} {
		if !strings.Contains(evalScope, want) {
			t.Fatalf("evalScope missing %q", want)
		}
	}
}

func TestCouncilConfigDefaults(t *testing.T) {
	c := config.Default().Council
	if c.Aggregator != "mimo-v2.6-pro" || c.DefaultAngles != 2 || c.MaxSeats != 40 || c.SeatTimeoutS != 240 || c.MaxReportWords != 800 || c.Oracle != "gpt-sol" {
		t.Fatalf("unexpected council defaults: %+v", c)
	}
	if len(c.Panel) != 5 {
		t.Fatalf("want 5 panel defaults, got %v", c.Panel)
	}
}
