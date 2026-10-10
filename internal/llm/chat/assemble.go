// CallAccumulator assembles streaming tool-call fragments (Chat Completions
// deltas) into complete calls: the first frame for a call index carries
// id+name, argument deltas accumulate across frames.
package chat

import (
	"sort"

	"github.com/unix/ugh/internal/llm"
)

// CallAccumulator collects streamed tool-call fragments.
type CallAccumulator struct {
	frames map[int]*llm.ToolCall
}

func NewCallAccumulator() *CallAccumulator { return &CallAccumulator{frames: map[int]*llm.ToolCall{}} }

// Add ingests one streamed fragment at the given stream index.
func (a *CallAccumulator) Add(index int, f llm.ToolCall) {
	c, ok := a.frames[index]
	if !ok {
		a.frames[index] = &llm.ToolCall{ID: f.ID, Name: f.Name, Args: f.Args}
		return
	}
	if f.ID != "" {
		c.ID = f.ID
	}
	if f.Name != "" {
		c.Name = f.Name
	}
	c.Args += f.Args
}

// Finish returns the assembled calls in stream order.
func (a *CallAccumulator) Finish() []llm.ToolCall {
	idxs := make([]int, 0, len(a.frames))
	for i := range a.frames {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]llm.ToolCall, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, *a.frames[i])
	}
	return out
}
