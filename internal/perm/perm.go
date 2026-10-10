// Package perm is ugh's permission system (PLAN.md §4): classifiers judge
// tool calls, the pure function Decide turns verdicts into a decision, a FIFO
// broker runs the y/a/n/s prompt (with headless policy), and a JSONL decision
// log records everything with secrets scrubbed.
//
// This package and internal/perm/static deliberately never import os/exec or
// syscall: the permission layer cannot run commands itself.
package perm

import (
	"context"
	"encoding/json"
	"errors"
)

// Call is one tool invocation judged by the permission system.
type Call struct {
	ID      string          // provider tool-call id (matches the UI's tool entry)
	Agent   string          // tree path, e.g. "root/2/1"
	Cwd     string          // the agent's working directory
	Tool    string          // tool name (bash, write, edit, ...)
	Args    json.RawMessage // raw tool arguments
	Command string          // bash only: the exact command string
	Task    string          // the agent's task, for classifier context
	Recent  []Message       // last few turns, for model-classifier context
	// Tier is the call's triage tier (trivial | low | middle | high), set by
	// the engine; TierReason says why.
	Tier, TierReason string
}

// Message is one recent conversation turn handed to model classifiers as
// context (§4.3 state.recent_messages). The caller withholds tool output.
type Message struct{ Role, Text string }

// NewCall builds a tool call for the gate, lifting bash's command string so
// the static classifier judges real argv (§4.4).
func NewCall(cwd, tool string, args json.RawMessage) Call {
	c := Call{Cwd: cwd, Tool: tool, Args: args}
	if tool == "bash" {
		c.Command = stringArg(args, "command")
	}
	return c
}

// Verdict is one classifier's opinion about one call.
type Verdict struct {
	Classifier string  `json:"name"`
	Flagged    bool    `json:"flagged"`
	Score      float64 `json:"score,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Cost       float64 `json:"cost,omitempty"` // USD billed for this verdict (model classifiers)
	Err        error   `json:"-"`
}

// Classifier judges a single call. Implementations must be safe for
// concurrent use: model classifiers run in parallel.
type Classifier interface {
	Name() string
	Classify(ctx context.Context, c Call) Verdict
}

// Policy actions.
const (
	Allow  = "allow"
	Prompt = "prompt"
	Deny   = "deny"
	Stop   = "stop"
)

// ErrStopped marks a whole agent run halted by the permission system. The
// model gets no further turn; control returns to the user.
var ErrStopped = errors.New("stopped by permission policy")

// Outcome is the final per-call result applied by the agent loop:
// allow (run the tool), deny (skip it, the model reads a denial) or stop
// (halt the entire run).
type Outcome struct {
	Action string // allow | deny | stop
	Reason string
}

// Gate is what the agent loop consults before running a tool.
type Gate interface {
	Check(ctx context.Context, c Call) Outcome
}

// OutputGate is implemented by gates that screen a tool result for
// instructions addressed to an AI agent (PLAN.md §4.3 untrusted-output
// check). The agent loop calls it after every tool that actually ran; a
// flag raises that agent's next permission prompt to at least prompt.
type OutputGate interface {
	CheckOutput(ctx context.Context, agent, tool, output string)
}
