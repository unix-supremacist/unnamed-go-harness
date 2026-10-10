package chat

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
	"github.com/unix/ugh/internal/usage"
)

// serveFixture runs a recorded SSE fixture (recorded from the live OpenCode Go
// endpoint) through a test server. The fixtures live in testdata/*.sse.
func serveFixture(t *testing.T, name string, hdr http.Header) *Client {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "sk-test", srv.Client())
	c.Session = "11111111-2222-4333-8444-555555555555"
	return c
}

// collect drains a stream into slices for assertions.
type collected struct {
	text     strings.Builder
	thinking strings.Builder
	stop     string
	usage    *llm.Usage
	calls    []llm.ToolCall
}

func (c *collected) run(t *testing.T, ch <-chan llm.Event) {
	t.Helper()
	acc := NewCallAccumulator()
	for ev := range ch {
		switch ev.Type {
		case llm.EventTextDelta:
			c.text.WriteString(ev.TextDelta)
		case llm.EventThinkingDelta:
			c.thinking.WriteString(ev.Thinking)
		case llm.EventToolCall:
			acc.Add(ev.Seq, *ev.Call)
		case llm.EventUsage:
			c.usage = ev.Usage
		case llm.EventDone:
			if c.stop == "" {
				c.stop = ev.Stop
			}
		case llm.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	c.calls = acc.Finish()
}

// TestTextStream replays the recorded text fixture: reasoning fragments, a
// content delta, finish_reason stop and a usage-only frame.
func TestTextStream(t *testing.T) {
	c := serveFixture(t, "text.sse", nil)
	ch, err := c.Stream(context.Background(), llm.Request{Model: "mimo-v2.6-pro",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "Reply with exactly the word: pong"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got collected
	got.run(t, ch)
	if got.text.String() != "pong" {
		t.Errorf("text = %q, want pong", got.text.String())
	}
	if want := `The user wants me to reply with exactly the word "pong".`; got.thinking.String() != want {
		t.Errorf("thinking = %q, want %q", got.thinking.String(), want)
	}
	if got.stop != "stop" {
		t.Errorf("stop = %q, want stop", got.stop)
	}
	if got.usage == nil || got.usage.In != 14 || got.usage.Out != 17 || got.usage.Cached != 0 {
		t.Errorf("usage = %+v, want {14 17 0}", got.usage)
	}
	if len(got.calls) != 0 {
		t.Errorf("calls = %+v, want none", got.calls)
	}
}

// TestToolCallStream replays the recorded tool-call fixture: the first frame
// carries id+name, later frames append argument fragments.
func TestToolCallStream(t *testing.T) {
	c := serveFixture(t, "toolcall.sse", nil)
	ch, err := c.Stream(context.Background(), llm.Request{Model: "mimo-v2.6-pro"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got collected
	got.run(t, ch)
	if len(got.calls) != 1 {
		t.Fatalf("calls = %+v, want 1", got.calls)
	}
	call := got.calls[0]
	if call.ID != "call_a55cbb76a9e34ce7bc0696f9" || call.Name != "bash" {
		t.Errorf("call = %+v", call)
	}
	if call.Args != `{"command": "echo hi"}` {
		t.Errorf("args = %q", call.Args)
	}
	if got.stop != "tool_calls" {
		t.Errorf("stop = %q, want tool_calls", got.stop)
	}
	if got.usage == nil || got.usage.In != 92 || got.usage.Out != 31 {
		t.Errorf("usage = %+v", got.usage)
	}
}

// TestRequestShape checks the serialized body and headers against the live
// requirements: bearer auth, SSE accept, required session header, and
// function-style tools.
func TestRequestShape(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join("testdata", "text.sse"))
	var (
		req  *http.Request
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(raw)
	}))
	defer srv.Close()
	c := New(srv.URL, "sk-test", srv.Client())
	c.Session = "session-uuid"
	ch, err := c.Stream(context.Background(), llm.Request{
		Model: "mimo-v2.6-pro", System: "be terse", Thinking: "low",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "hi"},
			{Role: llm.RoleAssistant, Text: "ok", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "bash", Args: `{"command":"ls"}`}}},
			{Role: llm.RoleTool, ToolCallID: "c1", ToolName: "bash", Text: "out"},
		},
		Tools: []llm.ToolSpec{{Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	if req.URL.Path != "/chat/completions" {
		t.Errorf("path = %q", req.URL.Path)
	}
	if req.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("auth = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("x-opencode-session") != "session-uuid" {
		t.Errorf("session header = %q", req.Header.Get("x-opencode-session"))
	}
	var out struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []any  `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if !out.Stream || out.Model != "mimo-v2.6-pro" {
		t.Errorf("body head = %+v", out)
	}
	if len(out.Messages) != 4 || out.Messages[0].Role != "system" || out.Messages[3].ToolCallID != "c1" {
		t.Errorf("messages = %+v", out.Messages)
	}
	if len(out.Messages[2].ToolCalls) != 1 {
		t.Errorf("assistant tool_calls = %+v", out.Messages[2])
	}
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "bash" {
		t.Errorf("tools = %+v", out.Tools)
	}
	if out.Reasoning.Effort != "low" {
		t.Errorf("reasoning = %+v", out.Reasoning)
	}
}

// TestHTTPError ensures non-200 responses surface as errors without leaking
// the body of a healthy stream.
func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()
	c := New(srv.URL, "sk-test", srv.Client())
	_, err := c.Stream(context.Background(), llm.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
}

// TestNonSSE rejects a 200 JSON body, which is how OpenCode Go reports a
// missing session header.
func TestNonSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"error","error":{"type":"MissingSessionID"}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "sk-test", srv.Client())
	_, err := c.Stream(context.Background(), llm.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "SSE") {
		t.Fatalf("err = %v, want SSE content-type error", err)
	}
}

// TestBadFrameIsError surfaces a malformed data frame as an error event.
func TestBadFrameIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {not json}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sawErr := false
	for ev := range ch {
		if ev.Type == llm.EventError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected an error event for a malformed frame")
	}
}

// TestCallAccumulator handles interleaved indices and stream order.
func TestCallAccumulator(t *testing.T) {
	acc := NewCallAccumulator()
	acc.Add(1, llm.ToolCall{ID: "b", Name: "read", Args: `{"p`})
	acc.Add(0, llm.ToolCall{ID: "a", Name: "bash"})
	acc.Add(1, llm.ToolCall{Args: `ath":"x"}`})
	got := acc.Finish()
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("calls = %+v", got)
	}
	if got[1].Args != `{"path":"x"}` {
		t.Errorf("args = %q", got[1].Args)
	}
	if got[0].Args != "" {
		t.Errorf("empty args = %q", got[0].Args)
	}
}

// TestThinkingParamUnsupported drops the `reasoning` request field for
// provider/model pairs whose upstream rejects it (PLAN §3: unsupported
// thinking params are dropped, not errors), and keeps it elsewhere.
func TestThinkingParamUnsupported(t *testing.T) {
	if !thinkingParamOK("opencode-go", "mimo-v2.6-flash") || !thinkingParamOK("openrouter", "z-ai/glm-5.3-flash") {
		t.Fatal("supported combos must keep the thinking param")
	}
	if thinkingParamOK("opencode-go", "glm-5.3-flash") || thinkingParamOK("opencode-go", "glm-5.3") {
		t.Fatal("glm on opencode-go must drop the thinking param")
	}
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL, "sk-test", srv.Client())
	c.Prov = "opencode-go"
	ch, err := c.Stream(context.Background(), llm.Request{Model: "glm-5.3-flash", Thinking: "low"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	if _, ok := got["reasoning"]; ok {
		t.Fatalf("glm on opencode-go must not receive reasoning: %v", got)
	}
	c.Prov = "openrouter"
	ch, err = c.Stream(context.Background(), llm.Request{Model: "z-ai/glm-5.3-flash", Thinking: "low"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	r, ok := got["reasoning"].(map[string]any)
	if !ok || r["effort"] != "low" {
		t.Fatalf("openrouter glm must keep reasoning: %v", got)
	}
}

// TestReportedCost: usage.cost reaches the event and the per-provider ledger.
func TestReportedCost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := serveFixture(t, "cost.sse", nil)
	c.Prov = "opencode-openrouter"
	ch, err := c.Stream(context.Background(), llm.Request{Model: "m",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "ping"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got collected
	got.run(t, ch)
	if got.usage == nil || got.usage.Cost != 0.00123 {
		t.Fatalf("usage = %+v", got.usage)
	}
	if tot, n := usage.Spent("opencode-openrouter"); n != 1 || tot != 0.00123 {
		t.Errorf("ledger = %v/%d", tot, n)
	}
}
