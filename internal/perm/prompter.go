package perm

import (
	"sync"
)

// Prompter is the prompt surface the engine consults: the FIFO stdin
// broker (headless/CLI) or a UI channel broker (TUI). Both implement the
// same session-allowlist semantics via SessionKey.
type Prompter interface {
	// Ask asks the human to judge one call; blocks until answered.
	Ask(c Call, vs []Verdict) Answer
	// Allowed reports whether the call was already allowed for the session.
	Allowed(c Call) bool
	// Interactive reports whether a human can answer prompts.
	Interactive() bool
}

// UIRequest is one prompt delivered to a TUI. The UI answers by sending the
// verdict on Reply (buffered) and calls Remember for an "a" answer.
type UIRequest struct {
	Call     Call
	Verdicts []Verdict
	Reply    chan Answer
}

// UIBroker delivers prompts over a channel instead of reading stdin, so the
// TUI renders them as a modal. Ask blocks until the UI answers or done
// closes (program exit), then the run stops.
type UIBroker struct {
	Requests chan UIRequest
	done     <-chan struct{}

	mu      sync.Mutex
	session map[string]bool
}

// NewUIBroker builds a channel broker. done is closed when the program
// exits so a blocked Ask never leaks its goroutine.
func NewUIBroker(done <-chan struct{}) *UIBroker {
	return &UIBroker{Requests: make(chan UIRequest), done: done, session: map[string]bool{}}
}

// Ask delivers the prompt to the UI and waits for its answer.
func (b *UIBroker) Ask(c Call, vs []Verdict) Answer {
	req := UIRequest{Call: c, Verdicts: vs, Reply: make(chan Answer, 1)}
	select {
	case b.Requests <- req:
	case <-b.done:
		return AnsStop
	}
	select {
	case a := <-req.Reply:
		return a
	case <-b.done:
		return AnsStop
	}
}

// Remember records a session-allowlist entry for an "a" answer.
func (b *UIBroker) Remember(c Call) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.session[SessionKey(c)] = true
}

// Allowed reports whether this call was allowed for the session.
func (b *UIBroker) Allowed(c Call) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session[SessionKey(c)]
}

// Interactive: a human answers prompts through the UI.
func (b *UIBroker) Interactive() bool { return true }
