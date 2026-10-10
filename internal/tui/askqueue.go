// The ask_user queue: questions agents put to the user. Separate from the
// permission broker on purpose — requests here carry their own context (an
// agent that is cancelled stops asking), complete exactly once, wait as long
// as the user takes, and keep their half-answered state while a permission
// prompt preempts them.
package tui

import (
	"context"
	"sync"

	"github.com/unix/ugh/internal/agent"
)

// askItem is one pending ask_user call.
type askItem struct {
	id    int
	req   agent.AskRequest
	reply chan agent.AskAnswer // buffered 1; written once
	once  sync.Once

	// UI state, owned by the UI goroutine and kept while preempted.
	page   int
	cursor []int          // per question: highlighted row (len(options) = the write-in row)
	picked []map[int]bool // per question: selected option indexes
	other  []string       // per question: write-in text
	typing bool           // the write-in field has focus
	typed  string         // text being typed (committed on enter)
}

func newAskItem(id int, req agent.AskRequest) *askItem {
	n := len(req.Questions)
	it := &askItem{id: id, req: req, reply: make(chan agent.AskAnswer, 1),
		cursor: make([]int, n), picked: make([]map[int]bool, n), other: make([]string, n)}
	for i := range it.picked {
		it.picked[i] = map[int]bool{}
	}
	return it
}

// complete delivers the answer exactly once; later calls are ignored.
func (it *askItem) complete(a agent.AskAnswer) {
	it.once.Do(func() { it.reply <- a })
}

type askQueue struct {
	mu     sync.Mutex
	items  []*askItem
	nextID int
}

func (q *askQueue) add(req agent.AskRequest) *askItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextID++
	it := newAskItem(q.nextID, req)
	q.items = append(q.items, it)
	return it
}

func (q *askQueue) head() *askItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	return q.items[0]
}

func (q *askQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// remove drops it from the queue (it may be queued or displayed).
func (q *askQueue) remove(it *askItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.items {
		if x == it {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return
		}
	}
}

// declineAll answers every pending request "declined".
func (q *askQueue) declineAll() {
	q.mu.Lock()
	items := q.items
	q.items = nil
	q.mu.Unlock()
	for _, it := range items {
		it.complete(agent.AskAnswer{Declined: true})
	}
}

// askChangedMsg tells the UI the queue changed (an item arrived or left).
type askChangedMsg struct{}

// askUser is agent.ContextConfig.AskUser: called from an agent's tool
// goroutine, it blocks until the user answers, declines, the agent's ctx is
// cancelled, or the UI shuts down.
func (s *State) askUser(ctx context.Context, req agent.AskRequest) (agent.AskAnswer, error) {
	rctx := ctx
	if t := s.askTimeout; t > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	it := s.asks.add(req)
	s.notifyAsks()
	select {
	case ans := <-it.reply:
		return ans, nil
	case <-rctx.Done():
		s.asks.remove(it)
		it.complete(agent.AskAnswer{Declined: true})
		s.notifyAsks()
		if ctx.Err() != nil {
			return agent.AskAnswer{Declined: true}, ctx.Err() // the run was cancelled
		}
		return agent.AskAnswer{Declined: true}, nil // the question itself timed out
	case <-s.done:
		s.asks.remove(it)
		it.complete(agent.AskAnswer{Declined: true})
		return agent.AskAnswer{Declined: true}, nil
	}
}

func (s *State) notifyAsks() {
	if p := s.prog.Load(); p != nil {
		p.Send(askChangedMsg{})
	}
}

// askOpen reports whether an agent's question is pending.
func (s *State) askOpen() bool { return s.asks.len() > 0 }

// askActive is the item on screen: pending, and not preempted by a
// permission prompt.
func (s *State) askActive() *askItem {
	if s.modal != nil {
		return nil
	}
	return s.asks.head()
}

// declineAsks answers every pending question "declined". It runs on the UI
// goroutine (session switches, quit), so it must never Send to the program:
// Send blocks on the unbuffered message channel that this goroutine drains.
// Callers refresh the screen themselves.
func (s *State) declineAsks(string) {
	s.asks.declineAll()
}
