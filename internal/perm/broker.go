package perm

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Answer is one prompt response.
type Answer int

const (
	AnsAllowOnce    Answer = iota // y
	AnsAllowSession               // a — allow this exact command/path for the session
	AnsDeny                       // n — the model gets "denied by user"
	AnsStop                       // s — halt the whole run
)

// Broker is the one per-process prompt broker (PLAN.md §4.5): requests queue
// FIFO, are shown one at a time, and each agent waits on its channel.
type Broker struct {
	in          io.Reader
	out         io.Writer
	interactive bool
	headless    string // deny | stop | allow

	mu      sync.Mutex
	q       []*request
	serving bool
	session map[string]bool // session allowlist ("a" answers)
	r       *bufio.Reader   // only touched by the serving goroutine
}

type request struct {
	call Call
	vs   []Verdict
	ch   chan Answer
}

// NewBroker builds a broker. interactive=false applies headlessPrompt to
// every prompt instead of reading stdin.
func NewBroker(in io.Reader, out io.Writer, interactive bool, headlessPrompt string) *Broker {
	if headlessPrompt == "" {
		headlessPrompt = "deny"
	}
	return &Broker{
		in: in, out: out, interactive: interactive,
		headless: headlessPrompt, session: map[string]bool{},
	}
}

// SetHeadless overrides the headless prompt policy (deny|stop|allow) after
// the engine has loaded config.
func (b *Broker) SetHeadless(x string) {
	switch x {
	case "allow", "stop":
		b.headless = x
	default:
		b.headless = "deny"
	}
}

// Interactive reports whether a human can answer prompts here.
func (b *Broker) Interactive() bool {
	return b != nil && b.interactive
}

// SessionKey is the exact command/path identity an "a" answer allows for the
// rest of the session.
func SessionKey(c Call) string {
	switch c.Tool {
	case "bash":
		return c.Cwd + "\x00bash\x00" + c.Command
	case "read", "write", "edit":
		if p := stringArg(c.Args, "path"); p != "" {
			return c.Cwd + "\x00" + c.Tool + "\x00" + p
		}
	}
	return c.Cwd + "\x00" + c.Tool + "\x00" + string(c.Args)
}

// Allowed reports whether this call was already allowed for the session.
func (b *Broker) Allowed(c Call) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session[SessionKey(c)]
}

// Ask prompts for one call. Non-interactive brokers show the prompt (so the
// transcript records what would have been asked) and apply headless_prompt;
// interactive ones queue FIFO and prompt on the shared stdin.
func (b *Broker) Ask(c Call, vs []Verdict) Answer {
	if b == nil {
		return AnsDeny
	}
	if !b.interactive {
		req := &request{call: c, vs: vs}
		b.show(req)
		fmt.Fprintf(b.out, "  no human attached → headless_prompt=%s\n", b.headless)
		return b.headlessAnswer()
	}
	req := &request{call: c, vs: vs, ch: make(chan Answer, 1)}
	b.mu.Lock()
	b.q = append(b.q, req)
	start := !b.serving
	if start {
		b.serving = true
		b.r = bufio.NewReader(b.in)
	}
	b.mu.Unlock()
	if start {
		go b.serve()
	}
	return <-req.ch
}

// serve owns stdin: it shows one prompt at a time, in FIFO order.
func (b *Broker) serve() {
	for {
		b.mu.Lock()
		if len(b.q) == 0 {
			b.serving = false
			b.mu.Unlock()
			return
		}
		req := b.q[0]
		b.mu.Unlock()

		ans := b.prompt(req)

		b.mu.Lock()
		b.q = b.q[1:]
		if ans == AnsAllowSession {
			b.session[SessionKey(req.call)] = true
		}
		b.mu.Unlock()
		req.ch <- ans
	}
}

func (b *Broker) prompt(req *request) Answer {
	b.show(req)
	fmt.Fprint(b.out, "  [y] allow once  [a] allow for session  [n] deny  [s] stop run\n> ")
	for tries := 0; tries < 3; tries++ {
		line, err := b.r.ReadString('\n')
		if err != nil && line == "" {
			return b.headlessAnswer()
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return AnsAllowOnce
		case "a", "allow":
			return AnsAllowSession
		case "n", "no", "":
			return AnsDeny
		case "s", "stop":
			return AnsStop
		}
		fmt.Fprint(b.out, "please answer y, a, n or s\n> ")
	}
	return AnsDeny
}

// show prints the prompt body: agent path, cwd, the command/diff and every
// classifier verdict (scrubbed).
func (b *Broker) show(req *request) {
	c := req.call
	fmt.Fprintf(b.out, "\n⚠ permission  agent=%s  cwd=%s  tool=%s\n", orDot(c.Agent), orDot(c.Cwd), orDot(c.Tool))
	fmt.Fprintf(b.out, "  %s\n", detail(c))
	for _, v := range req.vs {
		switch {
		case v.Err != nil:
			fmt.Fprintf(b.out, "  %-16s error: %v\n", v.Classifier, v.Err)
		case v.Flagged:
			fmt.Fprintf(b.out, "  %-16s FLAG %s\n", v.Classifier, orDot(v.Reason))
		default:
			fmt.Fprintf(b.out, "  %-16s ok\n", v.Classifier)
		}
	}
}

func (b *Broker) headlessAnswer() Answer {
	switch b.headless {
	case "allow":
		return AnsAllowOnce
	case "stop":
		return AnsStop
	default:
		return AnsDeny
	}
}

// detail is the one-line command/path shown in the prompt (scrubbed).
func detail(c Call) string {
	switch c.Tool {
	case "bash":
		return c.Command
	case "read", "write", "edit":
		if p := stringArg(c.Args, "path"); p != "" {
			return c.Tool + " " + p
		}
	}
	return truncate(Scrub(string(c.Args)), 400)
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
