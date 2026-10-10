// Package agent implements the agent loop and built-in agent-level tools.
// gutcheck.go ports the bounded gutcheck tool from pi-sup (PLAN_ROUTING_ORCHESTRATION.md §5).
// It evaluates 1–8 bounded questions (boolean, choice, score) over context state
// and optional evidence files, returning structured responses with probabilities,
// confidence, and explicit uncertainty advisories.
//
// Gutcheck is strictly advisory and cannot authorize actions or bypass approvals.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/perm/model"
	"github.com/unix/ugh/internal/tools"
)

const (
	// DefaultGutcheckToolName is the canonical tool name.
	DefaultGutcheckToolName = "gutcheck"

	// DefaultDecisiveThreshold is the minimum max-probability for a decision to be considered decisive.
	DefaultDecisiveThreshold = 0.8

	// MaxStateBytes bounds context state size (8 KB).
	MaxStateBytes = 8192

	// MaxFileBytes bounds individual evidence file size (8 KB).
	MaxFileBytes = 8192

	// MaxFiles bounds the number of evidence files allowed per call.
	MaxFiles = 8

	// MinQuestions is the lower bound of questions per call.
	MinQuestions = 1

	// MaxQuestions is the upper bound of questions per call.
	MaxQuestions = 8

	// MaxQuestionTextLen bounds question text length.
	MaxQuestionTextLen = 500

	// MaxOptionTextLen bounds individual choice option length.
	MaxOptionTextLen = 100

	// MinChoiceOptions is the minimum number of choices for a choice question.
	MinChoiceOptions = 2

	// MaxChoiceOptions is the maximum number of choices for a choice question.
	MaxChoiceOptions = 32

	// AdvisoryWarning is the required notice that gutcheck cannot authorize actions.
	AdvisoryWarning = "Advisory: gutcheck is probabilistic and cannot authorize actions or bypass approvals. Always verify uncertain answers."
)

var (
	questionIDRx = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,40}$`)
	secretFileRx = regexp.MustCompile(`(?i)^(?:\.env(?:\..*)?|id_[^/]*|\.npmrc|\.netrc|credentials|secrets?(?:\..*)?|.*?\.(?:pem|key|p12|pfx))$`)
)

// DecisionsAsker evaluates bounded questions against context state.
// *model.Client implements this interface.
type DecisionsAsker interface {
	Ask(ctx context.Context, state any, questions map[string]model.Question) (map[string]model.Result, error)
}

// DefaultDecisionClient returns a configured DecisionsAsker using cfg.Routing.Routers["jev"].
// It avoids automatic paid entitlement probes and keeps routing configuration
// completely separate from permission-classifier enablement.
func DefaultDecisionClient() (DecisionsAsker, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("gutcheck: load config: %w", err)
	}
	rc, ok := cfg.Routing.Routers["jev"]
	if !ok || rc.BaseURL == "" || rc.Model == "" {
		rc = config.Default().Routing.Routers["jev"]
	}
	storePath := cfg.Auth.Store
	keyFn := func(ctx context.Context) (string, error) {
		keyEnv := rc.KeyEnv
		if keyEnv == "" {
			keyEnv = "OPENROUTER_API_KEY"
		}
		if k := os.Getenv(keyEnv); k != "" {
			perm.RegisterSecret(k)
			return k, nil
		}
		if keyEnv != "OPENROUTER_API_KEY" {
			return "", fmt.Errorf("gutcheck: set %s for the configured decision endpoint", keyEnv)
		}
		store, err := auth.Open(storePath)
		if err != nil {
			return "", err
		}
		key, err := auth.Resolve("openrouter", os.LookupEnv, store)
		if err == nil {
			perm.RegisterSecret(key)
		}
		return key, err
	}
	return &model.Client{
		BaseURL:       rc.BaseURL,
		Path:          rc.Path,
		Model:         rc.Model,
		KeyEnv:        rc.KeyEnv,
		SessionHeader: rc.SessionHeader,
		Key:           keyFn,
	}, nil
}

// Rubric defines the scoring range and anchors for a score question.
type Rubric struct {
	Min     int      `json:"min"`
	Max     int      `json:"max"`
	Anchors []string `json:"anchors"`
}

// GutcheckQuestion represents one question in a gutcheck request.
type GutcheckQuestion struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // boolean | choice | score
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	Rubric   *Rubric  `json:"rubric,omitempty"`
	Anchors  []string `json:"anchors,omitempty"` // convenience alias for rubric.anchors
}

// GutcheckInput is the raw JSON argument payload received by gutcheck.
type GutcheckInput struct {
	State     any                `json:"state"`
	Files     []string           `json:"files,omitempty"`
	Questions []GutcheckQuestion `json:"questions"`
}

// FileEvidence holds a scrubbed file path and content.
type FileEvidence struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// QuestionResult represents one evaluated question in the structured response.
type QuestionResult struct {
	ID            string             `json:"id"`
	Type          string             `json:"type"`
	Answer        any                `json:"answer"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Decisive      bool               `json:"decisive"`
	Warning       string             `json:"warning,omitempty"`
}

// GutcheckTool implements the tools.Tool interface for bounded advisory gutchecks.
type GutcheckTool struct {
	Name              string
	Asker             DecisionsAsker
	DecisiveThreshold float64
	Timeout           time.Duration
}

var _ tools.Tool = (*GutcheckTool)(nil)

// NewGutcheckTool creates a gutcheck tool with plain struct defaults.
func NewGutcheckTool(asker DecisionsAsker) *GutcheckTool {
	return &GutcheckTool{
		Name:              DefaultGutcheckToolName,
		Asker:             asker,
		DecisiveThreshold: DefaultDecisiveThreshold,
	}
}

// Spec returns the model-facing tool declaration.
func (t *GutcheckTool) Spec() llm.ToolSpec {
	name := t.Name
	if name == "" {
		name = DefaultGutcheckToolName
	}
	return llm.ToolSpec{
		Name:        name,
		Description: "Quick probabilistic gut check for bounded questions; advisory only, cannot authorize actions or bypass approvals. One factor per question; verify uncertain answers with tests or domain tools.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{
					"description": "Context state for questions (string or object, max 8 KB)",
				},
				"files": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional list of project-relative evidence files (max 8 files, each max 8 KB)",
				},
				"questions": map[string]any{
					"type":        "array",
					"description": "1 to 8 bounded questions to evaluate",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":       map[string]any{"type": "string", "description": "Unique identifier (alphanumeric, max 40 chars)"},
							"type":     map[string]any{"type": "string", "enum": []string{"boolean", "choice", "score"}, "description": "Question type"},
							"question": map[string]any{"type": "string", "description": "Question text (max 500 chars, single factor)"},
							"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Options for choice question (2 to 32 options)"},
							"rubric": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"min":     map[string]any{"type": "integer"},
									"max":     map[string]any{"type": "integer"},
									"anchors": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
								},
								"description": "Rubric for score question (min >= 0, min < max <= 31, one anchor per level)",
							},
						},
						"required": []string{"id", "type", "question"},
					},
				},
			},
			"required": []string{"state", "questions"},
		},
	}
}

// Run executes the gutcheck tool with strict input validation and bounded execution.
func (t *GutcheckTool) Run(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var input GutcheckInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}

	preparedState, decQuestions, err := prepareGutcheck(input, cwd)
	if err != nil {
		return "", err
	}

	asker := t.Asker
	if asker == nil {
		var clientErr error
		asker, clientErr = DefaultDecisionClient()
		if clientErr != nil {
			return "", fmt.Errorf("gutcheck client: %w", clientErr)
		}
	}

	callCtx := ctx
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}

	answers, err := asker.Ask(callCtx, preparedState, decQuestions)
	if err != nil {
		return "", fmt.Errorf("gutcheck decisions: %w", err)
	}

	threshold := t.DecisiveThreshold
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultDecisiveThreshold
	}

	entries := make([]QuestionResult, 0, len(input.Questions))
	decisiveCount := 0
	uncertainCount := 0

	for _, q := range input.Questions {
		ans, ok := answers[q.ID]
		if !ok || ans.Answer == nil || len(ans.Probabilities) == 0 {
			return "", fmt.Errorf("gutcheck: missing or malformed answer for question %q", q.ID)
		}

		maxProb := 0.0
		sumProb := 0.0
		for _, p := range ans.Probabilities {
			if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
				return "", fmt.Errorf("gutcheck: non-finite probability in distribution for question %q", q.ID)
			}
			sumProb += p
			if p > maxProb {
				maxProb = p
			}
		}
		if math.Abs(sumProb-1.0) > 0.05 {
			return "", fmt.Errorf("gutcheck: probabilities do not sum to 1.0 for question %q (sum=%.3f)", q.ID, sumProb)
		}

		finalAnswer := ans.Answer
		probs := ans.Probabilities

		// Support ordinary bounded integer rubric min/max: translate returned ordinal score with offset
		if q.Type == "score" && q.Rubric != nil && q.Rubric.Min != 0 {
			minOffset := q.Rubric.Min
			if scoreF, ok := ans.Answer.(float64); ok {
				finalAnswer = scoreF + float64(minOffset)
			} else if scoreI, ok := ans.Answer.(int); ok {
				finalAnswer = float64(scoreI + minOffset)
			}
			translatedProbs := make(map[string]float64, len(probs))
			for k, v := range probs {
				var idx int
				if _, err := fmt.Sscanf(k, "%d", &idx); err == nil {
					translatedProbs[fmt.Sprint(idx+minOffset)] = v
				} else {
					translatedProbs[k] = v
				}
			}
			probs = translatedProbs
		}

		isDecisive := maxProb >= threshold
		if isDecisive {
			decisiveCount++
		} else {
			uncertainCount++
		}

		conf := ans.Confidence
		entry := QuestionResult{
			ID:            q.ID,
			Type:          q.Type,
			Answer:        finalAnswer,
			Probabilities: probs,
			Confidence:    &conf,
			Decisive:      isDecisive,
		}
		if !isDecisive {
			entry.Warning = fmt.Sprintf("Uncertain: max probability %.2f < %.2f decisive threshold. Verify with tools.", maxProb, threshold)
		}
		entries = append(entries, entry)
	}

	var sb strings.Builder
	sb.WriteString(AdvisoryWarning)
	sb.WriteString("\n")
	if uncertainCount > 0 {
		fmt.Fprintf(&sb, "Warning: %d uncertain question(s) (highest probability < %.2f); verify independently before acting.\n", uncertainCount, threshold)
	}
	fmt.Fprintf(&sb, "%d decisive, %d uncertain\n", decisiveCount, uncertainCount)

	rawEntries, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode gutcheck entries: %w", err)
	}
	sb.Write(rawEntries)

	return sb.String(), nil
}

// prepareGutcheck performs strict input validation and prepares the payload.
func prepareGutcheck(input GutcheckInput, cwd string) (map[string]any, map[string]model.Question, error) {
	if input.State == nil {
		return nil, nil, errors.New("state required")
	}

	switch input.State.(type) {
	case string, map[string]any:
		// supported state types
	default:
		return nil, nil, errors.New("state must be string or object")
	}

	stateRaw, err := json.Marshal(input.State)
	if err != nil {
		return nil, nil, fmt.Errorf("encode state: %w", err)
	}
	if len(stateRaw) > MaxStateBytes {
		return nil, nil, errors.New("state exceeds 8 KB")
	}

	if len(input.Questions) < MinQuestions || len(input.Questions) > MaxQuestions {
		return nil, nil, errors.New("provide 1–8 questions")
	}

	decQuestions := make(map[string]model.Question, len(input.Questions))
	seenIDs := make(map[string]bool, len(input.Questions))

	for i := range input.Questions {
		q := &input.Questions[i]
		if !questionIDRx.MatchString(q.ID) || seenIDs[q.ID] {
			return nil, nil, errors.New("question IDs must be unique identifiers")
		}
		seenIDs[q.ID] = true

		if q.Question == "" || len(q.Question) > MaxQuestionTextLen {
			return nil, nil, errors.New("question text required (max 500 chars)")
		}

		switch q.Type {
		case "boolean", "noul":
			decQuestions[q.ID] = model.Noul(q.Question)

		case "choice":
			opts := q.Options
			if len(opts) == 0 && q.Rubric != nil && len(q.Rubric.Anchors) > 0 {
				opts = q.Rubric.Anchors
			}
			if len(opts) < MinChoiceOptions || len(opts) > MaxChoiceOptions {
				return nil, nil, errors.New("choice needs 2–32 distinct options")
			}
			optSet := make(map[string]bool, len(opts))
			criteria := make(map[string]string, len(opts)+1)
			hasUnclear := false
			for _, opt := range opts {
				if opt == "" || len(opt) > MaxOptionTextLen || optSet[opt] {
					return nil, nil, errors.New("choice needs 2–32 distinct options")
				}
				optSet[opt] = true
				criteria[opt] = opt
				if strings.EqualFold(opt, "unclear") {
					hasUnclear = true
				}
			}
			if !hasUnclear {
				criteria["unclear"] = "Not enough evidence"
			}
			decQuestions[q.ID] = model.Choice(q.Question, criteria)

		case "score":
			r := q.Rubric
			if r == nil {
				anchors := q.Anchors
				if len(anchors) == 0 && len(q.Options) > 0 {
					anchors = q.Options
				}
				if len(anchors) >= MinChoiceOptions && len(anchors) <= MaxChoiceOptions {
					r = &Rubric{Min: 0, Max: len(anchors) - 1, Anchors: anchors}
					q.Rubric = r
				}
			}
			expectedAnchors := 0
			if r != nil {
				expectedAnchors = r.Max - r.Min + 1
			}
			if r == nil || r.Min < 0 || r.Min >= r.Max || r.Max > 31 || len(r.Anchors) != expectedAnchors {
				return nil, nil, errors.New("score needs min >= 0, min < max <= 31, and one anchor per level")
			}
			for _, a := range r.Anchors {
				if a == "" {
					return nil, nil, errors.New("score needs min >= 0, min < max <= 31, and one anchor per level")
				}
			}
			decQuestions[q.ID] = model.Score(q.Question, r.Anchors)

		default:
			return nil, nil, errors.New("unsupported question type")
		}
	}

	if len(input.Files) > MaxFiles {
		return nil, nil, errors.New("at most 8 files")
	}

	files := make([]FileEvidence, 0, len(input.Files))
	if len(input.Files) > 0 {
		if cwd == "" {
			cwd = "."
		}
		root, err := os.OpenRoot(cwd)
		if err != nil {
			return nil, nil, fmt.Errorf("open root: %w", err)
		}
		defer root.Close()

		for _, p := range input.Files {
			if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "~") || hasDotDot(p) || isSecretFile(p) {
				return nil, nil, errors.New("files must be non-secret project-relative paths")
			}
			f, err := root.Open(p)
			if err != nil {
				return nil, nil, fmt.Errorf("file outside project or secret-like: %w", err)
			}
			st, err := f.Stat()
			if err != nil || !st.Mode().IsRegular() {
				_ = f.Close()
				return nil, nil, errors.New("file exceeds 8 KB or is not a file")
			}
			content, err := io.ReadAll(io.LimitReader(f, int64(MaxFileBytes)+1))
			_ = f.Close()
			if err != nil {
				return nil, nil, fmt.Errorf("read file %q: %w", p, err)
			}
			if len(content) > MaxFileBytes {
				return nil, nil, errors.New("file exceeds 8 KB or is not a file")
			}
			files = append(files, FileEvidence{
				Path:    filepath.ToSlash(p),
				Content: perm.Scrub(string(content)),
			})
		}
	}

	preparedState := map[string]any{
		"user_state": perm.Scrub(string(stateRaw)),
		"files":      files,
	}

	return preparedState, decQuestions, nil
}

func hasDotDot(p string) bool {
	parts := strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for _, part := range parts {
		if part == ".." {
			return true
		}
	}
	return false
}

func isSecretFile(p string) bool {
	base := filepath.Base(p)
	return secretFileRx.MatchString(base)
}
