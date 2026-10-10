package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/unix/ugh/internal/llm"
)

// AskOption is one choice of an ask_user question.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// AskQuestion is one question of an ask_user call.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multi_select"`
}

// AskRequest is what an interactive UI is asked to put in front of the user.
type AskRequest struct {
	Agent     string
	Questions []AskQuestion
}

// AskReply is the user's answer to one question: chosen labels and/or a
// write-in ("Other").
type AskReply struct {
	Labels []string
	Other  string
}

// AskAnswer answers a whole request. Declined means the user dismissed it (or
// the UI went away); Replies is then empty.
type AskAnswer struct {
	Declined bool
	Replies  []AskReply
}

const (
	askMaxQuestions = 4
	askMinOptions   = 2
	askMaxOptions   = 6
	askMaxHeader    = 12
)

type askUserTool struct{ agent *Agent }

func (askUserTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "ask_user",
		Description: "Ask the user 1-4 multiple-choice questions when you are blocked on a decision that is theirs to make " +
			"and cannot be resolved from the request, the code or sensible defaults. Each question has 2-6 options; the user " +
			"can always write in their own answer, so do not add an \"Other\" option. Do not ask what you can find out yourself.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{
					"type": "array", "minItems": 1, "maxItems": askMaxQuestions,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"question": map[string]any{"type": "string", "description": "The complete question, ending with a question mark."},
							"header":   map[string]any{"type": "string", "description": "A very short label (max 12 characters)."},
							"options": map[string]any{
								"type": "array", "minItems": askMinOptions, "maxItems": askMaxOptions,
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"label":       map[string]any{"type": "string", "description": "1-5 words; unique within the question."},
										"description": map[string]any{"type": "string", "description": "What choosing it means."},
									},
									"required": []string{"label", "description"},
								},
							},
							"multi_select": map[string]any{"type": "boolean"},
						},
						"required": []string{"question", "header", "options"},
					},
				},
			},
			"required": []string{"questions"},
		},
	}
}

// validateAsk checks the model's arguments at runtime; the schema alone is not
// trusted.
func validateAsk(qs []AskQuestion) error {
	if len(qs) < 1 || len(qs) > askMaxQuestions {
		return fmt.Errorf("ask_user: need 1-%d questions, got %d", askMaxQuestions, len(qs))
	}
	for i, q := range qs {
		at := fmt.Sprintf("ask_user: question %d", i+1)
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("%s: empty question", at)
		}
		if strings.TrimSpace(q.Header) == "" || utf8.RuneCountInString(q.Header) > askMaxHeader {
			return fmt.Errorf("%s: header must be 1-%d characters", at, askMaxHeader)
		}
		if len(q.Options) < askMinOptions || len(q.Options) > askMaxOptions {
			return fmt.Errorf("%s: need %d-%d options, got %d", at, askMinOptions, askMaxOptions, len(q.Options))
		}
		seen := map[string]bool{}
		for _, o := range q.Options {
			l := strings.TrimSpace(o.Label)
			switch {
			case l == "":
				return fmt.Errorf("%s: empty option label", at)
			case strings.EqualFold(l, "other"):
				return fmt.Errorf(`%s: "Other" is reserved; the user can always write in an answer`, at)
			case seen[strings.ToLower(l)]:
				return fmt.Errorf("%s: duplicate option label %q", at, l)
			}
			seen[strings.ToLower(l)] = true
		}
	}
	return nil
}

func (t askUserTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var args struct {
		Questions []AskQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("ask_user: bad arguments: %v", err)
	}
	if err := validateAsk(args.Questions); err != nil {
		return "", err
	}
	ask := t.agent.cfg.Context.AskUser
	if ask == nil {
		return "", fmt.Errorf("ask_user: no user is attached to this run")
	}
	ans, err := ask(ctx, AskRequest{Agent: t.agent.cfg.Agent, Questions: args.Questions})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "declined", nil
	}
	return FormatAskAnswer(args.Questions, ans), nil
}

// FormatAskAnswer renders the answer the model sees: one `Q → A` line per
// question, or "declined".
func FormatAskAnswer(qs []AskQuestion, ans AskAnswer) string {
	if ans.Declined || len(ans.Replies) != len(qs) {
		return "declined"
	}
	var b strings.Builder
	for i, q := range qs {
		r := ans.Replies[i]
		parts := []string{}
		if len(r.Labels) > 0 {
			parts = append(parts, strings.Join(r.Labels, ", "))
		}
		if strings.TrimSpace(r.Other) != "" {
			parts = append(parts, fmt.Sprintf("other: %q", r.Other))
		}
		if len(parts) == 0 {
			parts = append(parts, "(no answer)")
		}
		fmt.Fprintf(&b, "%s → %s\n", q.Question, strings.Join(parts, " | "))
	}
	return strings.TrimRight(b.String(), "\n")
}
