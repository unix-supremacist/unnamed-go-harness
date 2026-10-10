package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/perm/model"
)

// fakeAsker records calls and returns canned answers or an error.
type fakeAsker struct {
	state     any
	questions map[string]model.Question
	answers   map[string]model.Result
	err       error
}

func (f *fakeAsker) Ask(_ context.Context, state any, questions map[string]model.Question) (map[string]model.Result, error) {
	f.state = state
	f.questions = questions
	if f.err != nil {
		return nil, f.err
	}
	return f.answers, nil
}

func TestGutcheckSpec(t *testing.T) {
	tool := NewGutcheckTool(nil)
	sp := tool.Spec()
	if sp.Name != "gutcheck" {
		t.Fatalf("expected tool name 'gutcheck', got %q", sp.Name)
	}
	if !strings.Contains(sp.Description, "advisory only") || !strings.Contains(sp.Description, "cannot authorize actions") {
		t.Fatalf("spec description should emphasize advisory nature: %q", sp.Description)
	}
	props, ok := sp.Parameters["properties"].(map[string]any)
	if !ok || props["state"] == nil || props["questions"] == nil {
		t.Fatalf("spec parameters missing state or questions properties: %v", props)
	}

	customTool := &GutcheckTool{Name: "gut_check"}
	if customTool.Spec().Name != "gut_check" {
		t.Fatalf("custom tool name not respected: got %q", customTool.Spec().Name)
	}
}

func TestGutcheckMalformedArguments(t *testing.T) {
	tool := NewGutcheckTool(&fakeAsker{})
	_, err := tool.Run(context.Background(), t.TempDir(), json.RawMessage(`{invalid-json`))
	if err == nil || !strings.Contains(err.Error(), "bad arguments") {
		t.Fatalf("expected bad arguments error, got %v", err)
	}
}

func TestGutcheckStateValidation(t *testing.T) {
	tool := NewGutcheckTool(&fakeAsker{})
	dir := t.TempDir()

	// Missing state
	_, err := tool.Run(context.Background(), dir, json.RawMessage(`{"questions":[{"id":"q1","type":"boolean","question":"is this valid"}]}`))
	if err == nil || !strings.Contains(err.Error(), "state required") {
		t.Fatalf("expected 'state required', got: %v", err)
	}

	// State of unsupported type (number)
	_, err = tool.Run(context.Background(), dir, json.RawMessage(`{"state": 12345, "questions":[{"id":"q1","type":"boolean","question":"is this valid"}]}`))
	if err == nil || !strings.Contains(err.Error(), "state must be string or object") {
		t.Fatalf("expected 'state must be string or object', got: %v", err)
	}

	// State exceeding 8 KB
	largeState := strings.Repeat("x", 8193)
	payload, _ := json.Marshal(map[string]any{
		"state":     largeState,
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is this valid"}},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "state exceeds 8 KB") {
		t.Fatalf("expected 'state exceeds 8 KB', got: %v", err)
	}
}

func TestGutcheckQuestionsCountValidation(t *testing.T) {
	tool := NewGutcheckTool(&fakeAsker{})
	dir := t.TempDir()

	// 0 questions
	_, err := tool.Run(context.Background(), dir, json.RawMessage(`{"state":"clean","questions":[]}`))
	if err == nil || !strings.Contains(err.Error(), "provide 1–8 questions") {
		t.Fatalf("expected 'provide 1–8 questions', got: %v", err)
	}

	// 9 questions (> 8)
	var qs []map[string]any
	for i := 1; i <= 9; i++ {
		qs = append(qs, map[string]any{
			"id":       fmt.Sprintf("q%d", i),
			"type":     "boolean",
			"question": fmt.Sprintf("is factor %d valid", i),
		})
	}
	payload, _ := json.Marshal(map[string]any{"state": "clean", "questions": qs})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "provide 1–8 questions") {
		t.Fatalf("expected 'provide 1–8 questions', got: %v", err)
	}
}

func TestGutcheckQuestionIDValidation(t *testing.T) {
	tool := NewGutcheckTool(&fakeAsker{})
	dir := t.TempDir()

	invalidIDs := []string{
		"1startsWithDigit",
		"has-hyphen",
		"has space",
		"has$dollar",
		"",
		strings.Repeat("a", 42), // max is 41 (1 initial + up to 40)
	}

	for _, id := range invalidIDs {
		payload, _ := json.Marshal(map[string]any{
			"state": "test",
			"questions": []any{
				map[string]any{"id": id, "type": "boolean", "question": "valid question"},
			},
		})
		_, err := tool.Run(context.Background(), dir, payload)
		if err == nil || !strings.Contains(err.Error(), "question IDs must be unique identifiers") {
			t.Errorf("expected unique identifier error for ID %q, got: %v", id, err)
		}
	}

	// Duplicate question IDs
	dupPayload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "dup_id", "type": "boolean", "question": "first question"},
			map[string]any{"id": "dup_id", "type": "boolean", "question": "second question"},
		},
	})
	_, err := tool.Run(context.Background(), dir, dupPayload)
	if err == nil || !strings.Contains(err.Error(), "question IDs must be unique identifiers") {
		t.Fatalf("expected unique identifier error for duplicate ID, got: %v", err)
	}
}

func TestGutcheckQuestionTextValidation(t *testing.T) {
	fake := &fakeAsker{
		answers: map[string]model.Result{
			"q1": {
				Probabilities: map[string]float64{"true": 0.9, "false": 0.1},
				Answer:        true,
				Confidence:    0.9,
			},
		},
	}
	tool := NewGutcheckTool(fake)
	dir := t.TempDir()

	// Empty question text
	payload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "boolean", "question": ""},
		},
	})
	_, err := tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "question text required (max 500 chars)") {
		t.Fatalf("expected empty question text error, got: %v", err)
	}

	// Question text > 500 chars
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "boolean", "question": strings.Repeat("x", 501)},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "question text required (max 500 chars)") {
		t.Fatalf("expected question text > 500 chars error, got: %v", err)
	}

	// Questions with English conjunctions or arithmetic must NOT be heuristically banned
	allowedQuestions := []string{
		"Is this refactor safe and correct?",
		"Should we revert or apply the change?",
		"Does it pass tests AND compile?",
		"Check lint OR test failure",
		"Calculate the token count for this file",
		"What is the sum of these metrics?",
		"What is 42 + 7?",
		"Is 100 * 3 within limits?",
	}
	for _, aq := range allowedQuestions {
		payload, _ = json.Marshal(map[string]any{
			"state": "test",
			"questions": []any{
				map[string]any{"id": "q1", "type": "boolean", "question": aq},
			},
		})
		_, err = tool.Run(context.Background(), dir, payload)
		if err != nil {
			t.Errorf("legitimate question %q should be accepted without conjunction/arithmetic bans: %v", aq, err)
		}
	}
}

func TestGutcheckChoiceValidation(t *testing.T) {
	tool := NewGutcheckTool(&fakeAsker{})
	dir := t.TempDir()

	// Less than 2 options
	payload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "choice", "question": "which path", "options": []string{"opt1"}},
		},
	})
	_, err := tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "choice needs 2–32 distinct options") {
		t.Fatalf("expected choice 2-32 options error, got: %v", err)
	}

	// Duplicate options
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "choice", "question": "which path", "options": []string{"dup", "dup"}},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "choice needs 2–32 distinct options") {
		t.Fatalf("expected choice distinct options error, got: %v", err)
	}

	// Option exceeding 100 chars
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "choice", "question": "which path", "options": []string{"opt1", strings.Repeat("x", 101)}},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "choice needs 2–32 distinct options") {
		t.Fatalf("expected option max length error, got: %v", err)
	}

	// More than 32 options
	var tooManyOpts []string
	for i := 1; i <= 33; i++ {
		tooManyOpts = append(tooManyOpts, fmt.Sprintf("opt%d", i))
	}
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "choice", "question": "which path", "options": tooManyOpts},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "choice needs 2–32 distinct options") {
		t.Fatalf("expected > 32 options error, got: %v", err)
	}
}

func TestGutcheckScoreValidation(t *testing.T) {
	fake := &fakeAsker{
		answers: map[string]model.Result{
			"q1": {
				Probabilities: map[string]float64{"0": 0.1, "1": 0.9},
				Answer:        float64(1),
				Confidence:    0.8,
			},
		},
	}
	tool := NewGutcheckTool(fake)
	dir := t.TempDir()

	// Nil rubric and no options/anchors
	payload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "score", "question": "quality rating"},
		},
	})
	_, err := tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "score needs min >= 0") {
		t.Fatalf("expected score rubric error, got: %v", err)
	}

	// Negative Min
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{
				"id": "q1", "type": "score", "question": "quality rating",
				"rubric": map[string]any{"min": -1, "max": 2, "anchors": []string{"a", "b", "c", "d"}},
			},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "score needs min >= 0") {
		t.Fatalf("expected negative min error, got: %v", err)
	}

	// Min >= Max
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{
				"id": "q1", "type": "score", "question": "quality rating",
				"rubric": map[string]any{"min": 5, "max": 5, "anchors": []string{"single"}},
			},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "score needs min >= 0") {
		t.Fatalf("expected min >= max error, got: %v", err)
	}

	// Max > 31
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{
				"id": "q1", "type": "score", "question": "quality rating",
				"rubric": map[string]any{"min": 0, "max": 32, "anchors": make([]string, 33)},
			},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "score needs min >= 0") {
		t.Fatalf("expected score max <= 31 error, got: %v", err)
	}

	// Anchors count mismatch
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{
				"id": "q1", "type": "score", "question": "quality rating",
				"rubric": map[string]any{"min": 1, "max": 5, "anchors": []string{"low", "mid", "high"}}, // 3 anchors instead of 5
			},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "score needs min >= 0") {
		t.Fatalf("expected anchors count mismatch error, got: %v", err)
	}

	// Ordinary bounded 1-based scale (1 to 2) with correct anchors must SUCCEED
	payload, _ = json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{
				"id": "q1", "type": "score", "question": "quality rating",
				"rubric": map[string]any{"min": 1, "max": 2, "anchors": []string{"acceptable", "comprehensive"}},
			},
		},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err != nil {
		t.Fatalf("bounded 1-based score rubric (min 1, max 2) should be accepted: %v", err)
	}
}

func TestGutcheckFilesValidation(t *testing.T) {
	tool := NewGutcheckTool(&fakeAsker{})
	dir := t.TempDir()

	// More than 8 files
	var files []string
	for i := 1; i <= 9; i++ {
		files = append(files, fmt.Sprintf("file%d.txt", i))
	}
	payload, _ := json.Marshal(map[string]any{
		"state":     "test",
		"files":     files,
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
	})
	_, err := tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "at most 8 files") {
		t.Fatalf("expected 'at most 8 files', got: %v", err)
	}

	// Absolute path
	payload, _ = json.Marshal(map[string]any{
		"state":     "test",
		"files":     []string{"/etc/passwd"},
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "files must be non-secret project-relative paths") {
		t.Fatalf("expected non-secret project-relative paths error for absolute path, got: %v", err)
	}

	// Path with ~
	payload, _ = json.Marshal(map[string]any{
		"state":     "test",
		"files":     []string{"~/secret.txt"},
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "files must be non-secret project-relative paths") {
		t.Fatalf("expected non-secret project-relative paths error for ~ path, got: %v", err)
	}

	// Path with .. traversal
	payload, _ = json.Marshal(map[string]any{
		"state":     "test",
		"files":     []string{"sub/../../secret.txt"},
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "files must be non-secret project-relative paths") {
		t.Fatalf("expected non-secret project-relative paths error for .. path, got: %v", err)
	}

	// Secret-looking files
	secretNames := []string{".env", ".env.local", "id_rsa", ".npmrc", ".netrc", "credentials", "secrets.json", "cert.pem", "server.key", "app.p12", "app.pfx"}
	for _, sn := range secretNames {
		payload, _ = json.Marshal(map[string]any{
			"state":     "test",
			"files":     []string{sn},
			"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
		})
		_, err = tool.Run(context.Background(), dir, payload)
		if err == nil || !strings.Contains(err.Error(), "files must be non-secret project-relative paths") {
			t.Errorf("expected secret file error for %q, got: %v", sn, err)
		}
	}

	// File exceeding 8 KB
	largeFile := filepath.Join(dir, "large.txt")
	_ = os.WriteFile(largeFile, make([]byte, 8193), 0o600)
	payload, _ = json.Marshal(map[string]any{
		"state":     "test",
		"files":     []string{"large.txt"},
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "file exceeds 8 KB or is not a file") {
		t.Fatalf("expected file exceeds 8 KB error, got: %v", err)
	}

	// Symlink pointing outside project root
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	_ = os.WriteFile(outsideFile, []byte("outside"), 0o600)
	symlinkPath := filepath.Join(dir, "escaped.txt")
	_ = os.Symlink(outsideFile, symlinkPath)

	payload, _ = json.Marshal(map[string]any{
		"state":     "test",
		"files":     []string{"escaped.txt"},
		"questions": []any{map[string]any{"id": "q1", "type": "boolean", "question": "is ok"}},
	})
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "file outside project or secret-like") {
		t.Fatalf("expected outside project error for symlink, got: %v", err)
	}
}

func TestGutcheckExecutionWithFakeAsker(t *testing.T) {
	dir := t.TempDir()
	evidenceFile := filepath.Join(dir, "evidence.txt")
	_ = os.WriteFile(evidenceFile, []byte("observed facts in evidence file"), 0o600)

	confBoolean := 0.95
	confChoice := 0.60
	confScore := 0.85

	cannedAnswers := map[string]model.Result{
		"q_bool": {
			Probabilities: map[string]float64{"true": 0.95, "false": 0.05},
			Answer:        true,
			Confidence:    confBoolean,
		},
		"q_choice": {
			Probabilities: map[string]float64{"approach_a": 0.60, "approach_b": 0.30, "unclear": 0.10},
			Answer:        "approach_a",
			Confidence:    confChoice,
		},
		"q_score": {
			Probabilities: map[string]float64{"0": 0.05, "1": 0.10, "2": 0.85},
			Answer:        float64(2), // ordinal index 2
			Confidence:    confScore,
		},
		"q_offset_score": {
			Probabilities: map[string]float64{"0": 0.10, "1": 0.90},
			Answer:        float64(1), // ordinal index 1 (second anchor)
			Confidence:    0.0,        // test zero confidence preservation
		},
	}

	fake := &fakeAsker{answers: cannedAnswers}
	tool := NewGutcheckTool(fake)

	inputPayload := map[string]any{
		"state": map[string]any{
			"task": "evaluate migration safety",
			"step": "pre-check",
		},
		"files": []string{"evidence.txt"},
		"questions": []any{
			map[string]any{
				"id":       "q_bool",
				"type":     "boolean",
				"question": "Does the evidence indicate non-breaking changes?",
			},
			map[string]any{
				"id":       "q_choice",
				"type":     "choice",
				"question": "Which architecture pattern fits the constraints?",
				"options":  []string{"approach_a", "approach_b"},
			},
			map[string]any{
				"id":       "q_score",
				"type":     "score",
				"question": "Rate the test coverage adequacy",
				"rubric": map[string]any{
					"min":     0,
					"max":     2,
					"anchors": []string{"insufficient", "acceptable", "comprehensive"},
				},
			},
			map[string]any{
				"id":       "q_offset_score",
				"type":     "score",
				"question": "Rate readiness on 1-based scale",
				"rubric": map[string]any{
					"min":     1,
					"max":     2,
					"anchors": []string{"ready", "production"},
				},
			},
		},
	}

	raw, _ := json.Marshal(inputPayload)
	out, err := tool.Run(context.Background(), dir, raw)
	if err != nil {
		t.Fatalf("unexpected error from Run: %v", err)
	}

	// Verify advisory warning is present
	if !strings.Contains(out, AdvisoryWarning) {
		t.Fatalf("output missing advisory warning: %s", out)
	}

	// Verify counts: 3 decisive (q_bool 0.95, q_score 0.85, q_offset_score 0.90), 1 uncertain (q_choice 0.60)
	if !strings.Contains(out, "3 decisive, 1 uncertain") {
		t.Fatalf("expected '3 decisive, 1 uncertain' in output summary, got: %s", out)
	}
	if !strings.Contains(out, "Warning: 1 uncertain question(s)") {
		t.Fatalf("expected uncertainty warning in output, got: %s", out)
	}

	// Verify parsing JSON entries from output
	jsonIdx := strings.Index(out, "[")
	if jsonIdx < 0 {
		t.Fatalf("no JSON array in output: %s", out)
	}
	var entries []QuestionResult
	if err := json.Unmarshal([]byte(out[jsonIdx:]), &entries); err != nil {
		t.Fatalf("failed to unmarshal JSON entries: %v\nOutput: %s", err, out)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Check ordinal score translation with offset on q_offset_score (min 1, ordinal 1 => translated answer 2.0)
	entryOffset := entries[3]
	if entryOffset.ID != "q_offset_score" {
		t.Fatalf("expected 4th entry to be q_offset_score, got %s", entryOffset.ID)
	}
	ansOffsetF, ok := entryOffset.Answer.(float64)
	if !ok || ansOffsetF != 2.0 {
		t.Fatalf("expected translated score 2.0 (ordinal 1 + min 1), got %v", entryOffset.Answer)
	}
	// Check zero confidence is preserved (not nil)
	if entryOffset.Confidence == nil || *entryOffset.Confidence != 0.0 {
		t.Fatalf("expected preserved zero confidence, got %v", entryOffset.Confidence)
	}
}

func TestGutcheckAnswerValidation(t *testing.T) {
	dir := t.TempDir()

	// Missing answer
	missingFake := &fakeAsker{
		answers: map[string]model.Result{},
	}
	tool := NewGutcheckTool(missingFake)
	payload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "boolean", "question": "test question"},
		},
	})
	_, err := tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "missing or malformed answer") {
		t.Fatalf("expected missing answer error, got: %v", err)
	}

	// Nil answer field
	nilFake := &fakeAsker{
		answers: map[string]model.Result{
			"q1": {Answer: nil, Probabilities: map[string]float64{"true": 1.0}},
		},
	}
	tool = NewGutcheckTool(nilFake)
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "missing or malformed answer") {
		t.Fatalf("expected malformed answer error for nil answer, got: %v", err)
	}

	// Empty probabilities
	emptyProbFake := &fakeAsker{
		answers: map[string]model.Result{
			"q1": {Answer: true, Probabilities: map[string]float64{}},
		},
	}
	tool = NewGutcheckTool(emptyProbFake)
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "missing or malformed answer") {
		t.Fatalf("expected malformed answer error for empty probabilities, got: %v", err)
	}

	// Non-finite probabilities (NaN)
	nanFake := &fakeAsker{
		answers: map[string]model.Result{
			"q1": {Answer: true, Probabilities: map[string]float64{"true": math.NaN(), "false": 0.0}},
		},
	}
	tool = NewGutcheckTool(nanFake)
	_, err = tool.Run(context.Background(), dir, payload)
	if err == nil || !strings.Contains(err.Error(), "non-finite probability") {
		t.Fatalf("expected non-finite probability error, got: %v", err)
	}
}

func TestGutcheckContextTimeoutAndCancellation(t *testing.T) {
	fake := &fakeAsker{
		answers: map[string]model.Result{},
	}
	tool := NewGutcheckTool(fake)

	// Pre-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	payload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "boolean", "question": "is ok"},
		},
	})

	_, err := tool.Run(ctx, t.TempDir(), payload)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}
}

func TestGutcheckToolTimeoutOption(t *testing.T) {
	slowAsker := &fakeAsker{
		err: context.DeadlineExceeded,
	}
	tool := &GutcheckTool{
		Asker:   slowAsker,
		Timeout: 50 * time.Millisecond,
	}

	payload, _ := json.Marshal(map[string]any{
		"state": "test",
		"questions": []any{
			map[string]any{"id": "q1", "type": "boolean", "question": "is ok"},
		},
	})

	_, err := tool.Run(context.Background(), t.TempDir(), payload)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}
