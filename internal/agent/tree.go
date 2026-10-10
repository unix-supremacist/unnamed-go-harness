package agent

import (
	"fmt"
	"strings"
	"sync"

	"github.com/unix/ugh/internal/llm"
)

type Tree struct {
	mu   sync.RWMutex
	root *Node
	next int
}
type Node struct {
	ID                       string
	Parent                   *Node
	Children                 []*Node
	Depth                    int
	Role, Model, Cwd, Status string
	Usage                    llm.Usage
	Messages                 []llm.Message
	// Partial and PartialThinking hold the turn being streamed right now.
	Partial, PartialThinking string
	// Writing names the tool call being generated and its argument size.
	Writing      string
	WritingChars int
	tree         *Tree
	agent        *Agent
}
type Snapshot struct {
	ID, Role, Model, Cwd, Status string
	Depth                        int
	Usage                        llm.Usage
	Messages                     []llm.Message
	Children                     []Snapshot
}

func (t *Tree) Root(id, model, cwd string) *Node {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.root == nil {
		t.root = &Node{ID: id, Role: "root", Model: model, Cwd: cwd, Status: "idle", tree: t}
	}
	return t.root
}
func (t *Tree) Child(parent *Node, role, model, cwd string) *Node {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	n := &Node{ID: fmt.Sprintf("%s/%s-%d", parent.ID, role, t.next), Parent: parent, Depth: parent.Depth + 1, Role: role, Model: model, Cwd: cwd, Status: "queued", tree: t}
	parent.Children = append(parent.Children, n)
	return n
}
func (n *Node) modelRef() string {
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	return n.Model
}
func (n *Node) setModel(s string) { n.tree.mu.Lock(); n.Model = s; n.tree.mu.Unlock() }
func (n *Node) status() string {
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	return n.Status
}
func (n *Node) setStatus(s string)   { n.tree.mu.Lock(); n.Status = s; n.tree.mu.Unlock() }
func (n *Node) setUsage(u llm.Usage) { n.tree.mu.Lock(); n.Usage = u; n.tree.mu.Unlock() }
func (n *Node) addMessage(m llm.Message) {
	n.tree.mu.Lock()
	n.Messages = append(n.Messages, m)
	if m.Role == llm.RoleAssistant {
		n.Partial, n.PartialThinking, n.Writing, n.WritingChars = "", "", "", 0
	}
	n.tree.mu.Unlock()
}
func (n *Node) appendPartial(text, thinking string) {
	n.tree.mu.Lock()
	n.Partial += text
	n.PartialThinking += thinking
	n.tree.mu.Unlock()
}
func (n *Node) setWriting(tool string, chars int) {
	n.tree.mu.Lock()
	n.Writing, n.WritingChars = tool, chars
	n.tree.mu.Unlock()
}
func (n *Node) clearPartial() {
	n.tree.mu.Lock()
	n.Partial, n.PartialThinking, n.Writing, n.WritingChars = "", "", "", 0
	n.tree.mu.Unlock()
}
func (n *Node) setAgent(a *Agent) { n.tree.mu.Lock(); n.agent = a; n.tree.mu.Unlock() }

// Info is a node's metadata without its messages (cheap to take often).
type Info struct {
	ID, Role, Model, Cwd, Status string
	Depth, Messages              int
	Usage                        llm.Usage
}

// Outline lists every node depth-first without copying messages.
func (t *Tree) Outline() []Info {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []Info
	var walk func(*Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		out = append(out, Info{ID: n.ID, Role: n.Role, Model: n.Model, Cwd: n.Cwd, Status: n.Status,
			Depth: n.Depth, Messages: len(n.Messages), Usage: n.Usage})
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(t.root)
	return out
}

// NodeView is one node's messages from an offset plus its live partials.
type NodeView struct {
	Info
	Messages                 []llm.Message // messages[from:]
	Partial, PartialThinking string
	Writing                  string
	WritingChars             int
}

// View returns node id's messages from index `from` on, plus what it is
// streaming right now; false when no such node exists.
func (t *Tree) View(id string, from int) (NodeView, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := t.find(id)
	if n == nil {
		return NodeView{}, false
	}
	v := NodeView{Info: Info{ID: n.ID, Role: n.Role, Model: n.Model, Cwd: n.Cwd, Status: n.Status, Depth: n.Depth,
		Messages: len(n.Messages), Usage: n.Usage}, Partial: n.Partial, PartialThinking: n.PartialThinking,
		Writing: n.Writing, WritingChars: n.WritingChars}
	if from < len(n.Messages) {
		v.Messages = append([]llm.Message(nil), n.Messages[from:]...)
	}
	return v, true
}

// Steer queues a user message for the agent at node id.
func (t *Tree) Steer(id, text string) bool {
	t.mu.RLock()
	n := t.find(id)
	var a *Agent
	if n != nil {
		a = n.agent
	}
	t.mu.RUnlock()
	if a == nil {
		return false
	}
	a.Steer(text)
	return true
}

func (t *Tree) find(id string) *Node {
	var walk func(*Node) *Node
	walk = func(n *Node) *Node {
		if n == nil || n.ID == id {
			return n
		}
		for _, c := range n.Children {
			if f := walk(c); f != nil {
				return f
			}
		}
		return nil
	}
	return walk(t.root)
}
func (t *Tree) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var copyNode func(*Node) Snapshot
	copyNode = func(n *Node) Snapshot {
		if n == nil {
			return Snapshot{}
		}
		s := Snapshot{ID: n.ID, Role: n.Role, Model: n.Model, Cwd: n.Cwd, Status: n.Status, Depth: n.Depth, Usage: n.Usage, Messages: append([]llm.Message(nil), n.Messages...)}
		for _, ch := range n.Children {
			s.Children = append(s.Children, copyNode(ch))
		}
		return s
	}
	return copyNode(t.root)
}

// StatusReport describes every agent in the tree in plain text: role, model,
// status, tokens and its latest step. It feeds /ask, so a busy root never
// has to be interrupted to answer "how is it going".
func (t *Tree) StatusReport() string { return t.report("") }

// SubtreeReport is StatusReport for one node's descendants.
func (t *Tree) SubtreeReport(n *Node) string { return t.report(n.ID) }

func (t *Tree) report(under string) string {
	var b strings.Builder
	var walk func(n Snapshot, depth int)
	walk = func(n Snapshot, depth int) {
		if n.ID == "" {
			return
		}
		if under != "" && !strings.HasPrefix(n.ID, under+"/") {
			for _, c := range n.Children {
				walk(c, depth)
			}
			return
		}
		fmt.Fprintf(&b, "%s%s (%s, %s) status=%s tokens=%d in/%d out messages=%d\n", strings.Repeat("  ", depth),
			n.ID, n.Role, n.Model, n.Status, n.Usage.In, n.Usage.Out, len(n.Messages))
		if len(n.Messages) > 0 {
			fmt.Fprintf(&b, "%s  latest: %s\n", strings.Repeat("  ", depth), latestStep(n.Messages))
		}
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(t.Snapshot(), 0)
	return b.String()
}

// latestStep summarizes the last message (and, for tool results, the last
// assistant text before it) in one line.
func latestStep(msgs []llm.Message) string {
	m := msgs[len(msgs)-1]
	var s string
	switch {
	case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
		c := m.ToolCalls[len(m.ToolCalls)-1]
		s = "calling " + c.Name + " " + summarize(c.Args, 200)
	case m.Role == llm.RoleTool:
		s = "got " + m.ToolName + " result: " + summarize(m.Text, 200)
	default:
		s = string(m.Role) + ": " + summarize(m.Text, 300)
	}
	for i := len(msgs) - 1; i >= 0 && m.Role != llm.RoleAssistant; i-- {
		if msgs[i].Role == llm.RoleAssistant && strings.TrimSpace(msgs[i].Text) != "" {
			s += " (last said: " + summarize(msgs[i].Text, 200) + ")"
			break
		}
	}
	return s
}

// RunningChildren lists the ids of n's direct children that have not
// finished.
func (t *Tree) RunningChildren(n *Node) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for _, c := range n.Children {
		switch c.Status {
		case "done", "error", "turn-out", "cancelled":
		default:
			out = append(out, c.ID)
		}
	}
	return out
}
