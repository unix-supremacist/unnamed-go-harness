// `ugh watch` and `ugh log`: follow a run from outside the process that runs
// it. Every agent appends one JSONL record per message to its session file,
// so a second ugh can show live per-agent activity without touching the run.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/provstate"
	"github.com/unix/ugh/internal/session"
	"golang.org/x/term"
)

// tail incrementally reads one session file and keeps a live summary of it.
type tail struct {
	path    string
	off     int64
	rest    string // partial last line
	meta    session.Record
	result  *session.Record
	waiting string // "waiting on …" while blocked on children
	turns   int
	tools   int
	last    session.Record // most recent message record
	lastAt  time.Time
}

// poll reads records appended since the last call and hands each to fn.
func (t *tail) poll(fn func(session.Record)) error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	t.off += int64(len(b))
	data := t.rest + string(b)
	lines := strings.Split(data, "\n")
	t.rest = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		var r session.Record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		t.apply(r)
		if fn != nil {
			fn(r)
		}
	}
	return nil
}

func (t *tail) apply(r session.Record) {
	ts, _ := time.Parse(time.RFC3339Nano, r.TS)
	switch {
	case r.Type == "meta" && t.meta.Type == "":
		t.meta = r
	case r.Type == "result":
		rr := r
		t.result = &rr
	case r.Type == "compaction":
		t.lastAt = ts
	case r.Type == "status":
		t.waiting = r.Status
	case r.Message != nil:
		t.waiting = ""
		if r.Message.Role == llm.RoleAssistant {
			t.turns++
			t.tools += len(r.Message.ToolCalls)
		}
		t.last, t.lastAt = r, ts
	}
}

func (t *tail) id() string {
	if t.meta.Agent == "" {
		return "root"
	}
	return t.meta.Agent
}

// status is the result status, or queued/running from the activity so far.
func (t *tail) status() string {
	if t.result != nil {
		return t.result.Status
	}
	if t.waiting == "queued" {
		return "queued"
	}
	if t.waiting != "" {
		return "waiting"
	}
	m := t.last.Message
	switch {
	case m == nil:
		return "queued"
	case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 && m.ToolCalls[len(m.ToolCalls)-1].Name == "spawn":
		return "waiting" // blocked on its children
	case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
		return "tool"
	default:
		return "thinking" // a model turn is streaming
	}
}

// activity describes the agent's latest step in one line.
func (t *tail) activity() string {
	if t.waiting == "queued" {
		return "waiting for a provider slot or premium admission"
	}
	if t.waiting != "" {
		return t.waiting
	}
	m := t.last.Message
	switch {
	case m == nil:
		return ""
	case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
		c := m.ToolCalls[len(m.ToolCalls)-1]
		return "→ " + c.Name + " " + oneLine(argSummary(c.Args))
	case m.Role == llm.RoleTool:
		return "after " + m.ToolName + ": " + oneLine(m.Text)
	case m.Role == llm.RoleAssistant:
		return "“" + oneLine(m.Text) + "”"
	case m.Role == llm.RoleUser:
		return "task: " + oneLine(m.Text)
	}
	return ""
}

// argSummary shows the most telling argument of a tool call.
func argSummary(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return args
	}
	for _, k := range []string{"command", "path", "pattern", "task"} {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	if ts, ok := m["tasks"].([]any); ok {
		return fmt.Sprintf("%d tasks", len(ts))
	}
	return args
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// runFiles lists the session files of one run (or, for runs that predate run
// ids, files touched since `since`).
func runFiles(run string, since time.Time) []string {
	base, err := session.StateDir()
	if err != nil {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
	var out []string
	for _, p := range paths {
		if run == "" {
			if st, err := os.Stat(p); err == nil && st.ModTime().After(since) {
				out = append(out, p)
			}
			continue
		}
		if m, err := firstRecord(p); err == nil && m.Run == run {
			out = append(out, p)
		}
	}
	return out
}

// pickRun resolves --session or, by default, the most recently written
// session file's run.
func pickRun(sess string) (string, error) {
	if sess == "" {
		base, err := session.StateDir()
		if err != nil {
			return "", err
		}
		paths, _ := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
		var newest time.Time
		for _, p := range paths {
			if st, err := os.Stat(p); err == nil && st.ModTime().After(newest) {
				newest, sess = st.ModTime(), p
			}
		}
		if sess == "" {
			return "", fmt.Errorf("no sessions yet")
		}
	}
	m, err := firstRecord(sess)
	if err != nil {
		return "", err
	}
	return m.Run, nil
}

func watchCmd(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	sess := fs.String("session", "", "any session file of the run (default: the most recently active run)")
	every := fs.Duration("interval", 2*time.Second, "refresh interval")
	once := fs.Bool("once", false, "print one snapshot and exit (automatic when stdout is not a terminal)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Piped or captured output gets one plain frame: repeated frames with
	// clear-screen codes are only useful on a terminal.
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		*once = true
	}
	run, err := pickRun(*sess)
	if err != nil {
		return err
	}
	since := time.Now().Add(-12 * time.Hour)
	tails := map[string]*tail{}
	for {
		for _, p := range runFiles(run, since) {
			if tails[p] == nil {
				tails[p] = &tail{path: p}
			}
		}
		for _, t := range tails {
			_ = t.poll(nil)
		}
		width := 160
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
			width = w
		}
		var b strings.Builder
		renderWatch(&b, run, tails, time.Now(), width)
		if *once {
			fmt.Print(b.String())
			return nil
		}
		fmt.Print("\x1b[H\x1b[2J" + b.String())
		time.Sleep(*every)
	}
}

// renderWatch draws the live tree: one line per agent with its status, turns,
// tool calls, idle time and latest activity.
func renderWatch(w io.Writer, run string, tails map[string]*tail, now time.Time, width int) {
	byID := map[string]*tail{}
	for _, t := range tails {
		if old := byID[t.id()]; old == nil || t.lastAt.After(old.lastAt) {
			byID[t.id()] = t
		}
	}
	kids := map[string][]*tail{}
	var roots []*tail
	for _, t := range byID {
		if p := t.meta.Parent; p != "" && byID[p] != nil && p != t.id() {
			kids[p] = append(kids[p], t)
		} else {
			roots = append(roots, t)
		}
	}
	sortTails := func(ts []*tail) { sort.Slice(ts, func(i, j int) bool { return ts[i].id() < ts[j].id() }) }
	sortTails(roots)
	label := run
	if label == "" {
		label = "(no run id: sessions active in the last 12h)"
	}
	counts := map[string]int{}
	for _, t := range byID {
		counts[t.status()]++
	}
	var parts []string
	for _, s := range []string{"thinking", "tool", "waiting", "queued", "ok", "turn-out", "error", "cancelled"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	fmt.Fprintf(w, "ugh watch · run %s · %s · %s\n", label, strings.Join(parts, " · "), now.Format("15:04:05"))
	if p := provstate.Summary(); p != "" {
		fmt.Fprintf(w, "providers: %s\n", p)
	}
	fmt.Fprintln(w)
	var walk func(t *tail, depth int)
	walk = func(t *tail, depth int) {
		idle := "-"
		if !t.lastAt.IsZero() {
			idle = now.Sub(t.lastAt).Round(time.Second).String()
		}
		head := fmt.Sprintf("%s%-*s %-9s t%-3d %3d tools  %6s  %s", strings.Repeat("  ", depth), 28-2*depth, t.id(),
			t.status(), t.turns, t.tools, idle, shortModel(t.meta.Model))
		fmt.Fprintln(w, clip(head, width))
		if t.result == nil {
			if a := t.activity(); a != "" {
				fmt.Fprintln(w, clip(strings.Repeat("  ", depth)+"    "+a, width))
			}
		}
		ks := kids[t.id()]
		sortTails(ks)
		for _, k := range ks {
			walk(k, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	fmt.Fprintln(w, "\nugh log <agent-id> -f follows one agent · ctrl+c quits")
}

func shortModel(m string) string {
	if i := strings.Index(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

func clip(s string, width int) string {
	r := []rune(s)
	if len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return s
}

func logCmd(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	sess := fs.String("session", "", "any session file of the run, to resolve agent ids (default: the most recently active run)")
	follow := fs.Bool("f", false, "keep following new messages")
	full := fs.Bool("full", false, "print tool results in full instead of the first lines")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: ugh log [-f] [--full] [--session FILE] <agent-id | session file>")
	}
	target := pos[0]
	path := target
	if _, err := os.Stat(target); err != nil {
		run, err := pickRun(*sess)
		if err != nil {
			return err
		}
		path = ""
		var newest time.Time
		for _, p := range runFiles(run, time.Now().Add(-12*time.Hour)) {
			m, err := firstRecord(p)
			id := m.Agent
			if id == "" {
				id = "root"
			}
			if err != nil || id != target {
				continue
			}
			if st, err := os.Stat(p); err == nil && st.ModTime().After(newest) {
				newest, path = st.ModTime(), p
			}
		}
		if path == "" {
			return fmt.Errorf("no session file for agent %q in run %s (see ugh watch for ids)", target, run)
		}
	}
	out := bufio.NewWriter(os.Stdout)
	t := &tail{path: path}
	print := func(r session.Record) { printRecord(out, r, *full) }
	for {
		if err := t.poll(print); err != nil {
			return err
		}
		out.Flush()
		if !*follow || t.result != nil {
			return nil
		}
		time.Sleep(time.Second)
	}
}

// parseInterleaved parses flags that may come before or after positional
// arguments (`ugh log root/worker-3 -f`); the stdlib parser stops at the
// first positional one.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// printRecord renders one session record as transcript text.
func printRecord(w io.Writer, r session.Record, full bool) {
	ts := ""
	if t, err := time.Parse(time.RFC3339Nano, r.TS); err == nil {
		ts = t.Local().Format("15:04:05") + " "
	}
	switch {
	case r.Type == "meta":
		id := r.Agent
		if id == "" {
			id = "root"
		}
		fmt.Fprintf(w, "%s── %s · %s · %s\n", ts, id, r.Model, r.Cwd)
	case r.Type == "result":
		fmt.Fprintf(w, "%s── finished: %s · %d turns", ts, r.Status, r.Turns)
		if r.Error != "" {
			fmt.Fprintf(w, " · %s", r.Error)
		}
		fmt.Fprintln(w)
	case r.Type == "compaction":
		fmt.Fprintf(w, "%s── compacted to %d messages\n", ts, len(r.Seed))
	case r.Message != nil:
		m := r.Message
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(w, "\n%suser: %s\n", ts, m.Text)
		case llm.RoleAssistant:
			if strings.TrimSpace(m.Text) != "" {
				fmt.Fprintf(w, "\n%sassistant: %s\n", ts, m.Text)
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(w, "%s→ %s %s\n", ts, c.Name, oneLine(c.Args))
			}
		case llm.RoleTool:
			text := m.Text
			if !full {
				lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
				if len(lines) > 6 {
					text = strings.Join(lines[:6], "\n") + fmt.Sprintf("\n… (%d more lines, --full shows all)", len(lines)-6)
				}
			}
			fmt.Fprintf(w, "%s← %s:\n%s\n", ts, m.ToolName, indent(text, "    "))
		}
	}
}

func indent(s, pre string) string {
	return pre + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pre)
}
