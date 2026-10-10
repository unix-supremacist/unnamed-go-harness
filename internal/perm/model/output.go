// OutputJudge: the untrusted-output check (PLAN.md §4.3). After a tool runs,
// OUTPUT_QUESTION asks whether the result contains instructions addressed to
// an AI agent; a flag raises that agent's next permission prompt.
package model

import (
	"context"
	"fmt"
	"net/http"

	"github.com/unix/ugh/internal/perm"
)

// OutputJudge runs the untrusted-output question on tool results.
type OutputJudge struct {
	cfg perm.ClassifierConfig
	cl  *Client
}

// NewOutputJudge builds the judge from the output_check config entry.
func NewOutputJudge(cfg perm.ClassifierConfig, key KeyFunc, httpc *http.Client) *OutputJudge {
	return &OutputJudge{
		cfg: cfg,
		cl: &Client{
			BaseURL: cfg.BaseURL, Path: cfg.Path, Model: cfg.Model, KeyEnv: cfg.KeyEnv,
			SessionHeader: cfg.SessionHeader, Encoding: cfg.StateEncoding,
			Key: key, HTTP: httpc,
		},
	}
}

// ClassifyOutput implements perm.OutputJudge. The threshold for the noul
// probability is the output_instructions entry, as in pisup.
func (o *OutputJudge) ClassifyOutput(ctx context.Context, agent, tool, output string) perm.Verdict {
	v := perm.Verdict{Classifier: perm.OutputCheckName}
	if o == nil || o.cl.Model == "" {
		v.Err = fmt.Errorf("decisions: output check has no model configured")
		return v
	}
	state := map[string]any{
		"agent":  agent,
		"tool":   tool,
		"output": perm.Scrub(clip(output, 6000)),
	}
	res, cost, err := o.cl.AskCost(ctx, state, OutputQuestions())
	v.Cost = cost
	if err != nil {
		v.Err = err
		return v
	}
	r, ok := res["instructions"]
	if !ok {
		v.Err = fmt.Errorf("decisions: output check returned no answer")
		return v
	}
	p := r.Probabilities["true"]
	v.Score = p
	if th, ok := o.cfg.Thresholds["output_instructions"]; ok && p >= th {
		v.Flagged = true
		v.Reason = fmt.Sprintf("instructions %.2f", p)
	}
	return v
}
