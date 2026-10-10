// Slash-command table: dispatch, /help and tab completion are all generated
// from cmdTable, so a command cannot exist in one place and be missing from
// another.
package tui

import (
	"sort"
	"strings"
)

type cmdSpec struct {
	name     string   // canonical, with the leading "/"
	aliases  []string // also with the leading "/"
	args     string   // usage hint, "" when none
	desc     string
	idleOnly bool // refused while a run is in progress
}

var cmdTable = []cmdSpec{
	{name: "/model", aliases: []string{"/models"}, args: "[ref]", desc: "choose the conversation model (picker without a ref)", idleOnly: true},
	{name: "/scoped-models", desc: "choose which models /model and ctrl+p cycle"},
	{name: "/thinking", aliases: []string{"/effort", "/think"}, args: "[off|minimal|low|medium|high|xhigh|max]", desc: "set thinking per model; bare cycles"},
	{name: "/mode", aliases: []string{"/permissions", "/approvals"}, args: "[manual|classify|auto-safe|auto]", desc: "permission mode; bare cycles"},
	{name: "/ask", args: "<question>", desc: "answer from live agent status without interrupting the run"},
	{name: "/send", args: "<agent-id> <message>", desc: "message a running subagent"},
	{name: "/new", aliases: []string{"/clear", "/reset"}, desc: "start a fresh session", idleOnly: true},
	{name: "/resume", aliases: []string{"/continue", "/sessions", "/history"}, args: "[n]", desc: "pick a previous session of this project", idleOnly: true},
	{name: "/context", desc: "what the context ladder, instructions and compaction are doing", idleOnly: true},
	{name: "/compact", aliases: []string{"/summarize"}, desc: "summarize older turns in place", idleOnly: true},
	{name: "/compact-model", args: "[ref|default]", desc: "choose the model that writes compaction summaries (picker without a ref; default = conversation model)"},
	{name: "/copy", desc: "copy the last assistant message to the clipboard (OSC 52)"},
	{name: "/init", desc: "have the agent write AGENTS.md and per-directory instruction files for this project", idleOnly: true},
	{name: "/usage", aliases: []string{"/cost", "/quota"}, desc: "plan usage and spend"},
	{name: "/routing", aliases: []string{"/status"}, desc: "routing and admission status"},
	{name: "/login", desc: "log in to a provider"},
	{name: "/tree", aliases: []string{"/agents"}, desc: "toggle the agent tree"},
	{name: "/view", args: "[agent-id]", desc: "open a subagent's transcript (bare returns to root)"},
	{name: "/help", aliases: []string{"/h", "/?"}, desc: "this list"},
	{name: "/quit", aliases: []string{"/q", "/exit"}, desc: "leave ugh"},
}

// lookupCmd resolves a typed first token (name or alias) to its spec.
func lookupCmd(tok string) (cmdSpec, bool) {
	tok = strings.ToLower(tok)
	for _, c := range cmdTable {
		if c.name == tok {
			return c, true
		}
		for _, a := range c.aliases {
			if a == tok {
				return c, true
			}
		}
	}
	return cmdSpec{}, false
}

// splitCommand returns the canonical name (or the raw token when unknown) and
// the argument text, sliced from the original line so its case and spacing
// survive aliasing.
func splitCommand(line string) (name, arg string, known bool) {
	line = strings.TrimSpace(line)
	tok, rest, _ := strings.Cut(line, " ")
	if i := strings.IndexAny(tok, "\t\n"); i >= 0 {
		rest = tok[i:] + " " + rest
		tok = tok[:i]
	}
	arg = strings.TrimSpace(rest)
	if c, ok := lookupCmd(tok); ok {
		return c.name, arg, true
	}
	return tok, arg, false
}

// slashNames is the completion vocabulary: canonical names, plus aliases.
func slashNames() []string { return slashMatches("") }

// slashMatches completes a typed prefix: canonical names win, and aliases
// are offered only when no canonical name matches.
func slashMatches(prefix string) []string {
	var names, aliases []string
	for _, c := range cmdTable {
		if strings.HasPrefix(c.name, prefix) {
			names = append(names, c.name)
		}
		for _, a := range c.aliases {
			if strings.HasPrefix(a, prefix) {
				aliases = append(aliases, a)
			}
		}
	}
	if len(names) == 0 || prefix == "" {
		names = append(names, aliases...)
	}
	sort.Strings(names)
	return names
}

// helpText renders /help from the table.
func helpText() string {
	var b strings.Builder
	b.WriteString("commands:\n")
	for _, c := range cmdTable {
		head := c.name
		if c.args != "" {
			head += " " + c.args
		}
		b.WriteString("  " + head + " — " + c.desc)
		if len(c.aliases) > 0 {
			b.WriteString(" (" + strings.Join(c.aliases, ", ") + ")")
		}
		b.WriteString("\n")
	}
	b.WriteString("tab completes /commands and @file paths\n")
	b.WriteString("keys: enter send · alt+enter newline · esc cancel run · ctrl+c clear (twice quits) · " +
		"ctrl+l model picker · ctrl+p next scoped model · ctrl+r resume · shift+tab perm mode · ctrl+o expand tools · pgup/pgdn scroll")
	return b.String()
}
