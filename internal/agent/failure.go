package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/provstate"
)

// FailureClass types a failed request attempt so retry and failover decisions
// do not depend on scattered substring checks.
type FailureClass int

const (
	FailNone       FailureClass = iota
	FailTransient               // transport stall/reset, 429 rate limit, 5xx: retry, then fail over
	FailQuota                   // account/usage exhaustion: fail over immediately, mark the provider
	FailAuth                    // 401/403 or credential problems: fail over, never retry
	FailModel                   // this route rejected the request (404, unknown parameter): fail over
	FailCapacity                // local premium admission refused: fail over to the next route
	FailContext                 // request does not fit the context window: not an outage
	FailPermission              // permission denial/stop: never switch providers
	FailCancelled               // cancellation: stop
	FailOther                   // task, tool or unclassified failures: not a provider outage
)

func (c FailureClass) String() string {
	return [...]string{"none", "transient", "quota", "auth", "model-rejected", "capacity", "context", "permission", "cancelled", "other"}[c]
}

// Retryable reports whether the same route is worth one more attempt.
func (c FailureClass) Retryable() bool { return c == FailTransient }

// FailsOver reports whether the next route in the chain should be tried.
func (c FailureClass) FailsOver() bool {
	switch c {
	case FailTransient, FailQuota, FailAuth, FailModel, FailCapacity:
		return true
	}
	return false
}

// ClassifyFailure types err. Typed errors (llm.HTTPError, admission, perm,
// context) win; message matching only covers stream-level errors that carry
// no type.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return FailNone
	}
	switch {
	case errors.Is(err, context.Canceled):
		return FailCancelled
	case errors.Is(err, perm.ErrStopped):
		return FailPermission
	case errors.Is(err, admission.ErrCapacityExceeded):
		return FailCapacity
	}
	if _, _, quota := provstate.QuotaError(err); quota {
		return FailQuota
	}
	s := strings.ToLower(err.Error())
	var he *llm.HTTPError
	if errors.As(err, &he) {
		body := strings.ToLower(he.Body)
		switch {
		case he.StatusCode == 401 || he.StatusCode == 403:
			return FailAuth
		case he.StatusCode == 408 || he.StatusCode == 429 || he.StatusCode >= 500:
			return FailTransient
		case isContextFit(body):
			return FailContext
		case he.StatusCode == 402:
			return FailQuota
		case he.StatusCode >= 400:
			return FailModel
		}
	}
	switch {
	case isContextFit(s):
		return FailContext
	case containsAny(s, "stalled", "stream read", "unexpected eof", "connection reset", "broken pipe", "deadline exceeded",
		"timeout", "http 429", "http 500", "http 502", "http 503", "http 504", "rate limit", "overloaded", "connection"):
		return FailTransient
	case containsAny(s, "unauthorized", "credential", "token"):
		return FailAuth
	case containsAny(s, "quota"):
		return FailQuota
	case containsAny(s, "http 4", "status 4", "http ", "status "):
		return FailModel
	}
	return FailOther
}

func isContextFit(s string) bool {
	return containsAny(s, "context_length_exceeded", "maximum context", "context window", "too many tokens", "prompt is too long")
}

func containsAny(s string, subs ...string) bool {
	for _, k := range subs {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
