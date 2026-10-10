// `ugh eval council` (PLAN.md §6.5): runs a small question set from
// evals/council/*.md, each with a reference answer, through (1) the full
// council, (2) a single mimo-v2.6-pro scout, and (3) an oracle model card
// (gpt-sol). Writes a side-by-side markdown report per question for grading.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/config"
)

func evalCmd(args []string) error {
	if len(args) == 0 || args[0] != "council" {
		return fmt.Errorf("eval: expected council")
	}
	fs := flag.NewFlagSet("eval council", flag.ContinueOnError)
	one := fs.String("q", "", "run only this question file (evals/council/<q>.md)")
	outDir := fs.String("out", "evals/council/reports", "report output directory")
	research := fs.Bool("research", true, "council pre-research evidence bundle")
	timeout := fs.Int("timeout", 1500, "per-question wall clock budget in seconds")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files, err := evalQuestions(*one)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rt := agent.NewRuntime() // per-provider stream semaphore, as in the TUI
	for _, f := range files {
		q, err := readEvalQuestion(f)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "== %s ==\n", q.title)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
		report, err := runEvalQuestion(ctx, cfg, q, *research, rt.Limit)
		cancel()
		if err != nil {
			return fmt.Errorf("eval %s: %w", q.title, err)
		}
		path, err := writeEvalReport(*outDir, q, report, time.Since(start))
		if err != nil {
			return err
		}
		fmt.Printf("%s → %s\n", q.title, path)
	}
	return nil
}

// evalQuestion is one parsed evals/council/*.md case.
type evalQuestion struct {
	file, title, question, reference string
	angles                           json.RawMessage // optional pinned lens ids
}

var evalKey = regexp.MustCompile(`^(question|reference|angles):\s*(.*)$`)

// evalScope is appended to every eval task: the models must answer from
// primary evidence and never read the eval artifacts that hold the reference
// answers or prior grading reports.
const evalScope = "\n\nEvidence scope: base the answer on primary evidence (code, configs and docs you inspect). Do NOT read or cite anything under evals/ (question files, reference answers, grading reports) or any prior run report, and skip the old/ retired tree; those are evaluation artifacts, not evidence."

// readEvalQuestion parses the simple format: `# Title`, then `question:`,
// `reference:` and optional `angles:` keys whose values run to the next key.
// `angles:` names comma-separated lens ids that pin the council's angles.
func readEvalQuestion(path string) (evalQuestion, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return evalQuestion{}, err
	}
	q := evalQuestion{file: path}
	key := ""
	var lensIDs []string
	var sections = map[string]*string{"question": &q.question, "reference": &q.reference}
	var lensLine string
	sections["angles"] = &lensLine
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "# ") && q.title == "" {
			q.title = strings.TrimSpace(line[2:])
			continue
		}
		if m := evalKey.FindStringSubmatch(line); m != nil {
			key = m[1]
			*sections[key] = m[2]
			continue
		}
		if key != "" && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			*sections[key] += "\n" + line
		}
	}
	for _, id := range strings.FieldsFunc(lensLine, func(r rune) bool { return r == ',' || r == ' ' }) {
		if _, ok := agent.Lenses[id]; !ok {
			return q, fmt.Errorf("evals/council: %s: unknown lens %q", path, id)
		}
		lensIDs = append(lensIDs, id)
	}
	if len(lensIDs) > 0 {
		q.angles, _ = json.Marshal(lensIDs)
	}
	if q.title == "" {
		q.title = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if strings.TrimSpace(q.question) == "" || strings.TrimSpace(q.reference) == "" {
		return q, fmt.Errorf("evals/council: %s needs question: and reference:", path)
	}
	return q, nil
}

func evalQuestions(only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}
	matches, err := filepath.Glob("evals/council/*.md")
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("evals/council: no question files found")
	}
	sort.Strings(matches)
	return matches, nil
}

// evalReport is everything one comparison run produced.
type evalReport struct {
	council   *agent.Result
	single    string
	singleUse struct{ in, out int }
	oracle    string
	oracleUse struct{ in, out int }
	singleErr error
	oracleErr error
}

func runEvalQuestion(ctx context.Context, cfg config.Config, q evalQuestion, research bool, limit func(string) chan struct{}) (*evalReport, error) {
	cc := cfg.Council
	r := &evalReport{}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	res, err := agent.CouncilRun(ctx, cwd, q.question+evalScope, q.angles, research, nil, limit)
	if err != nil {
		if res == nil || res.Report == "" {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "eval: council partial: %v\n", err)
	}
	r.council = res
	single := cc.Single
	if single == "" {
		single = "mimo-v2.6-pro"
	}
	text, usage, err := agent.ScoutRun(ctx, cwd, single, "Answer this repository design question with evidence. Be concise and concrete."+evalScope+"\n\n"+q.question)
	if err != nil {
		r.singleErr = err
	} else {
		r.single, r.singleUse.in, r.singleUse.out = text, usage.In, usage.Out
	}
	oracle := cc.Oracle
	if oracle == "" {
		oracle = "gpt-sol"
	}
	text, usage, err = agent.ScoutRun(ctx, cwd, oracle, "Answer this repository design question with evidence at oracle quality. Be concise and concrete."+evalScope+"\n\n"+q.question)
	if err != nil {
		r.oracleErr = err
	} else {
		r.oracle, r.oracleUse.in, r.oracleUse.out = text, usage.In, usage.Out
	}
	return r, nil
}

func words(s string) int { return len(strings.Fields(s)) }

func writeEvalReport(outDir string, q evalQuestion, r *evalReport, elapsed time.Duration) (string, error) {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", err
	}
	slug := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(q.title), "-")
	path := filepath.Join(outDir, slug+".md")
	var b strings.Builder
	fmt.Fprintf(&b, "# Council eval: %s\n\n", q.title)
	fmt.Fprintf(&b, "- date: %s\n- elapsed: %s\n- question file: %s\n\n", time.Now().UTC().Format(time.RFC3339), elapsed.Round(time.Second), q.file)
	fmt.Fprintf(&b, "## Question\n\n%s\n\n", q.question)
	fmt.Fprintf(&b, "## Reference answer\n\n%s\n\n", q.reference)
	fmt.Fprintf(&b, "## Council\n\n")
	if r.council != nil {
		done := 0
		var seatLines []string
		for _, s := range r.council.Seats {
			if s.Err == nil {
				done++
				seatLines = append(seatLines, fmt.Sprintf("| %s | %s | %s | ok |", s.Label, s.Model, s.Lens))
			} else {
				seatLines = append(seatLines, fmt.Sprintf("| — | %s | %s | failed: %v |", s.Model, s.Lens, s.Err))
			}
		}
		fmt.Fprintf(&b, "Panel: %d/%d seats answered; tokens %d in / %d out.\n\n", done, len(r.council.Seats), r.council.Usage.In, r.council.Usage.Out)
		if len(r.council.Failed) > 0 {
			fmt.Fprintf(&b, "Failed or timed-out seats: %s\n\n", strings.Join(r.council.Failed, "; "))
		}
		b.WriteString("| seat | model | lens | status |\n|---|---|---|---|\n")
		b.WriteString(strings.Join(seatLines, "\n") + "\n\n")
		b.WriteString(r.council.Report + "\n\n")
	} else {
		b.WriteString("(council failed)\n\n")
	}
	fmt.Fprintf(&b, "## Single (%s)\n\n", "mimo-v2.6-pro")
	if r.singleErr != nil {
		fmt.Fprintf(&b, "failed: %v\n\n", r.singleErr)
	} else {
		fmt.Fprintf(&b, "tokens %d in / %d out; %d words.\n\n%s\n\n", r.singleUse.in, r.singleUse.out, words(r.single), r.single)
	}
	fmt.Fprintf(&b, "## Oracle (gpt-sol)\n\n")
	if r.oracleErr != nil {
		fmt.Fprintf(&b, "failed: %v\n", r.oracleErr)
	} else {
		fmt.Fprintf(&b, "tokens %d in / %d out; %d words.\n\n%s\n", r.oracleUse.in, r.oracleUse.out, words(r.oracle), r.oracle)
	}
	return path, os.WriteFile(path, []byte(b.String()), 0644)
}
