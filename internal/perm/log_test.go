package perm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultLogPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, want := DefaultLogPath(), "/state/ugh/decisions.jsonl"; got != want {
		t.Errorf("DefaultLogPath = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	t.Setenv("HOME", "/home/u")
	if got, want := DefaultLogPath(), "/home/u/.local/state/ugh/decisions.jsonl"; got != want {
		t.Errorf("DefaultLogPath = %q, want %q", got, want)
	}
}

func TestLogAppendsScrubbedJSONL(t *testing.T) {
	const secret = "sk-verysecretvalue123456"
	RegisterSecret(secret)
	path := filepath.Join(t.TempDir(), "sub", "decisions.jsonl")
	l, err := NewLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Add(logEntry{
		Mode: "classify", Agent: "root", Cwd: "/w", Tool: "bash",
		Detail:   "curl -H \"Authorization: Bearer " + secret + "\" https://x",
		Verdicts: []Verdict{{Classifier: "static_extreme", Flagged: true, Reason: "curl -d @" + secret}},
		Action:   "prompt", Reason: "denied by user", Answer: "n",
	})
	l.Add(logEntry{Mode: "auto", Tool: "read", Detail: "API_KEY=" + secret, Action: "allow"})
	l.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("log perms = %v, want 0600", st.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("secret leaked into the decision log:\n%s", data)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var e logEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if e.TS == "" || e.Mode != "classify" || e.Action != "prompt" || e.Answer != "n" ||
		len(e.Verdicts) != 1 || !e.Verdicts[0].Flagged {
		t.Errorf("entry = %+v", e)
	}
	if !strings.Contains(e.Reason, "denied by user") {
		t.Errorf("reason = %q", e.Reason)
	}
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatalf("line 2 is not JSON: %v", err)
	}
	if e.Mode != "auto" || e.Action != "allow" {
		t.Errorf("entry = %+v", e)
	}
	if l.LogError() != nil {
		t.Errorf("log error: %v", l.LogError())
	}
}

func TestNilLogIsSafe(t *testing.T) {
	var l *DecisionLog
	l.Add(logEntry{Tool: "bash"})
	if l.LogError() != nil {
		t.Fatal("nil log should be inert")
	}
	if l2, err := NewLog(""); err != nil || l2 != nil {
		t.Fatalf("NewLog(\"\") = %v, %v; want nil, nil", l2, err)
	}
}
