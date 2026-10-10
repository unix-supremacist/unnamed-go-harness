// Frame-paced rendering and the focused-agent view.
//
// Events never re-render directly: they mark the transcript dirty and a
// ~20fps frame tick redraws. Finished entries are cached as one joined
// prefix, so a streaming token costs a render of the live tail only.
//
// Focus swaps the main transcript from root to any subagent: its whole
// history (thinking, text, every tool call with its result) is rebuilt from
// the agent tree, followed live, and what you type is sent to that agent.
package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/llm"
)

const frameEvery = 50 * time.Millisecond

type frameMsg struct{}

func frameTick() tea.Cmd {
	return tea.Tick(frameEvery, func(time.Time) tea.Msg { return frameMsg{} })
}

// startFrames starts the frame loop if it is not already running.
func (s *State) startFrames() tea.Cmd {
	if s.frameOn {
		return nil
	}
	s.frameOn = true
	return frameTick()
}

// onFrame pulls focused-agent updates, ticks running-tool timers and
// redraws when anything changed. The loop stops once idle.
func (s *State) onFrame() tea.Cmd {
	if s.focus != nil {
		s.syncFocus()
	}
	if time.Since(s.lastTimer) >= time.Second {
		s.lastTimer = time.Now()
		es := s.active()
		for i := range *es {
			if e := &(*es)[i]; e.kind == "tool" && e.tool.status == "running" {
				e.lines = nil
				s.dirty = true
			}
		}
	}
	if s.dirty {
		s.refresh()
	}
	if !s.running && s.focus == nil {
		s.frameOn = false
		return nil
	}
	return frameTick()
}

// active is the transcript on screen: root's entries or the focused agent's.
func (s *State) active() *[]entry {
	if s.focus != nil {
		return &s.focus.entries
	}
	return &s.entries
}

// invalidate drops the cached prefix (switching transcripts, resize, theme).
func (s *State) invalidate() {
	s.cache, s.cacheN = s.cache[:0], 0
}

// refresh renders stale entries, extends the cached prefix with entries that
// can no longer change, and feeds the viewport. It keeps the user's scroll
// position unless they are already at the bottom.
func (s *State) refresh() {
	s.dirty = false
	es := *s.active()
	for i := range es {
		if es[i].lines == nil {
			if i < s.cacheN {
				s.invalidate()
			}
			es[i].lines = s.render(&es[i])
		}
	}
	stable := func(e *entry) bool { return !e.live && !(e.kind == "tool" && e.tool.status == "running") }
	for s.cacheN < len(es)-1 && stable(&es[s.cacheN]) {
		if ls := es[s.cacheN].lines; len(ls) > 0 {
			s.cache = append(s.cache, strings.Join(ls, "\n")...)
			s.cache = append(s.cache, '\n')
		}
		s.cacheN++
	}
	var tail []string
	for _, e := range es[s.cacheN:] {
		tail = append(tail, e.lines...)
	}
	if f := s.focus; f != nil {
		if f.partialThinking != "" {
			tail = append(tail, s.thinkingLines(f.partialThinking, true)...)
		}
		if f.partial != "" {
			tail = append(tail, s.pal.dim.Render(clean(f.partial)), "")
		}
		if f.writing != "" {
			tail = append(tail, s.pal.dim.Render(f.writing), "")
		}
	}
	content := string(s.cache) + strings.Join(tail, "\n")
	if content == "" {
		content = " "
	}
	bottom := s.vp.AtBottom()
	s.vp.SetContent(content)
	if bottom {
		s.vp.GotoBottom()
	}
}

// thinkingLines renders a thinking block: collapsed to its tail (live) or a
// one-line marker, or in full when ctrl+o expanded tool/thinking output.
func (s *State) thinkingLines(text string, live bool) []string {
	text = strings.TrimSpace(clean(text))
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if s.expanded {
		out := []string{s.pal.dim.Render("✻ thinking")}
		for _, l := range lines {
			out = append(out, s.pal.dim.Render("  "+l))
		}
		return append(out, "")
	}
	if live {
		tail := lines
		if len(tail) > 3 {
			tail = tail[len(tail)-3:]
		}
		out := []string{s.pal.dim.Render("✻ thinking…")}
		for _, l := range tail {
			out = append(out, s.pal.dim.Render("  "+l))
		}
		return append(out, "")
	}
	return []string{s.pal.dim.Render("✻ thought for " + plural(len(lines), "line") + " · ctrl+o expands"), ""}
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return strings.TrimSpace(strings.Join([]string{itoa(n), w + "s"}, " "))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// appendThinking streams root's thinking into a live thinking entry.
func (s *State) appendThinking(t string) {
	if n := len(s.entries); n > 0 && s.entries[n-1].kind == "thinking" && s.entries[n-1].live {
		s.entries[n-1].text += t
		s.entries[n-1].lines = nil
		return
	}
	s.addEntry(entry{kind: "thinking", text: t, live: true})
}

// endThinking collapses root's live thinking once text or a tool follows.
func (s *State) endThinking() {
	if n := len(s.entries); n > 0 && s.entries[n-1].kind == "thinking" && s.entries[n-1].live {
		s.entries[n-1].live = false
		s.entries[n-1].lines = nil
	}
}

// ---- focus ----

type focusState struct {
	id                       string
	info                     agent.Info
	entries                  []entry
	n                        int            // messages consumed from the node
	toolIdx                  map[string]int // tool call id → entry index
	partial, partialThinking string
	writing                  string
}

// setFocus shows agent id's transcript ("" or root's id returns to root).
func (s *State) setFocus(id string) tea.Cmd {
	s.invalidate()
	if id == "" || (s.agent != nil && id == s.agent.ID) {
		s.focus = nil
		for i := range s.entries {
			s.entries[i].lines = nil
		}
		s.refresh()
		s.vp.GotoBottom()
		return nil
	}
	v, ok := s.runtime.Tree.View(id, 0)
	if !ok {
		s.addEntry(entry{kind: "error", text: "no agent " + id + " (ctrl+t lists them)"})
		s.refresh()
		return nil
	}
	s.focus = &focusState{id: id, toolIdx: map[string]int{}}
	s.focus.entries = []entry{{kind: "info", text: "── viewing " + id + " · " + v.Model + " · " + v.Cwd +
		"\n   esc returns to root · what you type is sent to this agent (read at its next turn)"}}
	s.syncFocus()
	s.refresh()
	s.vp.GotoBottom()
	return s.startFrames()
}

// syncFocus appends the focused agent's new messages and live partials.
func (s *State) syncFocus() {
	f := s.focus
	v, ok := s.runtime.Tree.View(f.id, f.n)
	if !ok {
		return
	}
	if v.Status != f.info.Status {
		s.dirty = true
	}
	f.info = v.Info
	for _, m := range v.Messages {
		s.focusAppend(m)
		s.dirty = true
	}
	f.n += len(v.Messages)
	writing := ""
	if v.Writing != "" {
		writing = "✎ writing " + v.Writing + " call · " + tok(v.WritingChars) + " chars"
	}
	if v.Partial != f.partial || v.PartialThinking != f.partialThinking || writing != f.writing {
		f.partial, f.partialThinking, f.writing = v.Partial, v.PartialThinking, writing
		s.dirty = true
	}
}

// focusAppend turns one message into the focused agent's transcript entries.
func (s *State) focusAppend(m llm.Message) {
	f := s.focus
	appendMessage(&f.entries, f.toolIdx, m)
}

// appendMessage turns one message into transcript entries: tool calls become
// running tool entries (indexed by call id) and their results fill them in.
func appendMessage(es *[]entry, idx map[string]int, m llm.Message) {
	switch m.Role {
	case llm.RoleUser:
		*es = append(*es, entry{kind: "user", text: m.Text})
	case llm.RoleAssistant:
		if strings.TrimSpace(m.Reasoning) != "" {
			*es = append(*es, entry{kind: "thinking", text: m.Reasoning})
		}
		if strings.TrimSpace(m.Text) != "" {
			*es = append(*es, entry{kind: "assistant", text: m.Text})
		}
		for _, c := range m.ToolCalls {
			idx[c.ID] = len(*es)
			*es = append(*es, entry{kind: "tool", tool: &toolEntry{call: c, status: "running", started: time.Now(), output: strings.TrimSpace(c.Args)}})
		}
	case llm.RoleTool:
		i, ok := idx[m.ToolCallID]
		if !ok {
			*es = append(*es, entry{kind: "info", text: "← " + m.ToolName + ": " + m.Text})
			return
		}
		t := (*es)[i].tool
		t.output, t.status = m.Text, "ok"
		switch {
		case strings.HasPrefix(m.Text, "error:"):
			t.status = "error"
		case strings.HasPrefix(m.Text, "permission denied"):
			t.status = "denied"
		}
		(*es)[i].lines = nil
		delete(idx, m.ToolCallID)
	}
}

// clean makes model or tool text safe to draw: no ANSI escapes, no carriage
// returns or other control characters (a stray \r desyncs the renderer and
// leaves stale cells on screen), tabs expanded.
func clean(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, strings.ReplaceAll(s, "\t", "    "))
}
