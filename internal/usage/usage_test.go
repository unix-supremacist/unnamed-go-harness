package usage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFetchAndRender(t *testing.T) {
	reset := time.Now().Add(26*time.Hour + 5*time.Minute).UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/go/usage":
			if r.Header.Get("Authorization") != "Bearer gokey" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"usage":{"weekly":{"percent":12.5,"resetsAt":"` + reset.Format(time.RFC3339) + `"},"rolling":{"percent":40,"status":"ok"}}}`))
		case "/codex":
			if r.Header.Get("ChatGPT-Account-Id") != "acct" {
				w.WriteHeader(403)
				return
			}
			w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":91,"limit_window_seconds":18000,"reset_at":` +
				itoa(reset.Unix()) + `},"secondary_window":{"used_percent":30,"limit_window_seconds":604800}}}`))
		case "/or/auth/key":
			w.Write([]byte(`{"data":{"usage":3.5,"limit":10,"is_free_tier":false,"rate_limit":{"requests":-1,"interval":"10s"}}}`))
		case "/or/credits":
			w.Write([]byte(`{"data":{"total_credits":20,"total_usage":7.25}}`))
		}
	}))
	defer srv.Close()
	src := Sources{
		Targets: []Target{
			{Provider: "opencode-go", Kind: "opencode", BaseURL: srv.URL + "/go", Key: key("gokey")},
			{Provider: "openrouter", Kind: "openrouter", BaseURL: srv.URL + "/or", Key: key("orkey")},
		},
		CodexTokens: func(context.Context) (string, string, error) { return "tok", "acct", nil },
		CodexURL:    srv.URL + "/codex",
	}
	secs, errs := Fetch(context.Background(), src)
	if len(errs) != 0 || len(secs) != 3 {
		t.Fatalf("secs=%d errs=%v", len(secs), errs)
	}
	out := Render(secs, errs, time.Now(), nil)
	for _, want := range []string{
		"opencode-go usage\n  Rolling",
		"40% used",
		"Weekly",
		"12.5% used",
		"resets ",
		"(1d 2h)",
		"ChatGPT Codex usage (Pro)",
		"5h ",
		"91% used",
		"7d ",
		"openrouter usage (Paid tier · no rate limit · balance $12.75)",
		"$3.50 of $10.00 used",
		"35% used",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Rolling sorts before Weekly.
	if strings.Index(out, "Rolling") > strings.Index(out, "Weekly") {
		t.Errorf("window order:\n%s", out)
	}
}

func TestFetchReportsMissingAndRejectedCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	src := Sources{
		Targets: []Target{
			{Provider: "opencode-go", Kind: "opencode", BaseURL: srv.URL, Key: key("bad")},
			{Provider: "openrouter", Kind: "openrouter", BaseURL: srv.URL, Key: func() (string, error) { return "", errors.New("none") }},
		},
		CodexTokens: func(context.Context) (string, string, error) { return "", "", errors.New("no login") },
	}
	secs, errs := Fetch(context.Background(), src)
	got := strings.Join(errs, "\n")
	if len(secs) != 0 || !strings.Contains(got, "OpenCode Go: credential was rejected") ||
		!strings.Contains(got, "Codex: no ChatGPT login") || strings.Contains(got, "OpenRouter") {
		t.Fatalf("secs=%v errs=%q", secs, got)
	}
}

func TestOpenRouterWithoutLimitHasNoBar(t *testing.T) {
	out := Render([]Section{{Title: "OpenRouter usage", Windows: []Window{{Label: "$13.58 used", NoBar: true}}}}, nil, time.Now(), nil)
	if strings.Contains(out, "% used") || !strings.Contains(out, "$13.58 used (no limit on this key)") {
		t.Errorf("out = %q", out)
	}
}

func TestUntilAndDateish(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for d, want := range map[time.Duration]string{-time.Minute: "now", 12 * time.Minute: "12m", 4*time.Hour + 10*time.Minute: "4h 10m", 50 * time.Hour: "2d 2h"} {
		if got := Until(now.Add(d), now); got != want {
			t.Errorf("Until(%s) = %q, want %q", d, got, want)
		}
	}
	if dateish(float64(1_700_000_000)).Unix() != 1_700_000_000 || dateish(float64(1_700_000_000_000)).Unix() != 1_700_000_000 {
		t.Error("unix seconds/millis")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func key(k string) func() (string, error) { return func() (string, error) { return k, nil } }

func withLedger(t *testing.T) {
	t.Helper()
	old := ledgerPath
	dir := t.TempDir()
	ledgerPath = func() string { return dir + "/cost.jsonl" }
	t.Cleanup(func() { ledgerPath = old })
}

func TestTwoOpenRouterKindsGetDistinctSections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/key") {
			w.Write([]byte(`{"data":{"usage":1}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	secs, errs := Fetch(context.Background(), Sources{Targets: []Target{
		{Provider: "openrouter", Kind: "openrouter", BaseURL: srv.URL + "/1", Key: key("a")},
		{Provider: "mine", Kind: "openrouter", BaseURL: srv.URL + "/2", Key: key("b")},
		{Provider: "dup", Kind: "openrouter", BaseURL: srv.URL + "/2", Key: key("b")},
	}})
	if len(secs) != 2 || secs[0].Provider != "openrouter" || secs[1].Provider != "mine" || secs[1].Title != "mine usage" {
		t.Fatalf("secs=%+v errs=%v", secs, errs)
	}
}

func TestNoneKindNoteAndMissingCredentialSkipped(t *testing.T) {
	secs, errs := Fetch(context.Background(), Sources{Targets: []Target{
		{Provider: "opencode-zen", Kind: "none", Key: key("k")},
		{Provider: "openrouter", Kind: "openrouter", Key: func() (string, error) { return "", errors.New("none") }},
	}})
	if len(secs) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "opencode-zen: usage not exposed") {
		t.Fatalf("secs=%v errs=%v", secs, errs)
	}
}

func TestUghSpend(t *testing.T) {
	withLedger(t)
	src := Sources{Targets: []Target{{Provider: "opencode-openrouter", Kind: "ugh-spend"}}}
	secs, errs := Fetch(context.Background(), src)
	if len(errs) != 0 || len(secs) != 1 || secs[0].Windows[0].Label != "$0.0000 spent over 0 calls" {
		t.Fatalf("empty: secs=%+v errs=%v", secs, errs)
	}
	Record("opencode-openrouter", 0.25)
	Record("opencode-openrouter", 0.5)
	secs, _ = Fetch(context.Background(), src)
	if secs[0].Provider != "opencode-openrouter" || !strings.Contains(secs[0].Subtitle, "not account-wide") ||
		secs[0].Windows[0].Label != "$0.7500 spent over 2 calls" || !secs[0].Windows[0].NoBar {
		t.Fatalf("secs=%+v", secs)
	}
}

func TestLedgerSumsPerProvider(t *testing.T) {
	withLedger(t)
	Record("a", 1.5)
	Record("b", 2)
	Record("a", 0.5)
	Record("a", 0) // ignored
	if tot, n := Spent("a"); tot != 2 || n != 2 {
		t.Fatalf("a = %v/%d", tot, n)
	}
}
