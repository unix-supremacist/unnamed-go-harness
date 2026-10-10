// Package usage reports subscription and credit usage per provider (a port
// of pisup's /usage): OpenCode Go usage windows, ChatGPT Codex 5h/weekly
// windows, and OpenRouter key usage plus credit balance.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Window is one usage window, e.g. "Rolling 37% used, resets at …".
type Window struct {
	Label    string
	Percent  float64
	ResetsAt time.Time
	Status   string
	NoBar    bool // an amount without a limit: no percentage to draw
	Plain    bool // with NoBar: print the label alone, no "no limit" note
}

// Section is one provider's usage.
type Section struct {
	Provider        string // ugh provider name: opencode-go | codex | openrouter
	Title, Subtitle string
	Windows         []Window
}

// Target is one provider to report on. Kind picks the fetcher.
type Target struct {
	Provider string // ugh provider name; becomes Section.Provider
	Kind     string // opencode | openrouter | ugh-spend | none
	BaseURL  string
	Key      func() (string, error)
	Headers  map[string]string // extra request headers (e.g. x-opencode-org-id)
}

// Sources supplies credentials and endpoints; tests point URLs at fakes.
type Sources struct {
	HTTP        *http.Client
	Targets     []Target
	CodexTokens func(context.Context) (access, account string, err error)

	// CodexURL overrides the ChatGPT endpoint.
	CodexURL string
}

func (s *Sources) defaults() {
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if s.CodexURL == "" {
		s.CodexURL = "https://chatgpt.com/backend-api/wham/usage"
	}
}

// Fetch queries every provider concurrently. Providers with a failing
// endpoint come back as errors (one line each), providers without usage as a
// note, the rest as sections, in a fixed order (targets, then Codex).
func Fetch(ctx context.Context, src Sources) ([]Section, []string) {
	src.defaults()
	type job func(context.Context, Sources) (Section, error)
	var jobs []job
	var notes []string
	for _, t := range src.Targets {
		if t.Kind == "none" && t.Key != nil {
			if k, err := t.Key(); err == nil && k != "" {
				notes = append(notes, t.Provider+": usage not exposed by the provider (see API-reported cost)")
			}
		}
	}
	for _, t := range dedupe(src.Targets) {
		t := t
		switch t.Kind {
		case "opencode":
			jobs = append(jobs, func(ctx context.Context, s Sources) (Section, error) { return openCode(ctx, s, t) })
		case "openrouter":
			jobs = append(jobs, func(ctx context.Context, s Sources) (Section, error) { return openRouter(ctx, s, t) })
		case "ugh-spend":
			jobs = append(jobs, func(ctx context.Context, s Sources) (Section, error) { return localSpend(ctx, s, t) })
		}
	}
	if src.CodexTokens != nil {
		jobs = append(jobs, codex)
	}
	secs := make([]*Section, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, f := range jobs {
		wg.Add(1)
		go func(i int, f job) {
			defer wg.Done()
			sec, err := f(ctx, src)
			if err != nil {
				errs[i] = err
				return
			}
			secs[i] = &sec
		}(i, f)
	}
	wg.Wait()
	var out []Section
	var bad []string
	for i := range jobs {
		if secs[i] != nil {
			out = append(out, *secs[i])
		}
		if errs[i] != nil && !errors.Is(errs[i], errNoCredential) {
			bad = append(bad, errs[i].Error())
		}
	}
	return out, append(bad, notes...)
}

// errNoCredential marks a provider the user never set up: skipped quietly
// from the report, not an error line.
var errNoCredential = errors.New("no credential")

// dedupe drops targets that would repeat the same request (same kind, base
// and headers), keeping the first.
func dedupe(ts []Target) []Target {
	seen := map[string]bool{}
	var out []Target
	for _, t := range ts {
		if t.Kind == "" || t.Kind == "none" {
			continue
		}
		hk := make([]string, 0, len(t.Headers))
		for k, v := range t.Headers {
			hk = append(hk, k+"="+v)
		}
		sort.Strings(hk)
		id := t.Kind + "|" + t.BaseURL + "|" + strings.Join(hk, ",")
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, t)
	}
	return out
}

func title(t Target, fallback string) string {
	if t.Provider == "" {
		return fallback
	}
	return t.Provider + " usage"
}

func getJSON(ctx context.Context, src Sources, url string, headers map[string]string) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := src.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "Timeout") {
			return nil, 0, fmt.Errorf("request timed out")
		}
		return nil, 0, fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("unreadable response")
	}
	if arr, ok := body.([]any); ok && len(arr) > 0 {
		body = arr[0]
	}
	m, _ := body.(map[string]any)
	return m, resp.StatusCode, nil
}

func openCode(ctx context.Context, src Sources, t Target) (Section, error) {
	key, err := t.Key()
	if err != nil || key == "" {
		return Section{}, errNoCredential
	}
	body, code, err := getJSON(ctx, src, strings.TrimRight(t.BaseURL, "/")+"/usage", withAuth(t, key))
	switch {
	case err != nil:
		return Section{}, fmt.Errorf("OpenCode Go: %v", err)
	case code == 401:
		return Section{}, fmt.Errorf("OpenCode Go: credential was rejected")
	case code == 403:
		return Section{}, fmt.Errorf("OpenCode Go: account does not have Go access")
	case code != 200:
		return Section{}, fmt.Errorf("OpenCode Go: usage service returned HTTP %d", code)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage == nil {
		return Section{}, fmt.Errorf("OpenCode Go: unrecognized usage response")
	}
	var ws []Window
	for name, raw := range usage {
		w, _ := raw.(map[string]any)
		pct, ok := num(w["percent"])
		if !ok {
			continue
		}
		st, _ := w["status"].(string)
		ws = append(ws, Window{Label: capitalize(name), Percent: pct, ResetsAt: dateish(w["resetsAt"]), Status: st})
	}
	order := map[string]int{"Rolling": 0, "Weekly": 1, "Monthly": 2}
	sort.Slice(ws, func(i, j int) bool { return rank(order, ws[i].Label) < rank(order, ws[j].Label) })
	if len(ws) == 0 {
		return Section{}, fmt.Errorf("OpenCode Go: no usage windows returned")
	}
	return Section{Provider: t.Provider, Title: title(t, "OpenCode Go usage"), Windows: ws}, nil
}

func codex(ctx context.Context, src Sources) (Section, error) {
	if src.CodexTokens == nil {
		return Section{}, fmt.Errorf("Codex: no credential")
	}
	access, account, err := src.CodexTokens(ctx)
	if err != nil || access == "" {
		return Section{}, fmt.Errorf("Codex: no ChatGPT login (ugh login codex)")
	}
	if account == "" {
		return Section{}, fmt.Errorf("Codex: account id missing from the login")
	}
	body, code, err := getJSON(ctx, src, src.CodexURL, map[string]string{
		"Authorization": "Bearer " + access, "ChatGPT-Account-Id": account,
		"Origin": "https://chatgpt.com", "Referer": "https://chatgpt.com/", "User-Agent": "Mozilla/5.0",
	})
	switch {
	case err != nil:
		return Section{}, fmt.Errorf("Codex: %v", err)
	case code == 401 || code == 403:
		return Section{}, fmt.Errorf("Codex: ChatGPT login was rejected")
	case code != 200:
		return Section{}, fmt.Errorf("Codex: usage service returned HTTP %d", code)
	}
	rl := firstMap(body, "rate_limit", "rate_limits")
	if rl == nil {
		return Section{}, fmt.Errorf("Codex: unrecognized usage response")
	}
	var ws []Window
	for _, c := range []struct {
		keys     []string
		fallback float64
	}{
		{[]string{"primary_window", "primary", "five_hour_limit", "five_hour"}, 18000},
		{[]string{"secondary_window", "secondary", "weekly_limit", "weekly"}, 604800},
	} {
		w := firstMap(rl, c.keys...)
		if w == nil {
			continue
		}
		secs, ok := num(w["limit_window_seconds"])
		if !ok || secs <= 0 {
			secs = c.fallback
		}
		pct, ok := num(w["used_percent"])
		if !ok {
			if left, ok2 := num(firstOf(w, "percent_left", "remaining_percent")); ok2 {
				pct = 100 - left
			}
		}
		reset := w["reset_at"]
		if reset == nil {
			reset = w["reset_time_ms"]
		}
		ws = append(ws, Window{Label: windowLabel(secs), Percent: pct, ResetsAt: dateish(reset)})
	}
	if len(ws) == 0 {
		return Section{}, fmt.Errorf("Codex: no usage windows returned")
	}
	plan, _ := body["plan_type"].(string)
	return Section{Provider: "codex", Title: "ChatGPT Codex usage", Subtitle: capitalize(plan), Windows: ws}, nil
}

func openRouter(ctx context.Context, src Sources, t Target) (Section, error) {
	key, err := t.Key()
	if err != nil || key == "" {
		return Section{}, errNoCredential
	}
	auth := withAuth(t, key)
	base := strings.TrimRight(t.BaseURL, "/")
	body, code, err := getJSON(ctx, src, base+"/auth/key", auth)
	switch {
	case err != nil:
		return Section{}, fmt.Errorf("OpenRouter: %v", err)
	case code == 401 || code == 403:
		return Section{}, fmt.Errorf("OpenRouter: credential was rejected")
	case code != 200:
		return Section{}, fmt.Errorf("OpenRouter: usage service returned HTTP %d", code)
	}
	data, _ := body["data"].(map[string]any)
	used, ok := num(data["usage"])
	if !ok {
		return Section{}, fmt.Errorf("OpenRouter: unrecognized usage response")
	}
	limit, hasLimit := num(data["limit"])
	if limit < 0 {
		hasLimit = false
	}
	w := Window{Label: fmt.Sprintf("$%.2f used", used), NoBar: true}
	if hasLimit && limit > 0 {
		w.Label = fmt.Sprintf("$%.2f of $%.2f used", used, limit)
		w.Percent, w.NoBar = used/limit*100, false
	}
	sub := "Paid tier"
	if free, _ := data["is_free_tier"].(bool); free {
		sub = "Free tier"
	}
	if rl, _ := data["rate_limit"].(map[string]any); rl != nil {
		req, ok := num(rl["requests"])
		iv, _ := rl["interval"].(string)
		switch {
		case ok && req == -1:
			sub += " · no rate limit"
		case ok && iv != "":
			sub += fmt.Sprintf(" · %d requests/%s", int(req), iv)
		}
	}
	// The balance is best effort: a key without credits access still shows usage.
	if cb, code, err := getJSON(ctx, src, base+"/credits", auth); err == nil && code == 200 {
		cd, _ := cb["data"].(map[string]any)
		total, ok1 := num(cd["total_credits"])
		spent, ok2 := num(cd["total_usage"])
		if ok1 && ok2 {
			sub += fmt.Sprintf(" · balance $%.2f", math.Max(0, total-spent))
		}
	}
	return Section{Provider: t.Provider, Title: title(t, "OpenRouter usage"), Subtitle: sub, Windows: []Window{w}}, nil
}

// ---- rendering ----

// Render formats sections like pisup: a title, then one bar per window with
// its reset time. color(text, percent) may style the bar (nil = plain).
func Render(secs []Section, errs []string, now time.Time, color func(string, float64) string) string {
	width := 0
	for _, s := range secs {
		for _, w := range s.Windows {
			width = max(width, len([]rune(w.Label)))
		}
	}
	var b strings.Builder
	for i, s := range secs {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s.Title)
		if s.Subtitle != "" {
			b.WriteString(" (" + s.Subtitle + ")")
		}
		b.WriteString("\n")
		for _, w := range s.Windows {
			if w.NoBar && w.Plain {
				fmt.Fprintf(&b, "  %s\n", w.Label)
				continue
			}
			if w.NoBar {
				fmt.Fprintf(&b, "  %s (no limit on this key)\n", w.Label)
				continue
			}
			p := math.Max(0, math.Min(100, w.Percent))
			bar := Bar(p, 20)
			if color != nil {
				bar = color(bar, p)
			}
			pct := strconv.FormatFloat(p, 'f', 1, 64)
			if p == math.Trunc(p) {
				pct = strconv.Itoa(int(p))
			}
			fmt.Fprintf(&b, "  %-*s  %s  %5s%% used", width, w.Label, bar, pct)
			if w.Status != "" && w.Status != "ok" {
				b.WriteString(" · " + w.Status)
			}
			b.WriteString("\n")
			if !w.ResetsAt.IsZero() {
				fmt.Fprintf(&b, "  %-*s  resets %s (%s)\n", width, "", w.ResetsAt.Local().Format("Mon Jan 2 15:04"), Until(w.ResetsAt, now))
			}
		}
	}
	if len(errs) > 0 {
		b.WriteString("\n" + strings.Join(errs, "\n") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Bar draws a filled/empty bar for a percentage.
func Bar(percent float64, width int) string {
	filled := int(math.Round(math.Max(0, math.Min(100, percent)) / 100 * float64(width)))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// Until formats the time left before t as "2d 3h", "4h 10m" or "12m".
func Until(t, now time.Time) string {
	m := int(math.Ceil(t.Sub(now).Minutes()))
	if m <= 0 {
		return "now"
	}
	d, h, mm := m/1440, m%1440/60, m%60
	var parts []string
	if d > 0 {
		parts = append(parts, fmt.Sprintf("%dd", d))
	}
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if d == 0 && mm > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mm))
	}
	return strings.Join(parts, " ")
}

// ---- json helpers ----

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// dateish accepts RFC 3339 strings and unix seconds or milliseconds.
func dateish(v any) time.Time {
	if s, ok := v.(string); ok {
		if _, isNum := num(s); !isNum {
			t, _ := time.Parse(time.RFC3339, s)
			return t
		}
	}
	f, ok := num(v)
	if !ok || f <= 0 {
		return time.Time{}
	}
	if f < 1e10 {
		return time.Unix(int64(f), 0)
	}
	return time.UnixMilli(int64(f))
}

func firstMap(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
	}
	return nil
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func windowLabel(secs float64) string {
	s := int(secs)
	switch {
	case s%86400 == 0:
		return fmt.Sprintf("%dd", s/86400)
	case s%3600 == 0:
		return fmt.Sprintf("%dh", s/3600)
	case s%60 == 0:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%ds", s)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func rank(order map[string]int, label string) int {
	if r, ok := order[label]; ok {
		return r
	}
	return len(order)
}

func withAuth(t Target, key string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + key}
	for k, v := range t.Headers {
		h[k] = v
	}
	return h
}

// localSpend reports the cost ugh itself has recorded for a provider whose
// account usage cannot be queried (a BYOK Console connection: the Console
// holds the upstream key and exposes no usage). It is a floor, not an
// account-wide figure, and is labelled so.
func localSpend(_ context.Context, _ Sources, t Target) (Section, error) {
	total, calls := Spent(t.Provider)
	return Section{Provider: t.Provider, Title: title(t, "Usage"), Subtitle: "spent via ugh, not account-wide",
		Windows: []Window{{Label: fmt.Sprintf("$%.4f spent over %d calls", total, calls), NoBar: true, Plain: true}}}, nil
}
