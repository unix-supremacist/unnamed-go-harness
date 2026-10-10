package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/perm"
)

func TestViewLinesFitAndHaveNoControlChars(t *testing.T) {
	s := testState(t)
	s.eng = &perm.Engine{}
	s.w, s.h = 110, 36
	s.vp.SetWidth(110)
	s.modelRef, s.thinking = "x/y", "off"
	s.addEntry(entry{kind: "user", text: strings.Repeat("long user words ", 20)})
	s.applyEvent(agent.Event{Kind: "thinking", Text: "func main() {\n\t\t\tfmt.Println(\"x\")\r\n" + strings.Repeat("\tword", 30)})
	s.refresh()
	v := s.View()
	for i, l := range strings.Split(v.Content, "\n") {
		if strings.ContainsAny(l, "\t\r") {
			t.Errorf("line %d has raw control chars: %q", i, ansi.Strip(l))
		}
		if w := ansi.StringWidth(l); w > 110 {
			t.Errorf("line %d width %d: %q", i, w, ansi.Strip(l)[:60])
		}
	}
}
