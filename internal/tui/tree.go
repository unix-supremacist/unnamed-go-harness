package tui

import (
	"fmt"
	"strings"
)

// treeView is the ctrl+t pane: every agent with its live status; enter
// switches the main transcript to the selected agent.
func (s *State) treeView() string {
	nodes := s.runtime.Tree.Outline()
	if s.treeSelected >= len(nodes) {
		s.treeSelected = len(nodes) - 1
	}
	if s.treeSelected < 0 {
		s.treeSelected = 0
	}
	var b strings.Builder
	b.WriteString("agents · ↑↓ select · enter view · esc close\n")
	for i, n := range nodes {
		marker := "  "
		if i == s.treeSelected {
			marker = "▸ "
		}
		if s.focus != nil && n.ID == s.focus.id {
			marker = "◉ "
		}
		name := n.ID
		if j := strings.LastIndex(name, "/"); j >= 0 {
			name = name[j+1:]
		}
		fmt.Fprintf(&b, "%s%s%s  %s  %s\n", marker, strings.Repeat("  ", n.Depth), name, n.Status, tok(n.Usage.In+n.Usage.Out))
		if s.w >= 95 {
			fmt.Fprintf(&b, "   %s%s\n", strings.Repeat("  ", n.Depth), shortRef(n.Model))
		}
	}
	return b.String()
}

func shortRef(m string) string {
	if i := strings.Index(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}
