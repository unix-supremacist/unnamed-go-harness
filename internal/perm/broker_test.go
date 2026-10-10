package perm

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a writer the test can read while the broker goroutine writes.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var promptCall = Call{Agent: "root", Cwd: "/tmp/w", Tool: "bash", Command: "rm -rf ./build"}

func TestBrokerHeadlessPolicy(t *testing.T) {
	for headless, want := range map[string]Answer{
		"deny": AnsDeny, "stop": AnsStop, "allow": AnsAllowOnce,
	} {
		b := NewBroker(nil, &syncBuf{}, false, headless)
		if got := b.Ask(promptCall, nil); got != want {
			t.Errorf("headless_prompt=%s: Ask = %v, want %v", headless, got, want)
		}
		if b.Interactive() {
			t.Errorf("headless_prompt=%s: Interactive() = true", headless)
		}
	}
}

func TestBrokerHeadlessPromptShowsVerdictsAndReason(t *testing.T) {
	out := &syncBuf{}
	b := NewBroker(strings.NewReader(""), out, false, "deny")
	if got := b.Ask(promptCall, []Verdict{
		{Classifier: "static_extreme", Flagged: true, Reason: "rm with recursive/force flags"},
		{Classifier: "jev_or", Err: errTest},
	}); got != AnsDeny {
		t.Fatalf("Ask = %v, want deny", got)
	}
	s := out.String()
	for _, want := range []string{"permission", "rm -rf ./build", "static_extreme", "FLAG", "jev_or", "error:"} {
		if !strings.Contains(s, want) {
			t.Errorf("prompt output missing %q:\n%s", want, s)
		}
	}
}

var errTest = testErr("timeout")

type testErr string

func (e testErr) Error() string { return string(e) }

func TestBrokerStdinAnswers(t *testing.T) {
	for in, want := range map[string]Answer{
		"y\n": AnsAllowOnce,
		"a\n": AnsAllowSession,
		"n\n": AnsDeny,
		"s\n": AnsStop,
	} {
		out := &syncBuf{}
		b := NewBroker(strings.NewReader(in), out, true, "deny")
		if got := b.Ask(promptCall, nil); got != want {
			t.Errorf("input %q: Ask = %v, want %v", in, got, want)
		}
		if !b.Interactive() {
			t.Error("Interactive() = false for a stdin broker")
		}
	}
}

func TestBrokerRetriesBadAnswersThenDenies(t *testing.T) {
	out := &syncBuf{}
	b := NewBroker(strings.NewReader("what\n\n"), out, true, "deny")
	if got := b.Ask(promptCall, nil); got != AnsDeny {
		t.Fatalf("Ask = %v, want deny on empty line", got)
	}
	if !strings.Contains(out.String(), "please answer y, a, n or s") {
		t.Errorf("no retry message:\n%s", out.String())
	}
}

func TestBrokerEOF_fallsBackToHeadlessPolicy(t *testing.T) {
	out := &syncBuf{}
	b := NewBroker(strings.NewReader(""), out, true, "stop")
	if got := b.Ask(promptCall, nil); got != AnsStop {
		t.Fatalf("Ask on EOF = %v, want stop", got)
	}
}

func TestBrokerSessionAllow(t *testing.T) {
	b := NewBroker(strings.NewReader("a\n"), &syncBuf{}, true, "deny")
	if got := b.Ask(promptCall, nil); got != AnsAllowSession {
		t.Fatalf("Ask = %v, want session allow", got)
	}
	if !b.Allowed(promptCall) {
		t.Error("same command not allowed after \"a\"")
	}
	// "a" allows the exact command string for the session (§4.2).
	other := promptCall
	other.Command = "rm -rf /"
	if b.Allowed(other) {
		t.Error("different command must not inherit the session allow")
	}
	// A write tool answer keys on path.
	w := Call{Tool: "write", Args: []byte(`{"path":"sub/a.txt"}`)}
	b2 := NewBroker(strings.NewReader("a\n"), &syncBuf{}, true, "deny")
	if b2.Ask(w, nil) != AnsAllowSession {
		t.Fatal("write prompt not allowed for session")
	}
	if !b2.Allowed(Call{Tool: "write", Args: []byte(`{"path":"sub/a.txt"}`)}) {
		t.Error("same path not allowed after \"a\"")
	}
	if b2.Allowed(Call{Tool: "write", Args: []byte(`{"path":"sub/b.txt"}`)}) {
		t.Error("different path inherited the session allow")
	}
}

// Two queued prompts are served one at a time, FIFO, on the same stdin.
// io.Pipe keeps the serving goroutine blocked on read, so the queue state is
// observable before answers are handed over.
func TestBrokerServesFIFO(t *testing.T) {
	out := &syncBuf{}
	pr, pw := io.Pipe()
	b := NewBroker(pr, out, true, "deny")
	first, second := make(chan Answer, 1), make(chan Answer, 1)
	go func() { first <- b.Ask(promptCall, nil) }()
	waitFor(t, func() bool { return b.queued() == 1 })
	go func() { second <- b.Ask(Call{Tool: "bash", Command: "git push --force"}, nil) }()
	waitFor(t, func() bool { return b.queued() == 2 })
	if _, err := io.WriteString(pw, "y\n"); err != nil {
		t.Fatal(err)
	}
	if got := <-first; got != AnsAllowOnce {
		t.Errorf("first prompt = %v, want allow", got)
	}
	if _, err := io.WriteString(pw, "n\n"); err != nil {
		t.Fatal(err)
	}
	if got := <-second; got != AnsDeny {
		t.Errorf("second prompt = %v, want deny", got)
	}
	if n := strings.Count(out.String(), "permission"); n != 2 {
		t.Errorf("saw %d prompts, want 2:\n%s", n, out.String())
	}
}

// queued reports how many prompts are waiting (test hook).
func (b *Broker) queued() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.q)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}
