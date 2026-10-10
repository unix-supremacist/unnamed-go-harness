// View: transcript rendering (glamour markdown, compact tool lines), the
// permission modal and model-picker bottom sheets, the editor and the footer.
package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
)

// render converts one entry into cached display lines.
func (s *State) render(e *entry) []string {
	if e.kind != "usage" {
		e.text = clean(e.text)
	}
	if e.tool != nil {
		e.tool.output = clean(e.tool.output)
		e.tool.call.Args = clean(e.tool.call.Args)
	}
	switch e.kind {
	case "user":
		return []string{s.pal.user.Render("❯ " + e.text), ""}
	case "usage": // pre-styled bars: keep their colors
		return append(strings.Split(e.text, "\n"), "")
	case "info":
		return []string{s.pal.dim.Render(e.text), ""}
	case "error":
		return []string{s.pal.red.Render(e.text), ""}
	case "thinking":
		return s.thinkingLines(e.text, e.live)
	case "assistant":
		if e.live || e.text == "" {
			return []string{s.pal.dim.Render(e.text), ""}
		}
		return withBlank(s.markdown(e.text))
	case "tool":
		return withBlank(s.toolLines(e.tool))
	}
	return nil
}

func withBlank(lines []string) []string {
	return append(lines, "")
}

// markdown renders message text through glamour at the current width.
func (s *State) markdown(text string) []string {
	if text == "" {
		return nil
	}
	w := s.w - 6
	if w > 100 {
		w = 100
	}
	if w < 20 {
		w = 20
	}
	if s.md == nil || s.mdWidth != w { // setDark nils s.md on theme changes
		md, err := glamour.NewTermRenderer(
			glamour.WithWordWrap(w),
			glamour.WithStyles(s.glamStyles),
		)
		if err != nil {
			return []string{text}
		}
		s.md, s.mdWidth = md, w
	}
	out, err := s.md.Render(text)
	if err != nil {
		return []string{text}
	}
	out = strings.TrimRight(out, "\n")
	return strings.Split(out, "\n")
}

// toolLines renders one tool call: compact header, optionally the output
// (or the colored diff for edit calls).
func (s *State) toolLines(t *toolEntry) []string {
	name, detail := toolHead(t.call)
	var mark string
	switch t.status {
	case "running":
		mark = s.pal.yellow.Render("…")
	case "ok":
		mark = s.pal.green.Render("✓")
	case "stopped", "denied", "error":
		mark = s.pal.red.Render("✗")
	}
	head := fmt.Sprintf("%s %s %s  %s",
		s.pal.dot.Render("●"), name, mark, s.pal.dim.Render(detail))
	if len(t.flagged) > 0 {
		head += s.pal.dim.Render(" ⚑ " + strings.Join(t.flagged, ","))
	}
	if t.elapsed > 0 {
		head += s.pal.dim.Render(fmt.Sprintf(" %s", t.elapsed.Round(10*time.Millisecond)))
	} else if t.status == "running" && !t.started.IsZero() {
		head += s.pal.dim.Render(fmt.Sprintf(" %s", time.Since(t.started).Round(time.Second)))
	}
	lines := []string{head}
	if t.status == "running" && t.tail != "" { // live output of a running command
		for _, l := range strings.Split(clean(t.tail), "\n") {
			lines = append(lines, s.pal.dim.Render("  │ "+l))
		}
	}
	if !s.expanded {
		return lines
	}
	out := t.output
	if out == "" && t.status == "running" {
		out = detail
	}
	if out == "" || out == detail {
		return lines
	}
	// Edit calls show their real old → new change as a colored diff instead
	// of the one-line summary.
	if name == "edit" && t.status != "running" {
		return append(lines, withBlank(s.editDiffBlock(t))...)
	}
	split := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(split) > 100 {
		split = append(append([]string{}, split[:100]...), "…["+
			fmt.Sprint(len(split)-100)+" more lines]")
	}
	for _, l := range split {
		lines = append(lines, s.pal.expanded.Render("  "+l))
	}
	return lines
}

// toolHead summarizes a call's arguments for the compact line.
func toolHead(call llm.ToolCall) (string, string) {
	var m map[string]any
	json.Unmarshal([]byte(call.Args), &m) // malformed args fall through
	detail := ""
	switch {
	case call.Name == "bash" && m["command"] != nil:
		detail, _ = m["command"].(string)
	case m["path"] != nil:
		detail, _ = m["path"].(string)
	case m["query"] != nil:
		detail, _ = m["query"].(string)
	case m["pattern"] != nil:
		detail, _ = m["pattern"].(string)
	default:
		detail = strings.Join(strings.Fields(call.Args), " ")
	}
	detail = strings.TrimSpace(detail)
	if len(detail) > 70 {
		detail = detail[:70] + "…"
	}
	return call.Name, detail
}

// ---- sheet (modal + picker) ----

// sheetView renders the active bottom sheet (permission modal or model
// picker). The sheet is empty/zero-height when inactive.
func (s *State) sheetView() (string, int) {
	switch {
	case s.modal != nil:
		return s.modalSheet()
	case s.askActive() != nil:
		return s.askSheet()
	case s.picker != nil:
		return s.pickerSheet()
	case s.resume != nil:
		return s.resumeSheet()
	}
	return "", 0
}

func (s *State) modalSheet() (string, int) {
	req := s.modal
	w := s.w - 6
	if w > 96 {
		w = 96
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠ permission  agent=%s  cwd=%s\n", orDash(req.Call.Agent), orDash(req.Call.Cwd))
	b.WriteString(s.verdictLines(*req))
	fmt.Fprintf(&b, "\n[y] allow once   [a] allow for session   [n] deny   [s] stop run")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(s.pal.modalBorder).
		Width(w).Padding(0, 1).
		Render(s.toolDetailBlock(*req) + b.String())
	return box, lipgloss.Height(box)
}

// toolDetailBlock shows the call's command/args in full for judging.
func (s *State) toolDetailBlock(req perm.UIRequest) string {
	c := req.Call
	head := c.Tool
	if c.Command != "" {
		head += "  " + s.pal.expanded.Render(c.Command)
	} else if args := string(c.Args); strings.TrimSpace(args) != "{}" && strings.TrimSpace(args) != "" {
		head += "  " + s.pal.expanded.Render(args)
	}
	return head + "\n"
}

func (s *State) verdictLines(req perm.UIRequest) string {
	var b strings.Builder
	vs := req.Verdicts
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].Classifier < vs[j].Classifier })
	for _, v := range vs {
		name := s.pal.dim.Render(fmt.Sprintf("%-16s", v.Classifier))
		switch {
		case v.Err != nil:
			fmt.Fprintf(&b, "%s error: %v\n", name, v.Err)
		case v.Flagged:
			fmt.Fprintf(&b, "%s FLAG %s\n", name, s.pal.red.Render(v.Reason))
		default:
			fmt.Fprintf(&b, "%s ok\n", name)
		}
	}
	return b.String()
}

func orDash(s_ string) string {
	if strings.TrimSpace(s_) == "" {
		return "."
	}
	return s_
}

func (s *State) pickerSheet() (string, int) {
	w := s.w - 6
	if w > 96 {
		w = 96
	}
	visible := s.h - 10 // leave room for transcript, editor and footer
	if visible > 10 {
		visible = 10
	}
	if visible < 2 {
		visible = 2
	}
	items := s.picker.items
	// keep the cursor in view
	start := 0
	if s.picker.curs >= visible {
		start = s.picker.curs - visible + 1
	}
	end := len(items)
	if end > start+visible {
		end = start + visible
	}
	var b strings.Builder
	p := s.picker
	head := "model — type to filter · ↑/↓ select · enter choose · tab " + map[bool]string{true: "show all", false: "scoped only"}[p.onlyScoped] + " · esc close"
	if p.compact {
		head = "compaction model — type to filter · ↑/↓ select · enter choose (default = conversation model) · esc close"
	}
	if p.scopedEdit {
		head = "scoped models — type to filter · ↑/↓ · space/enter toggle · ctrl+a all shown · ctrl+x none · esc close (none = no models)"
	}
	b.WriteString(s.pal.dim.Render(head) + "\n")
	if p.filter != "" {
		b.WriteString("filter: " + p.filter + "\n")
	}
	if s.picker.loading {
		b.WriteString(s.spin.View() + " loading models…")
	}
	if s.picker.err != "" {
		b.WriteString(s.pal.red.Render("model list: "+s.picker.err+" (type /model <ref> directly)") + "\n")
	}
	for i := start; i < end; i++ {
		marker := "  "
		if i == s.picker.curs {
			marker = "▸ "
		}
		line := marker + items[i]
		if p.scopedEdit {
			box := "[ ] "
			if p.scoped[items[i]] {
				box = "[x] "
			}
			line = marker + box + items[i]
		}
		if i == s.picker.curs {
			access := p.access[items[i]]
			if access == "" {
				access = "unknown: catalog presence does not confirm access"
			}
			line += " — " + access
			line = s.pal.user.Render(line)
		}
		b.WriteString("\n" + line)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(s.pal.pickerBorder).
		Width(w).Padding(0, 1).Render(b.String())
	return box, lipgloss.Height(box)
}

// ---- layout ----

// View composes transcript + sheet + editor + footer.
func (s *State) View() tea.View {
	s.layout()
	sheet, sheetH := s.sheetView()
	editor := s.editorView()
	footer := s.footerView()

	vpH := s.h - sheetH - len(editor) - len(footer)
	if vpH < 1 {
		vpH = 1
	}
	s.vp.SetHeight(vpH)
	transcript := s.vp.View()
	if s.treeOpen {
		pane := s.treeView()
		if s.w < 95 {
			transcript = pane
		} else {
			transcript = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(s.w-42).Height(vpH).Render(transcript), lipgloss.NewStyle().Border(lipgloss.NormalBorder(), true, false, false, true).Width(40).Height(vpH).Render(pane))
		}
	}
	parts := []string{transcript}
	if sheet != "" {
		parts = append(parts, sheet)
	}
	parts = append(parts, editor...)
	parts = append(parts, footer...)
	v := tea.NewView(strings.Join(parts, "\n"))
	v.AltScreen = true
	return v
}

// editorView is the separator line, the tab-completion hint (when active)
// and the growing textarea.
func (s *State) editorView() []string {
	sep := s.pal.dim.Render(strings.Repeat("─", maxInt(s.w, 1)))
	if s.compHint != "" {
		return []string{sep, s.pal.dim.Render(s.compHint), s.ta.View()}
	}
	return []string{sep, s.ta.View()}
}

// footerView shows status and, while running, a spinner with elapsed time.
// Context % and cost are honest: the context window comes from the model's
// card ("ctx ?" when unknown) and cost comes from the configured [pricing]
// table ("cost n/a" when absent, "included" for subscription models).
func (s *State) footerView() []string {
	ctxPart := "ctx ?"
	if s.agent != nil {
		used, limit := s.agent.ContextUsage()
		switch {
		case limit > 0:
			ctxPart = fmt.Sprintf("ctx %s/%s", tok(used), tok(limit))
		case used > 0:
			ctxPart = fmt.Sprintf("ctx %s", tok(used))
		}
	}
	left := fmt.Sprintf("%s · thinking %s · perm %s · %s in/%s out tok · %s · %s · %s",
		s.modelRef, s.thinking, s.eng.Policy.Mode,
		tok(s.usage.In), tok(s.usage.Out), ctxPart, s.costLabel(), s.cwd)
	if f := s.focus; f != nil {
		left = fmt.Sprintf("viewing %s · %s · %s · %s in/%s out tok · esc → root", f.id, f.info.Model, f.info.Status,
			tok(f.info.Usage.In), tok(f.info.Usage.Out))
	}
	if !s.running {
		return []string{s.pal.dim.Render(left)}
	}
	spin := s.spin.View()
	if spin == "" {
		spin = spinner.Dot.Frames[0]
	}
	phase := s.phase
	if phase == "" {
		phase = "thinking"
	}
	top := fmt.Sprintf("%s %s %s", s.pal.dot.Render(spin), phase,
		s.pal.dim.Render(time.Since(s.started).Round(time.Second).String()))
	return []string{top, s.pal.dim.Render(left)}
}

// costLabel prefers the cost the provider reported, else prices from the [pricing] table: a configured
// entry prices the run in USD per million tokens, an entry marked included
// reports a subscription, codex models are subscription-backed by default,
// and anything unconfigured is honestly "cost n/a".
func (s *State) costLabel() string {
	if s.usage.Cost > 0 {
		return fmt.Sprintf("cost $%.4f", s.usage.Cost)
	}
	lookup := []string{s.modelRef}
	if _, model, ok := strings.Cut(s.modelRef, "/"); ok {
		lookup = append(lookup, model)
	}
	for _, key := range lookup {
		if p, ok := s.pricing[key]; ok {
			if p.Included {
				return "cost included"
			}
			cost := float64(s.usage.In)/1e6*p.In +
				float64(s.usage.Out)/1e6*p.Out +
				float64(s.usage.Cached)/1e6*p.Cached
			return fmt.Sprintf("cost $%.4f", cost)
		}
	}
	if strings.HasPrefix(s.modelRef, "codex/") {
		return "cost included (subscription)"
	}
	return "cost n/a"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
