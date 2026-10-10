package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/unix/ugh/internal/session"
)

// sendCmd queues a message for a running agent (or a whole subtree) of a
// run; the agent receives it as a user message before its next turn, or
// within seconds when it is waiting on its children.
func sendCmd(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	sess := fs.String("session", "", "any session file of the run (default: the most recently active run)")
	subtree := fs.Bool("subtree", false, "also send to every running descendant of the agent")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return fmt.Errorf("usage: ugh send [--subtree] [--session FILE] <agent-id> <message...>")
	}
	target, text := pos[0], strings.Join(pos[1:], " ")
	run, err := pickRun(*sess)
	if err != nil {
		return err
	}
	if run == "" {
		return fmt.Errorf("that run predates run ids; messages need a run started by a newer ugh")
	}
	running, finished := map[string]bool{}, map[string]bool{}
	for _, p := range runFiles(run, time.Now().Add(-48*time.Hour)) {
		m, err := firstRecord(p)
		if err != nil {
			continue
		}
		id := m.Agent
		if id == "" {
			id = "root"
		}
		if _, done := lastResult(p); done {
			finished[id] = true
		} else {
			running[id] = true
		}
	}
	var targets []string
	for id := range running {
		if id == target || (*subtree && strings.HasPrefix(id, target+"/")) || (*subtree && target == "root") {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		if finished[target] {
			return fmt.Errorf("%s has already finished", target)
		}
		return fmt.Errorf("no running agent %s in run %s (ugh watch lists them)", target, run)
	}
	for _, id := range targets {
		if err := session.SendInbox(run, id, text); err != nil {
			return err
		}
	}
	fmt.Printf("queued for %d agent(s): %s\ndelivered before each agent's next turn (or within ~2s while it waits on children)\n",
		len(targets), strings.Join(targets, ", "))
	return nil
}
