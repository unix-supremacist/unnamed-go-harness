package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/llm/chat"
)

// TestRecordedStream replays the recorded live stream (captured 2026-10-05
// from chatgpt.com/backend-api/codex/responses while creating hello.go; the
// safety_identifier was redacted). The live endpoint sends SSE without a
// Content-Type header, so the fixture server mimics that.
func TestRecordedStream(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "responses.sse"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var gotAccount, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccount = r.Header.Get("chatgpt-account-id")
		if r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("unexpected request path or auth header")
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		gotBody = string(b)
		w.Header().Set("Content-Type", "") // Disable net/http's text/plain sniffing; live endpoint omits it.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	c := New(func(context.Context) (string, string, error) {
		return "sk-test", "acct-test", nil
	}, srv.Client())
	c.endpoint = srv.URL + "/backend-api/codex/responses" // never dial the live endpoint in tests
	ch, err := c.Stream(context.Background(), llm.Request{
		Model: "gpt-6-sol", System: "sys",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "do it"}},
		Tools:    []llm.ToolSpec{{Name: "write", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	acc := chat.NewCallAccumulator()
	var text strings.Builder
	var native []string
	var usage *llm.Usage
	done := 0
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			text.WriteString(ev.TextDelta)
		case llm.EventToolCall:
			acc.Add(ev.Seq, *ev.Call)
		case llm.EventNative:
			native = append(native, ev.Native)
		case llm.EventUsage:
			usage = ev.Usage
		case llm.EventDone:
			done++
		case llm.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if gotAccount != "acct-test" {
		t.Errorf("chatgpt-account-id header = %q", gotAccount)
	}
	for _, want := range []string{`"store":false`, `"include":["reasoning.encrypted_content"]`, `"strict":null`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %s", want)
		}
	}

	const wantText = "I’ll create `hello.go` to print `hi`, then run it."
	if text.String() != wantText {
		t.Errorf("text = %q", text.String())
	}

	calls := acc.Finish()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_s74S66wguVzcEfEx21Jk6Sos" || calls[0].Name != "write" {
		t.Errorf("call = %+v", calls[0])
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Args), &args); err != nil || args["path"] != "hello.go" {
		t.Errorf("call args = %q (%v)", calls[0].Args, err)
	}

	if usage == nil || usage.In != 107 || usage.Out != 69 {
		t.Errorf("usage = %+v", usage)
	}
	if done != 1 {
		t.Errorf("done events = %d", done)
	}

	// The native replay payload holds the completed items in stream order:
	// the assistant message, then the function call (its output is appended
	// separately as a function_call_output on the next turn).
	var items []json.RawMessage
	if err := json.Unmarshal([]byte("["+strings.Join(native, ",")+"]"), &items); err != nil {
		t.Fatalf("native payload: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("native items = %d, want 2", len(items))
	}
	for i, want := range []string{`"type":"message"`, `"type":"function_call"`} {
		if !strings.Contains(string(items[i]), want) {
			t.Errorf("native item %d = %.120s, want %s", i, items[i], want)
		}
	}
}

// TestEncryptedReasoningReplay keeps encrypted reasoning and the completed
// function call in their original order, then appends the tool output.
func TestEncryptedReasoningReplay(t *testing.T) {
	reason := `{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}`
	message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"running"}]}`
	call := `{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"echo hi\"}"}`
	var native []string
	for i, item := range []string{reason, message, call} {
		for _, ev := range itemDone(i, json.RawMessage(item)) {
			if ev.Type == llm.EventNative {
				native = append(native, ev.Native)
			}
		}
	}
	if len(native) != 3 || len(itemDone(0, json.RawMessage(`{"type":"reasoning","id":"rs_2"}`))) != 0 {
		t.Fatalf("encrypted replay items = %d, want 3; plaintext reasoning must not replay", len(native))
	}
	body, err := buildRequest(llm.Request{Model: "gpt-6-sol", Messages: []llm.Message{
		{Role: llm.RoleAssistant, Text: "running", Native: "[" + strings.Join(native, ",") + "]"},
		{Role: llm.RoleTool, ToolCallID: "c1", Text: "hi"},
	}}, "session")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Store bool              `json:"store"`
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Store || len(out.Input) != 4 {
		t.Fatalf("store=%t input=%s", out.Store, out.Input)
	}
	for i, want := range []string{reason, message, call} {
		if string(out.Input[i]) != want {
			t.Errorf("replayed item %d = %s, want %s", i, out.Input[i], want)
		}
	}
	if !strings.Contains(string(out.Input[3]), `"type":"function_call_output"`) ||
		!strings.Contains(string(out.Input[3]), `"call_id":"c1"`) {
		t.Errorf("tool output = %s", out.Input[3])
	}
}
