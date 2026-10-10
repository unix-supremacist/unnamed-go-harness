package tui

import (
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
)

func TestEditDiffBlock(t *testing.T) {
	s := &State{}
	s.initTheme("dark")
	tt := &toolEntry{call: llm.ToolCall{ID: "1", Name: "edit", Args: `{"path":"f.go","old_text":"alpha\nbeta\ngamma","new_text":"alpha\nBETA\ngamma"}`}}
	lines := s.editDiffBlock(tt)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "- beta") || !strings.Contains(joined, "+ BETA") {
		t.Fatalf("diff missing -/+ lines: %q", joined)
	}
	// unchanged context lines are prefixed with a space
	if !strings.Contains(joined, "  alpha") {
		t.Fatalf("missing context line: %q", joined)
	}
	// malformed args render nothing
	if got := s.editDiffBlock(&toolEntry{call: llm.ToolCall{ID: "2", Name: "edit", Args: "{"}}); len(got) != 0 {
		t.Fatalf("malformed args should render nothing, got %q", got)
	}
}
