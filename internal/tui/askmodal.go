// The ask_user modal: keys and rendering for the question at the head of the
// ask queue. Its progress lives in the askItem, so a permission prompt that
// preempts it (and is answered) hands back exactly the screen the user left.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/unix/ugh/internal/agent"
)

func (it *askItem) rows() int { return len(it.req.Questions[it.page].Options) + 1 }

// writeInRow is the index of the "Other" row of the current question.
func (it *askItem) writeInRow() int { return len(it.req.Questions[it.page].Options) }

func (s *State) askKey(it *askItem, msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	defer s.refresh()
	q := it.req.Questions[it.page]
	if it.typing {
		switch k := msg.String(); k {
		case "enter":
			text := strings.TrimSpace(it.typed)
			it.typing = false
			it.other[it.page] = text
			if text == "" {
				return s, nil
			}
			if !q.MultiSelect {
				it.picked[it.page] = map[int]bool{}
			}
			s.askAdvance(it)
		case "esc":
			it.typing, it.typed = false, ""
		case "backspace":
			if r := []rune(it.typed); len(r) > 0 {
				it.typed = string(r[:len(r)-1])
			}
		case "space":
			it.typed += " "
		default:
			if msg.Text != "" {
				it.typed += msg.Text
			}
		}
		return s, nil
	}
	switch k := msg.String(); k {
	case "esc":
		s.askFinish(it, agent.AskAnswer{Declined: true}, "declined")
	case "up":
		if it.cursor[it.page] > 0 {
			it.cursor[it.page]--
		}
	case "down":
		if it.cursor[it.page] < it.rows()-1 {
			it.cursor[it.page]++
		}
	case "shift+tab":
		if it.page > 0 {
			it.page--
		}
	case "space", "enter":
		row := it.cursor[it.page]
		if row == it.writeInRow() {
			it.typing, it.typed = true, it.other[it.page]
			break
		}
		if k == "space" && !q.MultiSelect {
			break
		}
		switch {
		case q.MultiSelect && k == "space":
			if it.picked[it.page][row] {
				delete(it.picked[it.page], row)
			} else {
				it.picked[it.page][row] = true
			}
		case q.MultiSelect: // enter confirms; with nothing chosen it takes the highlighted row
			if len(it.picked[it.page]) == 0 && strings.TrimSpace(it.other[it.page]) == "" {
				it.picked[it.page][row] = true
			}
			s.askAdvance(it)
		default:
			it.picked[it.page] = map[int]bool{row: true}
			it.other[it.page] = ""
			s.askAdvance(it)
		}
	default:
		if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
			if i := int(k[0] - '1'); i < it.rows() {
				it.cursor[it.page] = i
			}
		}
	}
	return s, nil
}

// askAdvance moves to the next question, or submits after the last.
func (s *State) askAdvance(it *askItem) {
	if it.page < len(it.req.Questions)-1 {
		it.page++
		return
	}
	ans := it.answer()
	s.askFinish(it, ans, "")
}

// answer collects the replies in question order.
func (it *askItem) answer() agent.AskAnswer {
	ans := agent.AskAnswer{}
	for i, q := range it.req.Questions {
		var r agent.AskReply
		for j, o := range q.Options {
			if it.picked[i][j] {
				r.Labels = append(r.Labels, o.Label)
			}
		}
		r.Other = it.other[i]
		ans.Replies = append(ans.Replies, r)
	}
	return ans
}

// askFinish completes the item, removes it from the queue and leaves a
// transcript line per question.
func (s *State) askFinish(it *askItem, ans agent.AskAnswer, note string) {
	it.complete(ans)
	s.asks.remove(it)
	who := it.req.Agent
	if ans.Declined || len(ans.Replies) != len(it.req.Questions) {
		s.addEntry(entry{kind: "info", text: fmt.Sprintf("· ❓ %s asked %d question(s) — declined", who, len(it.req.Questions))})
		return
	}
	for i, q := range it.req.Questions {
		s.addEntry(entry{kind: "info", text: fmt.Sprintf("· ❓ %s · %s → %s", who, q.Question, replyText(ans.Replies[i]))})
	}
}

func replyText(r agent.AskReply) string {
	var parts []string
	if len(r.Labels) > 0 {
		parts = append(parts, strings.Join(r.Labels, ", "))
	}
	if r.Other != "" {
		parts = append(parts, fmt.Sprintf("other: %q", r.Other))
	}
	if len(parts) == 0 {
		return "(no answer)"
	}
	return strings.Join(parts, " | ")
}

func (s *State) askSheet() (string, int) {
	it := s.askActive()
	w := min(s.w-6, 96)
	q := it.req.Questions[it.page]
	var b strings.Builder
	waiting := ""
	if n := s.asks.len(); n > 1 {
		waiting = fmt.Sprintf(" · %d more waiting", n-1)
	}
	b.WriteString(s.pal.dim.Render(fmt.Sprintf("❓ %s asks (%d/%d)%s", it.req.Agent, it.page+1, len(it.req.Questions), waiting)) + "\n")
	b.WriteString(s.pal.user.Render(q.Header) + "  " + q.Question + "\n\n")
	for i, o := range q.Options {
		mark := "( )"
		if q.MultiSelect {
			mark = "[ ]"
		}
		if it.picked[it.page][i] {
			mark = map[bool]string{true: "[x]", false: "(•)"}[q.MultiSelect]
		}
		b.WriteString(askRow(i == it.cursor[it.page], fmt.Sprintf("%d %s %s", i+1, mark, o.Label), o.Description, s))
	}
	other := "Other…"
	switch {
	case it.typing:
		other = "Other: " + it.typed + "▏"
	case it.other[it.page] != "":
		other = "Other: " + it.other[it.page]
	}
	idx := it.writeInRow()
	mark := "( )"
	if q.MultiSelect {
		mark = "[ ]"
	}
	if it.other[it.page] != "" {
		mark = map[bool]string{true: "[x]", false: "(•)"}[q.MultiSelect]
	}
	b.WriteString(askRow(it.cursor[it.page] == idx, fmt.Sprintf("%d %s %s", idx+1, mark, other), "", s))
	hint := "↑/↓ or 1-9 move · enter choose · shift+tab back · esc decline"
	if q.MultiSelect {
		hint = "↑/↓ or 1-9 move · space toggle · enter confirm · shift+tab back · esc decline"
	}
	if it.typing {
		hint = "type your answer · enter accept · esc cancel"
	}
	b.WriteString("\n" + s.pal.dim.Render(hint))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(s.pal.modalBorder).
		Width(w).Padding(0, 1).Render(b.String())
	return box, lipgloss.Height(box)
}

func askRow(cur bool, label, desc string, s *State) string {
	marker := "  "
	if cur {
		marker = "▸ "
	}
	line := marker + label
	if desc != "" {
		line += s.pal.dim.Render(" — " + desc)
	}
	if cur {
		line = s.pal.user.Render(marker+label) + func() string {
			if desc == "" {
				return ""
			}
			return s.pal.dim.Render(" — " + desc)
		}()
	}
	return line + "\n"
}
