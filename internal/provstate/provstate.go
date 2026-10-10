// Package provstate records providers that should not be used right now,
// shared by every ugh process through ~/.local/state/ugh/providers.json:
//
//   - exhausted: the provider refused with a usage/quota limit; skip it for
//     every agent and every new spawn until the reset time;
//   - reserved: its usage window passed the configured reserve; new spawns
//     avoid it, agents already on it keep going.
package provstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State is one provider/model restriction.
type State struct {
	Kind       string    `json:"kind"` // exhausted | reserved | rejected | outage
	Until      time.Time `json:"until"`
	Reason     string    `json:"reason"`
	Source     string    `json:"source,omitempty"`      // api_error | quota | reserve | config
	Scope      string    `json:"scope,omitempty"`       // account | model | transport | provider
	AccountID  string    `json:"account_id,omitempty"`  // account identity
	ModelID    string    `json:"model_id,omitempty"`    // model id if scoped to model
	Transport  string    `json:"transport,omitempty"`   // transport if scoped to transport
	RecordedAt time.Time `json:"recorded_at,omitempty"` // evidence freshness timestamp
}

var (
	mu       sync.Mutex
	cache    map[string]State
	cachedAt time.Time
	pathFn   = defaultPath
	now      = time.Now
)

func defaultPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "ugh", "providers.json")
}

func load() map[string]State {
	if cache != nil && now().Sub(cachedAt) < 3*time.Second {
		return cache
	}
	m := map[string]State{}
	if b, err := os.ReadFile(pathFn()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	for p, s := range m {
		if !s.Until.IsZero() && now().After(s.Until) {
			delete(m, p)
		}
	}
	cache, cachedAt = m, now()
	return m
}

// Get returns the provider's active restriction, if any.
func Get(provider string) (State, bool) {
	mu.Lock()
	defer mu.Unlock()
	s, ok := load()[provider]
	if ok && !s.Until.IsZero() && now().After(s.Until) {
		return State{}, false
	}
	return s, ok
}

// CheckRoute returns active restrictions for a specific provider, model, transport, and account.
func CheckRoute(provider, model, transport, accountID string) (State, bool) {
	mu.Lock()
	defer mu.Unlock()
	m := load()
	matches := func(s State) bool {
		return (s.Until.IsZero() || now().Before(s.Until)) &&
			(s.AccountID == "" || s.AccountID == accountID) &&
			(s.ModelID == "" || s.ModelID == model) &&
			(s.Transport == "" || s.Transport == transport)
	}
	// 1. Check account-scoped provider exhaustion.
	if accountID != "" {
		if s, ok := m[provider+"@"+accountID]; ok && matches(s) {
			return s, true
		}
	}
	// 2. Check model-scoped rejection.
	if model != "" {
		if s, ok := m[provider+"/"+model]; ok && matches(s) {
			return s, true
		}
	}
	// 3. Check general provider-wide restriction.
	if s, ok := m[provider]; ok && matches(s) {
		return s, true
	}
	return State{}, false
}

// InvalidateAccount clears cached restrictions for the given accountID.
func InvalidateAccount(accountID string) {
	if accountID == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	cache = nil
	m := load()
	changed := false
	for k, s := range m {
		if s.AccountID == accountID || strings.HasSuffix(k, "@"+accountID) {
			delete(m, k)
			changed = true
		}
	}
	if changed {
		writeLocked(m)
	}
}

// MarkExhausted records an account-level provider quota exhaustion.
func MarkExhausted(provider, accountID string, until time.Time, reason, source string) {
	key := provider
	scope := "provider"
	if accountID != "" {
		key = provider + "@" + accountID
		scope = "account"
	}
	Mark(key, State{
		Kind:       "exhausted",
		Until:      until,
		Reason:     reason,
		Source:     source,
		Scope:      scope,
		AccountID:  accountID,
		RecordedAt: now(),
	})
}

// MarkModelRejected records a model rejection (e.g. 404 or protocol unsupported).
func MarkModelRejected(provider, model string, until time.Time, reason, source string) {
	key := provider + "/" + model
	Mark(key, State{
		Kind:       "rejected",
		Until:      until,
		Reason:     reason,
		Source:     source,
		Scope:      "model",
		ModelID:    model,
		RecordedAt: now(),
	})
}

// MarkOutage records a transient provider endpoint outage.
func MarkOutage(provider string, until time.Time, reason, source string) {
	Mark(provider, State{
		Kind:       "outage",
		Until:      until,
		Reason:     reason,
		Source:     source,
		Scope:      "provider",
		RecordedAt: now(),
	})
}

// Usable reports whether new work may start on the provider (neither
// exhausted nor reserved).
func Usable(provider string) bool {
	_, ok := Get(provider)
	return !ok
}

// Exhausted reports whether the provider refuses requests right now.
func Exhausted(provider string) bool {
	s, ok := Get(provider)
	return ok && s.Kind == "exhausted"
}

// Mark records a restriction (an exhausted mark is never downgraded to
// reserved before it expires).
func Mark(provider string, s State) {
	mu.Lock()
	defer mu.Unlock()
	cache = nil
	m := load()
	if old, ok := m[provider]; ok && old.Kind == "exhausted" && s.Kind == "reserved" {
		return
	}
	if s.RecordedAt.IsZero() {
		s.RecordedAt = now()
	}
	m[provider] = s
	writeLocked(m)
}

// Clear removes a reserved mark (usage fell back under the reserve).
func Clear(provider, kind string) {
	mu.Lock()
	defer mu.Unlock()
	cache = nil
	m := load()
	if s, ok := m[provider]; ok && s.Kind == kind {
		delete(m, provider)
		writeLocked(m)
	}
}

func writeLocked(m map[string]State) {
	p := pathFn()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
	cache, cachedAt = m, now()
}

// Summary describes every active restriction in one line, e.g.
// "codex exhausted until 22:47 (usage_limit_reached)".
func Summary() string {
	mu.Lock()
	m := load()
	mu.Unlock()
	var parts []string
	for p, s := range m {
		if !s.Until.IsZero() && now().After(s.Until) {
			continue
		}
		parts = append(parts, p+" "+s.Kind+" until "+s.Until.Local().Format("15:04")+" ("+s.Reason+")")
	}
	sort.Strings(parts)
	return strings.Join(parts, " · ")
}

var resetsIn = regexp.MustCompile(`"?resets_in_seconds"?\s*:\s*(\d+)`)
var resetsAt = regexp.MustCompile(`"?resets?_at"?\s*:\s*"?(\d{9,13})`)

// QuotaError reports whether err is a provider usage/quota refusal and when
// it resets (default: an hour from now when the error does not say).
func QuotaError(err error) (until time.Time, reason string, ok bool) {
	if err == nil {
		return time.Time{}, "", false
	}
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "usage_limit_reached"):
		reason = "usage_limit_reached"
	case strings.Contains(low, "insufficient_quota"), strings.Contains(low, "quota exceeded"), strings.Contains(low, "exceeded your current quota"):
		reason = "quota exceeded"
	case strings.Contains(low, "http 402"), strings.Contains(low, "insufficient credits"):
		reason = "out of credits"
	default:
		return time.Time{}, "", false
	}
	until = now().Add(time.Hour)
	if m := resetsIn.FindStringSubmatch(s); m != nil {
		if n, e := strconv.Atoi(m[1]); e == nil {
			until = now().Add(time.Duration(n) * time.Second)
		}
	} else if m := resetsAt.FindStringSubmatch(s); m != nil {
		if n, e := strconv.ParseInt(m[1], 10, 64); e == nil {
			if n > 1e11 {
				until = time.UnixMilli(n)
			} else {
				until = time.Unix(n, 0)
			}
		}
	}
	return until, reason, true
}

// SetPathForTest points the state file elsewhere.
func SetPathForTest(p string) {
	mu.Lock()
	pathFn = func() string { return p }
	cache = nil
	mu.Unlock()
}
