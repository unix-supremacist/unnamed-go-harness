package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
)

func TestClassifyFailure(t *testing.T) {
	http := func(code int, body string) error { return &llm.HTTPError{Provider: "p", StatusCode: code, Body: body} }
	for _, tc := range []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil", nil, FailNone},
		{"cancel", fmt.Errorf("wrap: %w", context.Canceled), FailCancelled},
		{"permission", perm.ErrStopped, FailPermission},
		{"capacity", fmt.Errorf("x: %w", admission.ErrCapacityExceeded), FailCapacity},
		{"429", http(429, "slow down"), FailTransient},
		{"503", http(503, "down"), FailTransient},
		{"401", http(401, "bad key"), FailAuth},
		{"403", http(403, "no"), FailAuth},
		{"402", http(402, "pay"), FailQuota},
		{"context", http(400, `{"error":{"code":"context_length_exceeded"}}`), FailContext},
		{"unknown field", http(400, "unknown field reasoning"), FailModel},
		{"404", http(404, "no such model"), FailModel},
		{"stall", errors.New("stream stalled"), FailTransient},
		{"task", errors.New("poor answer"), FailOther},
		{"codex quota", errors.New(`codex: HTTP 429: {"type":"usage_limit_reached","resets_in_seconds":600}`), FailQuota},
	} {
		if got := ClassifyFailure(tc.err); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	for _, c := range []FailureClass{FailContext, FailPermission, FailCancelled, FailOther} {
		if c.FailsOver() || c.Retryable() {
			t.Errorf("%s must neither retry nor fail over", c)
		}
	}
	if transient(http(401, "x")) || !transient(http(500, "x")) {
		t.Error("transient() must follow the class")
	}
}
