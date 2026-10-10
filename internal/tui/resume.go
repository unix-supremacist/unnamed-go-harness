// The resume picker (ctrl+r, /resume): this project's root sessions, newest
// activity first, scanned off the UI goroutine.
package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/unix/ugh/internal/session"
)

const resumeLimit = 100

type resumeState struct {
	seq     int // scan generation; a closed or restarted picker drops older results
	loading bool
	all     []session.Info
	items   []int // indexes into all matching the filter
	curs    int
	filter  string
}

type resumeScanMsg struct {
	seq   int
	infos []session.Info
	err   error
}

// openResume shows the picker and starts the scan.
func (s *State) openResume() tea.Cmd {
	if err := s.switchBlocked(); err != nil {
		s.addEntry(entry{kind: "info", text: "· cannot resume now: " + err.Error()})
		s.refresh()
		return nil
	}
	s.resumeSeq++
	seq, cwd := s.resumeSeq, s.cwd
	s.resume = &resumeState{seq: seq, loading: true}
	s.refresh()
	return func() tea.Msg {
		infos, err := session.ListRoots(cwd, resumeLimit)
		return resumeScanMsg{seq: seq, infos: infos, err: err}
	}
}

func (s *State) onResumeScan(m resumeScanMsg) {
	r := s.resume
	if r == nil || r.seq != m.seq {
		return // closed or superseded while scanning
	}
	r.loading = false
	if m.err != nil {
		s.addEntry(entry{kind: "error", text: "/resume: " + m.err.Error()})
		s.resume = nil
	} else {
		r.all = m.infos
		r.apply()
	}
	s.refresh()
}

func (r *resumeState) apply() {
	cur := -1
	if r.curs < len(r.items) {
		cur = r.items[r.curs]
	}
	r.items = r.items[:0]
	q := strings.ToLower(r.filter)
	for i, in := range r.all {
		hay := strings.ToLower(in.Title + " " + in.Meta.Model + " " + filepath.Base(in.Path))
		if q == "" || strings.Contains(hay, q) {
			r.items = append(r.items, i)
		}
	}
	r.curs = 0
	for i, idx := range r.items {
		if idx == cur {
			r.curs = i
		}
	}
}

func (s *State) resumeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	r := s.resume
	switch k := msg.String(); k {
	case "up":
		if r.curs > 0 {
			r.curs--
		}
	case "down":
		if r.curs < len(r.items)-1 {
			r.curs++
		}
	case "pgup":
		r.curs = max(0, r.curs-8)
	case "pgdown":
		r.curs = min(max(0, len(r.items)-1), r.curs+8)
	case "enter":
		if r.curs >= len(r.items) {
			break
		}
		in := r.all[r.items[r.curs]]
		if in.Err != nil {
			break
		}
		s.resume = nil
		if err := s.switchSession(s.resumeSession(in.Path)); err != nil {
			s.addEntry(entry{kind: "error", text: "/resume: " + err.Error()})
		}
	case "backspace":
		if r.filter != "" {
			rs := []rune(r.filter)
			r.filter = string(rs[:len(rs)-1])
			r.apply()
		}
	case "esc", "ctrl+r":
		s.resume = nil
	default:
		if msg.Text != "" && msg.Text != " " {
			r.filter += msg.Text
			r.apply()
		}
	}
	s.refresh()
	return s, nil
}

// resumeN resumes the n-th session in the picker's order (1-based).
func (s *State) resumeN(n int) {
	roots, err := session.ListRoots(s.cwd, resumeLimit)
	switch {
	case err != nil:
		s.addEntry(entry{kind: "error", text: "/resume: " + err.Error()})
	case n < 1 || n > len(roots):
		s.addEntry(entry{kind: "error", text: fmt.Sprintf("/resume: pick 1-%d", len(roots))})
	case roots[n-1].Err != nil:
		s.addEntry(entry{kind: "error", text: "/resume: unreadable session: " + roots[n-1].Err.Error()})
	default:
		if err := s.switchSession(s.resumeSession(roots[n-1].Path)); err != nil {
			s.addEntry(entry{kind: "error", text: "/resume: " + err.Error()})
		}
	}
	s.refresh()
}

// ago renders a duration since t compactly (3h ago).
func ago(t time.Time, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

func (s *State) resumeSheet() (string, int) {
	w := min(s.w-6, 110)
	visible := max(2, min(s.h-10, 10))
	r := s.resume
	start := 0
	if r.curs >= visible {
		start = r.curs - visible + 1
	}
	end := min(len(r.items), start+visible)
	var b strings.Builder
	b.WriteString(s.pal.dim.Render("resume — type to filter · ↑/↓ select · enter open · esc close") + "\n")
	if r.filter != "" {
		b.WriteString("filter: " + r.filter + "\n")
	}
	switch {
	case r.loading:
		b.WriteString(s.spin.View() + " scanning sessions…")
	case len(r.items) == 0:
		b.WriteString(s.pal.dim.Render("no sessions for this project"))
	}
	now := time.Now()
	for i := start; i < end; i++ {
		in := r.all[r.items[i]]
		marker := "  "
		if i == r.curs {
			marker = "▸ "
		}
		var line string
		if in.Err != nil {
			line = marker + s.pal.dim.Render(filepath.Base(in.Path)+" (unreadable: "+in.Err.Error()+")")
		} else {
			title := in.Title
			if title == "" {
				title = "(empty session)"
			}
			title = clip(title, max(20, w-52))
			line = fmt.Sprintf("%s%s · %s · %s · %d msgs", marker, title, ago(in.MTime, now), in.Meta.Model, in.Msgs)
			if i == r.curs {
				line = s.pal.user.Render(line)
			}
		}
		b.WriteString("\n" + line)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(s.pal.pickerBorder).
		Width(w).Padding(0, 1).Render(b.String())
	return box, lipgloss.Height(box)
}
