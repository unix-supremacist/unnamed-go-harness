// `ugh tree --session <file>`: print a finished (or running) agent tree from
// the session files that share the given session's run id.
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

	"github.com/unix/ugh/internal/session"
)

type treeNode struct {
	id, parent, model, cwd, path, status string
	turns, msgs                          int
	in, out                              int
	children                             []*treeNode
}

func treeCmd(args []string) error {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	sessPath := fs.String("session", "", "any session file of the run (root or child)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if *sessPath == "" && len(pos) == 1 {
		*sessPath = pos[0]
	}
	if *sessPath == "" {
		return fmt.Errorf("usage: ugh tree --session <file.jsonl>")
	}
	meta, err := firstRecord(*sessPath)
	if err != nil {
		return err
	}
	if meta.Run == "" {
		return fmt.Errorf("%s has no run id (it predates run-linked sessions)", *sessPath)
	}
	base, err := session.StateDir()
	if err != nil {
		return err
	}
	paths, _ := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
	nodes, err := runNodes(paths, meta.Run)
	if err != nil {
		return err
	}
	return printTree(os.Stdout, meta.Run, nodes)
}

// runNodes loads every session file of one run.
func runNodes(paths []string, run string) (map[string]*treeNode, error) {
	nodes := map[string]*treeNode{}
	for _, p := range paths {
		meta, err := firstRecord(p)
		if err != nil || meta.Run != run {
			continue
		}
		loaded, err := session.Load(p)
		if err != nil {
			continue
		}
		n := &treeNode{id: meta.Agent, parent: meta.Parent, model: meta.Model, cwd: meta.Cwd, path: p,
			status: "unfinished", msgs: len(loaded.Messages)}
		if n.id == "" {
			n.id = "root"
		}
		if r, ok := lastResult(p); ok {
			n.status, n.turns = r.Status, r.Turns
			if r.Model != "" {
				n.model = r.Model
			}
			if r.Usage != nil {
				n.in, n.out = r.Usage.In, r.Usage.Out
			}
		}
		if old, dup := nodes[n.id]; dup && old.status != "unfinished" {
			continue // keep the finished file when a root was resumed into a new one
		}
		nodes[n.id] = n
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no session files for run %s", run)
	}
	return nodes, nil
}

func printTree(w io.Writer, run string, nodes map[string]*treeNode) error {
	var roots []*treeNode
	for _, n := range nodes {
		if p, ok := nodes[n.parent]; ok && n.parent != "" && n.id != n.parent {
			p.children = append(p.children, n)
		} else {
			roots = append(roots, n)
		}
	}
	byID := func(ns []*treeNode) { sort.Slice(ns, func(i, j int) bool { return ns[i].id < ns[j].id }) }
	byID(roots)
	counts := map[string]int{}
	fmt.Fprintf(w, "run %s · %d agents\n", run, len(nodes))
	var walk func(n *treeNode, depth int)
	walk = func(n *treeNode, depth int) {
		counts[n.status]++
		fmt.Fprintf(w, "%s%s  %s  %s  turns %d  tok %s/%s  %s\n", strings.Repeat("  ", depth), n.id,
			orDash(n.model), n.status, n.turns, tokK(n.in), tokK(n.out), n.path)
		byID(n.children)
		for _, c := range n.children {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	var parts []string
	for _, s := range []string{"ok", "turn-out", "error", "cancelled", "unfinished"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	fmt.Fprintln(w, strings.Join(parts, " · "))
	return nil
}

func firstRecord(path string) (session.Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return session.Record{}, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return session.Record{}, fmt.Errorf("%s: empty session file", path)
	}
	var r session.Record
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return session.Record{}, fmt.Errorf("%s: bad meta record: %w", path, err)
	}
	return r, nil
}

// lastResult returns the file's last "result" record, if any.
func lastResult(path string) (session.Record, bool) {
	f, err := os.Open(path)
	if err != nil {
		return session.Record{}, false
	}
	defer f.Close()
	var last session.Record
	found := false
	rd := bufio.NewReader(f)
	for {
		line, err := rd.ReadString('\n')
		if strings.Contains(line, `"type":"result"`) {
			var r session.Record
			if json.Unmarshal([]byte(line), &r) == nil && r.Type == "result" {
				last, found = r, true
			}
		}
		if err != nil {
			return last, found
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tokK(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
