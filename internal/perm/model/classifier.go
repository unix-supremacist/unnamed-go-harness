// Classifier: the shared decisions client plus the permission questions and
// pisup's gate thresholds. All four model instances (Jev via OpenRouter and
// OpenCode Go, Clef, Respan) are this type with different config.
package model

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/unix/ugh/internal/perm"
)

// Classifier judges one tool call with the decisions API.
type Classifier struct {
	name string
	cfg  perm.ClassifierConfig
	cl   *Client
}

// New builds a classifier from its config entry. A missing slug or endpoint
// is not a construction failure: the call reports it as an Err verdict, which
// on_error=ignore (the default) tolerates.
func New(name string, cfg perm.ClassifierConfig, key KeyFunc, httpc *http.Client) *Classifier {
	return &Classifier{
		name: name,
		cfg:  cfg,
		cl: &Client{
			BaseURL: cfg.BaseURL, Path: cfg.Path, Model: cfg.Model, KeyEnv: cfg.KeyEnv,
			SessionHeader: cfg.SessionHeader, Encoding: cfg.StateEncoding,
			Key: key, HTTP: httpc,
		},
	}
}

// Name implements perm.Classifier.
func (c *Classifier) Name() string { return c.name }

// questions returns the classifier's question set: the configured one with
// per-question fallbacks (questionFrom), or Questions() verbatim when none
// is configured.
func (c *Classifier) questions() map[string]Question {
	if len(c.cfg.Questions) == 0 {
		return Questions()
	}
	out := make(map[string]Question, len(c.cfg.Questions))
	for name, q := range c.cfg.Questions {
		out[name] = questionFrom(name, q)
	}
	return out
}

// questionFrom fills a configured question's optional fields from the
// built-in question of the same name (or a noul default when there is none);
// explicit settings always win.
func questionFrom(name string, q perm.QuestionConfig) Question {
	out := Question{Type: "noul"}
	if b, ok := AllQuestions()[name]; ok {
		out = b
	}
	if q.Type != "" {
		out.Type = q.Type
	}
	if q.Instructions != "" {
		out.Instructions = q.Instructions
	}
	if len(q.Criteria) > 0 {
		out.Criteria = q.Criteria
	}
	return out
}

// Classify implements perm.Classifier: build the scrubbed state (§4.3), ask
// the three permission questions, and flag on any thresholded probability.
func (c *Classifier) Classify(ctx context.Context, call perm.Call) perm.Verdict {
	v := perm.Verdict{Classifier: c.name}
	if c.cl.Model == "" {
		v.Err = fmt.Errorf("decisions: classifier %s has no model configured", c.name)
		return v
	}
	res, cost, err := c.cl.AskCost(ctx, CallState(call), c.questions())
	v.Cost = cost
	if err != nil {
		v.Err = err
		return v
	}
	v.Flagged, v.Score, v.Reason = Apply(res, c.cfg.Thresholds)
	return v
}

// CallState builds the scrubbed decisions state for one call (PLAN.md §4.3):
// task, the last few turns (tool output withheld), cwd, tool and args.
func CallState(c perm.Call) map[string]any {
	recent := make([]map[string]string, 0, len(c.Recent))
	for _, m := range c.Recent {
		recent = append(recent, map[string]string{
			"role": m.Role,
			"text": perm.Scrub(clip(m.Text, 2000)),
		})
	}
	return map[string]any{
		"task":            perm.Scrub(clip(c.Task, 4000)),
		"recent_messages": recent,
		"cwd":             c.Cwd,
		"tool":            c.Tool,
		"args":            perm.Scrub(clip(string(c.Args), 6000)),
	}
}

// rules are gone: Apply (below) evaluates whatever questions a classifier
// asked, so configured question sets work without special-casing.

// Apply flags when any thresholded probability is at or above its threshold,
// returning the tripped ones for the reason string ("irreversible 0.91, …").
// It is generic over the question set: a noul result is compared against the
// threshold keyed by its question name (so arbitrary configured noul names
// work, e.g. off_task_or_injected), a choice/score result against the
// thresholds keyed by its option names. Names without a threshold never trip.
func Apply(results map[string]Result, thresholds map[string]float64) (bool, float64, string) {
	var (
		tripped []string
		score   float64
	)
	trip := func(label string, p float64) {
		tripped = append(tripped, fmt.Sprintf("%s %.2f", label, p))
		if p > score {
			score = p
		}
	}
	for name, res := range results {
		if p, ok := res.Probabilities["true"]; ok {
			// noul: the threshold is keyed by the question name.
			if th, ok := thresholds[name]; ok && p >= th {
				trip(name, p)
			}
		}
		for opt, p := range res.Probabilities {
			if th, ok := thresholds[opt]; ok && p >= th {
				trip(opt, p)
			}
		}
	}
	if len(tripped) == 0 {
		return false, score, ""
	}
	sort.Strings(tripped)
	return true, score, strings.Join(tripped, ", ")
}
