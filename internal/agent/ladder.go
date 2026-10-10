package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/perm/model"
	"github.com/unix/ugh/internal/session"
)

// The visibility ladder (the Jev paper's central idea): each old tool result
// is hidden, shown short, or shown in full, decided per query once the query
// is known. Nothing is lost — the session archive keeps every result, and the
// recall tool pages any of them back in.
//
//   - Every new query (prompt, steer, inbox message) starts all-full; decisions
//     only lower individual chunks. A skip, error, timeout or malformed answer
//     leaves a chunk full.
//   - The view is frozen for the tool-loop turns of one query, so the provider's
//     prefix cache holds. It is applied only when the request is assembled;
//     history itself is never rewritten, and every tool message keeps its slot.
//   - recall returns text as a *new* tool result; old placeholders never change.

type viewLevel int

const (
	levelFull viewLevel = iota
	levelShort
	levelHide
	levelDigest
)

func (l viewLevel) String() string {
	return [...]string{"full", "short", "hide", "digest"}[l]
}

const (
	ladderMaxFails     = 3 // consecutive all-failed passes before the ladder gives up
	ladderMaxQuestions = 8 // per request
	ladderMaxStateB    = 16 << 10
	ladderParallel     = 3
	ladderBudget       = 10 * time.Second
	ladderQueryMax     = 2 << 10
	ladderArgsMax      = 200
	ladderHeadMax      = 400

	shortLines   = 15
	shortLineCap = 200
	shortTotal   = 4 << 10
)

// ladderStats describes the latest ladder pass, for the trace and /context.
type ladderStats struct {
	Chunks, Hide, Short, Digest, Full int
	SavedBytes                        int
	Cost                              model.Cost
	Err                               string
	At                                time.Time
}

type chunk struct {
	ord  int
	text string
	e    archived
}

// eligibleChunks are tool results big enough to matter and old enough to touch.
func (a *Agent) eligibleChunks() []chunk {
	cc := a.cfg.Context
	end := len(a.hist) - cc.KeepRecent
	var out []chunk
	for i := 0; i < end && i < len(a.hist); i++ {
		m := a.hist[i]
		if m.Role != llm.RoleTool || m.Ord <= 0 || len(m.Text) < cc.MinChunkBytes {
			continue
		}
		e, ok := a.arch.get(m.Ord)
		if !ok {
			continue
		}
		out = append(out, chunk{ord: m.Ord, text: m.Text, e: e})
	}
	return out
}

// callLabel names a result for placeholders: the tool and a squeezed argument.
func callLabel(e archived) string {
	args := strings.Join(strings.Fields(e.Args), " ")
	var m map[string]any
	if json.Unmarshal([]byte(e.Args), &m) == nil {
		for _, k := range []string{"command", "pattern", "path", "query"} {
			if v, ok := m[k].(string); ok && v != "" {
				args = v
				if k == "pattern" {
					args = fmt.Sprintf("%q", v)
				}
				break
			}
		}
		args = strings.Join(strings.Fields(args), " ")
	}
	if len(args) > 80 {
		args = args[:80] + "…"
	}
	return strings.TrimSpace(e.Tool + " " + args)
}

func kib(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.0f KiB", float64(n)/1024)
}

func renderHidden(e archived, size int) string {
	return fmt.Sprintf("[%s: %s · %s · hidden as not relevant to this request · recall(%q)]", e.alias(), callLabel(e), kib(size), e.alias())
}

// renderShort is the deterministic short view: the first and last shortLines
// lines (everything when there are 2×shortLines or fewer), each line capped,
// the whole bounded by shortTotal bytes, plus the recall hint.
func renderShort(e archived, text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	capLine := func(s string) string {
		if len(s) > shortLineCap {
			return strings.ToValidUTF8(s[:shortLineCap], "") + "…"
		}
		return s
	}
	var head, tail []string
	omitted := 0
	if len(lines) <= 2*shortLines {
		for _, l := range lines {
			head = append(head, capLine(l))
		}
	} else {
		for _, l := range lines[:shortLines] {
			head = append(head, capLine(l))
		}
		for _, l := range lines[len(lines)-shortLines:] {
			tail = append(tail, capLine(l))
		}
		omitted = len(lines) - 2*shortLines
	}
	// Bound by bytes: trim the inner ends of the head and tail first.
	size := func() int {
		n := 0
		for _, l := range head {
			n += len(l) + 1
		}
		for _, l := range tail {
			n += len(l) + 1
		}
		return n
	}
	for size() > shortTotal && (len(head) > 1 || len(tail) > 1) {
		if len(head) >= len(tail) && len(head) > 1 {
			head = head[:len(head)-1]
		} else if len(tail) > 1 {
			tail = tail[1:]
		}
		omitted++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s: %s · %s · short view (%d of %d lines) · recall(%q, start_line, grep)]\n", e.alias(), callLabel(e), kib(len(text)), len(head)+len(tail), len(lines), e.alias())
	b.WriteString(strings.Join(head, "\n"))
	if omitted > 0 {
		fmt.Fprintf(&b, "\n… %d lines omitted …\n", omitted)
	} else if len(tail) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(tail, "\n"))
	return strings.TrimRight(b.String(), "\n")
}

// viewText is the text the model sees for a tool message under the current view.
func (a *Agent) viewText(m llm.Message) string {
	lv, ok := a.view[m.Ord]
	if !ok || lv == levelFull {
		return m.Text
	}
	e, ok := a.arch.get(m.Ord)
	if !ok {
		return m.Text
	}
	switch lv {
	case levelHide:
		return renderHidden(e, len(m.Text))
	case levelShort:
		return renderShort(e, m.Text)
	case levelDigest:
		if d, ok := a.digests[m.Ord]; ok {
			return d
		}
	}
	return m.Text
}

// assemble is the history exactly as the model will see it on this route:
// the visibility view applied, then adapted to the destination provider.
func (a *Agent) assemble(msgs []llm.Message) []llm.Message {
	if len(a.view) > 0 {
		out := make([]llm.Message, len(msgs))
		copy(out, msgs)
		for i := range out {
			if out[i].Role == llm.RoleTool && out[i].Ord > 0 {
				out[i].Text = a.viewText(out[i])
			}
		}
		msgs = out
	}
	return adaptHistory(msgs, a.cfg.Node.Model)
}

// jevClient resolves the decisions client once per agent. A missing client
// disables the ladder for this agent (fail-open: everything stays full).
func (a *Agent) jevClient() model.CostAsker {
	a.jevOnce.Do(func() {
		if a.cfg.Context.Jev != nil {
			a.jev = a.cfg.Context.Jev
			return
		}
		c, err := DefaultDecisionClient()
		if err != nil {
			fmt.Fprintf(a.cfg.Trace, "🔎 %s: ladder disabled: %v\n", a.cfg.Agent, err)
			return
		}
		if ca, ok := c.(model.CostAsker); ok {
			a.jev = ca
		}
	})
	return a.jev
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

type chunkState struct {
	Alias string `json:"alias"`
	Tool  string `json:"tool"`
	Args  string `json:"args"`
	Bytes int    `json:"bytes"`
	Lines int    `json:"lines"`
	Head  string `json:"head"`
}

// stateFor builds one chunk's Jev state; every string is scrubbed.
func stateFor(c chunk) chunkState {
	return chunkState{
		Alias: c.e.alias(), Tool: c.e.Tool, Args: perm.Scrub(clipBytes(c.e.Args, ladderArgsMax)),
		Bytes: len(c.text), Lines: strings.Count(c.text, "\n") + 1,
		Head: perm.Scrub(clipBytes(strings.TrimSpace(c.text), ladderHeadMax)),
	}
}

func (a *Agent) ladderQuestion(c chunk, withDigest bool) model.Question {
	crit := map[string]string{
		"hide":  "Not relevant to the current request; a one-line placeholder is enough",
		"short": "Only its first and last lines matter",
		"full":  "The whole output is needed to continue",
	}
	if withDigest {
		crit["digest"] = "A faithful summary of it is enough"
	}
	return model.Choice(fmt.Sprintf("The assistant is working on the request in state.query. Tool result %s (%s, %s) is one of the earlier tool results in its context. "+
		"How much of it must stay in view for the assistant to do the work asked for? Default to full when unsure.",
		c.e.alias(), c.e.Tool, kib(len(c.text))), crit)
}

// digestable reports whether a chunk can be summarized instead of shown.
func (a *Agent) digestable(e archived) bool {
	cc := a.cfg.Context
	if cc.DigestModel == "" {
		return false
	}
	for _, t := range cc.DigestTools {
		if t == e.Tool {
			return true
		}
	}
	return false
}

// decideView runs at a query boundary. It never fails the run: whatever goes
// wrong, the view stays (or becomes) all-full.
func (a *Agent) decideView(ctx context.Context) {
	a.viewDirty = false
	a.view = nil
	cc := a.cfg.Context
	if !cc.ladderOn() || a.cfg.OneShot || a.ladderFails >= ladderMaxFails {
		return
	}
	chunks := a.eligibleChunks()
	st := ladderStats{Chunks: len(chunks), At: time.Now()}
	defer func() { a.lastLadder = st }()
	if len(chunks) == 0 {
		return
	}
	potential := 0
	for _, c := range chunks {
		potential += len(c.text) - len(renderHidden(c.e, len(c.text)))
	}
	if potential < cc.MinSavings {
		st.Full = len(chunks)
		return
	}
	jev := a.jevClient()
	if jev == nil {
		st.Full, st.Err = len(chunks), "no decisions client"
		return
	}
	sort.SliceStable(chunks, func(i, j int) bool { return len(chunks[i].text) > len(chunks[j].text) })
	if max := ladderMaxQuestions * ladderParallel; len(chunks) > max {
		chunks = chunks[:max] // the rest stay full: bounded cost beats completeness
	}
	query := perm.Scrub(clipBytes(a.query, ladderQueryMax))

	type batchResult struct {
		chunks []chunk
		res    map[string]model.Result
		cost   model.Cost
		err    error
	}
	var batches [][]chunk
	for i := 0; i < len(chunks); i += ladderMaxQuestions {
		batches = append(batches, chunks[i:min(i+ladderMaxQuestions, len(chunks))])
	}
	pctx, cancel := context.WithTimeout(ctx, ladderBudget)
	defer cancel()
	results := make([]batchResult, len(batches))
	var wg sync.WaitGroup
	for bi, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states := make([]chunkState, len(batch))
			qs := map[string]model.Question{}
			for i, c := range batch {
				states[i] = stateFor(c)
				qs[c.e.alias()] = a.ladderQuestion(c, a.digestable(c.e))
			}
			state := map[string]any{"query": query, "chunks": states}
			if b, _ := json.Marshal(state); len(b) > ladderMaxStateB {
				results[bi] = batchResult{chunks: batch, err: fmt.Errorf("state too large (%d bytes)", len(b))}
				return
			}
			res, cost, err := jev.AskCosted(pctx, state, qs)
			results[bi] = batchResult{chunks: batch, res: res, cost: cost, err: err}
		}()
	}
	wg.Wait()

	view := map[int]viewLevel{}
	cost := model.Cost{Known: true}
	var firstErr error
	failed := 0
	for _, br := range results {
		cost = cost.Add(br.cost)
		if br.err != nil {
			failed++
			if firstErr == nil {
				firstErr = br.err
			}
			continue
		}
		for _, c := range br.chunks {
			r, ok := br.res[c.e.alias()]
			choice, _ := r.Answer.(string)
			if !ok {
				continue
			}
			switch choice {
			case "hide":
				view[c.ord] = levelHide
			case "short":
				view[c.ord] = levelShort
			case "digest":
				if a.digestable(c.e) {
					view[c.ord] = levelDigest
				}
			}
		}
	}
	// Digests are computed now, not at assembly: assembly must be deterministic.
	var digestTodo []chunk
	for _, c := range chunks {
		if view[c.ord] == levelDigest {
			if _, done := a.digests[c.ord]; !done {
				digestTodo = append(digestTodo, c)
			}
		}
	}
	if len(digestTodo) > 0 {
		var dwg sync.WaitGroup
		var mu sync.Mutex
		sem := make(chan struct{}, ladderParallel)
		for _, c := range digestTodo {
			dwg.Add(1)
			sem <- struct{}{}
			go func() {
				defer dwg.Done()
				defer func() { <-sem }()
				text, u, ok := a.digestChunk(ctx, c.e, c.text)
				mu.Lock()
				defer mu.Unlock()
				cost.USD += u.Cost
				if u.Cost == 0 && u.In == 0 && u.Out == 0 {
					cost.Known = false
				}
				if ok {
					if a.digests == nil {
						a.digests = map[int]string{}
					}
					a.digests[c.ord] = text
				}
			}()
		}
		dwg.Wait()
		for _, c := range digestTodo {
			if _, ok := a.digests[c.ord]; !ok {
				delete(view, c.ord) // a failed digest is full, not a gap
			}
		}
	}

	// A decisions endpoint that keeps failing outright (no key, no network) is
	// given up on for this agent rather than retried every query.
	if failed == len(results) {
		a.ladderFails++
		if a.ladderFails >= ladderMaxFails {
			fmt.Fprintf(a.cfg.Trace, "🔎 %s: ladder turned off after %d failed passes: %v\n", a.cfg.Agent, a.ladderFails, firstErr)
			a.emit(Event{Kind: "note", Text: fmt.Sprintf("🔎 ladder turned off for this agent after %d failed passes: %v", a.ladderFails, firstErr)})
		}
	} else {
		a.ladderFails = 0
	}
	a.view = view
	for _, c := range a.eligibleChunks() {
		lv := view[c.ord]
		a.noteChunk(&st, c, lv)
	}
	st.Cost = cost
	if firstErr != nil {
		st.Err = firstErr.Error()
	}
	// The ladder's own spend is part of the run's spend, counted once here.
	a.Usage.Cost += cost.USD
	a.cfg.Node.setUsage(a.Usage)
	a.emit(Event{Kind: "usage", Usage: a.Usage})
	costLabel := "$?"
	if cost.Known {
		costLabel = fmt.Sprintf("$%.4f", cost.USD)
	}
	line := fmt.Sprintf("🔎 ladder: %d chunks → %d hide · %d short · %d digest · %d full · ~%dk tok saved · %s",
		st.Chunks, st.Hide, st.Short, st.Digest, st.Full, st.SavedBytes*10/35/1000, costLabel)
	fmt.Fprintln(a.cfg.Trace, line)
	a.emit(Event{Kind: "note", Text: line})
	if firstErr != nil {
		fmt.Fprintf(a.cfg.Trace, "🔎 %s: ladder: some chunks stay full: %v\n", a.cfg.Agent, firstErr)
	}
	if a.cfg.OnRecord != nil {
		a.cfg.OnRecord(session.Record{Type: "ladder", Agent: a.cfg.Agent, Meta: map[string]any{
			"chunks": st.Chunks, "hide": st.Hide, "short": st.Short, "digest": st.Digest, "full": st.Full,
			"saved_bytes": st.SavedBytes, "cost_usd": cost.USD, "cost_known": cost.Known, "error": st.Err,
		}})
	}
}

// noteChunk tallies one chunk's level into st.
func (a *Agent) noteChunk(st *ladderStats, c chunk, lv viewLevel) {
	switch lv {
	case levelHide:
		st.Hide++
		st.SavedBytes += len(c.text) - len(renderHidden(c.e, len(c.text)))
	case levelShort:
		st.Short++
		st.SavedBytes += len(c.text) - len(renderShort(c.e, c.text))
	case levelDigest:
		st.Digest++
		st.SavedBytes += len(c.text) - len(a.digests[c.ord])
	default:
		st.Full++
	}
}

// ---- recall ----

const recallCap = 24 << 10

type recallTool struct{ agent *Agent }

func (recallTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "recall",
		Description: "Re-read an earlier tool result by its id (the c<N> shown in a hidden or short placeholder, or in a trimmed result's hint). " +
			"Returns up to 24 KiB per call: page with start_line, or filter with grep (case-insensitive regex, matching lines with numbers). " +
			"Works for results that were hidden, shortened, trimmed, or compacted away.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string", "description": `result id such as "c42"`},
				"start_line": map[string]any{"type": "integer", "description": "first line to return (1-based, default 1)"},
				"grep":       map[string]any{"type": "string", "description": "return only lines matching this pattern"},
			},
			"required": []string{"id"},
		},
	}
}

func (t recallTool) Run(_ context.Context, _ string, raw json.RawMessage) (string, error) {
	var args struct {
		ID        string `json:"id"`
		StartLine int    `json:"start_line"`
		Grep      string `json:"grep"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("recall: bad arguments: %v", err)
	}
	return t.agent.recall(args.ID, args.StartLine, args.Grep)
}
