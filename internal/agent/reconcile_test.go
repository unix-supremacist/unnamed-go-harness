package agent

import (
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
)

func TestReconcileHistoryAddsAmbiguousResults(t *testing.T) {
	h := []llm.Message{
		{Role: llm.RoleUser, Text: "go"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a", Name: "bash"}, {ID: "b", Name: "edit"}}},
		{Role: llm.RoleTool, ToolCallID: "a", Text: "ok"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c", Name: "bash"}}},
	}
	got := reconcileHistory(h)
	if len(got) != 6 {
		t.Fatalf("len %d: %+v", len(got), got)
	}
	if got[3].ToolCallID != "b" || !strings.Contains(got[3].Text, "outcome unknown") || got[2].Text != "ok" {
		t.Fatalf("b not reconciled after a: %+v", got)
	}
	if got[5].ToolCallID != "c" || got[5].Role != llm.RoleTool {
		t.Fatalf("trailing call not reconciled: %+v", got)
	}
	if len(h) != 4 {
		t.Fatal("input modified")
	}
	complete := h[:3:3]
	complete = append(complete, llm.Message{Role: llm.RoleTool, ToolCallID: "b", Text: "x"})
	if r := reconcileHistory(complete); len(r) != 4 {
		t.Fatalf("complete history changed: %+v", r)
	}
}
