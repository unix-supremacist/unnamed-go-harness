// Package chat is the OpenAI-compatible Chat Completions SSE client used by
// both OpenCode Go and OpenRouter (same wire format). Copy-and-trim of the
// request builder and stream parser from old/internal/provider/opencodego;
// the validation layers are replaced by simple, tolerant decoding.
package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/usage"
)

// Client implements llm.Provider for one base-URL + key pair.
type Client struct {
	// Prov is the provider name ("opencode-go", "openrouter"): the thinking
	// param table below keys on it (PLAN §3).
	Prov string
	base string
	key  string
	// Session is sent as x-opencode-session. OpenCode Go requires it
	// (an authenticated request without it returns HTTP 400 MissingSessionID);
	// other Chat Completions endpoints ignore it.
	Session string
	// NoThinking adds model id prefixes (beyond the built-in table) whose
	// upstream rejects `reasoning`; Headers are extra request headers
	// (config-defined providers).
	NoThinking []string
	Headers    map[string]string
	// Flex, when set, tries OpenRouter flex endpoints first (see flex.go).
	Flex *Flex
	http *http.Client
}

// New returns a client for one base URL. The optional http client is used
// directly when given (tests inject fixtures through it).
func New(base, key string, hc *http.Client) *Client {
	if hc == nil {
		hc = NewHTTPClient()
	}
	return &Client{base: strings.TrimRight(base, "/"), key: key, http: hc}
}

// Stream builds the request, posts it, and pumps parsed SSE events into the
// returned channel. The channel is closed when the stream ends; ctx cancels.
func (c *Client) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	if !thinkingParamOK(c.Prov, req.Model) || hasPrefix(c.NoThinking, req.Model) {
		req.Thinking = "" // §3: drop unsupported thinking params, never errors
	}
	if c.Flex.applies(req) {
		return c.streamFlex(ctx, req)
	}
	return c.stream(ctx, req, nil)
}

// stream posts one request; extra fields are merged into the body.
func (c *Client) stream(ctx context.Context, req llm.Request, extra map[string]any) (<-chan llm.Event, error) {
	body, err := buildRequest(req, extra)
	if err != nil {
		return nil, err
	}
	gctx, guard := NewGuard(ctx)
	r, err := http.NewRequestWithContext(gctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chat: build request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+c.key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	for k, v := range c.Headers {
		r.Header.Set(k, v)
	}
	if c.Session != "" {
		r.Header.Set("x-opencode-session", c.Session)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		err = guard.Err(err)
		guard.Stop()
		return nil, fmt.Errorf("chat: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		guard.Stop()
		return nil, &llm.HTTPError{Provider: "chat", StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(ct), "event-stream") {
		resp.Body.Close()
		guard.Stop()
		return nil, fmt.Errorf("chat: expected SSE, got %q", ct)
	}
	resp.Body = guard.Body(resp.Body)
	events := make(chan llm.Event, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		PumpSSE(ctx, resp.Body, events, func(data string) []llm.Event {
			evs := decodeEvent(data)
			for _, ev := range evs {
				if ev.Usage != nil && ev.Usage.Cost > 0 {
					usage.Record(c.Prov, ev.Usage.Cost)
				}
			}
			return evs
		})
	}()
	return events, nil
}

// buildRequest serializes the Chat Completions body. Salvaged structure:
// user/assistant/tool messages, assistant tool_calls, function tools.
func buildRequest(req llm.Request, extra map[string]any) ([]byte, error) {
	messages := make([]any, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			messages = append(messages, map[string]any{"role": "user", "content": m.Text})
		case llm.RoleAssistant:
			v := map[string]any{"role": "assistant", "content": m.Text}
			if m.Reasoning != "" {
				v["reasoning_content"] = m.Reasoning
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]any, 0, len(m.ToolCalls))
				for _, call := range m.ToolCalls {
					args := call.Args
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{
						"id": call.ID, "type": "function",
						"function": map[string]any{"name": call.Name, "arguments": args},
					})
				}
				v["tool_calls"] = calls
			}
			messages = append(messages, v)
		case llm.RoleTool:
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": m.ToolCallID, "content": m.Text,
			})
		}
	}
	body := map[string]any{"model": req.Model, "stream": true, "messages": messages}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.Parameters,
				},
			})
		}
		body["tools"] = tools
	}
	if req.Thinking != "" && req.Thinking != "off" {
		body["reasoning"] = map[string]any{"effort": req.Thinking}
	}
	for k, v := range extra {
		body[k] = v
	}
	return json.Marshal(body)
}

// thinkingUnsupported is the PLAN §3 small table of provider/model pairs
// whose upstream rejects the Chat Completions `reasoning` request field
// (HTTP 400 unknown field). Values are model id prefixes. The thinking param
// is dropped for these combos — never an error. Settled by live smoke
// 2026-10-06: GLM on the OpenCode Go gateway rejects `reasoning`, while
// mimo/qwen/deepseek on the gateway and z-ai/glm on OpenRouter accept it.
var thinkingUnsupported = map[string][]string{
	"opencode-go":  {"glm-"},
	"opencode-zen": {"glm-"},
}

func hasPrefix(prefixes []string, model string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// thinkingParamOK reports whether the `reasoning` request field is supported
// for this provider/model; unsupported params are dropped, not errors.
func thinkingParamOK(prov, model string) bool {
	for _, prefix := range thinkingUnsupported[prov] {
		if strings.HasPrefix(model, prefix) {
			return false
		}
	}
	return true
}

// ---- SSE pump ----

// PumpSSE reads SSE frames and turns each data payload into events with
// decode. Shared by the Chat Completions clients and the codex Responses
// client (which supplies its own decoder).
func PumpSSE(ctx context.Context, body io.Reader, events chan<- llm.Event, decode func(string) []llm.Event) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var fields []string
	emit := func(ev llm.Event) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	dispatch := func() {
		if len(fields) == 0 {
			return
		}
		data := strings.Join(fields, "\n")
		fields = nil
		for _, ev := range decode(data) {
			if !emit(ev) {
				return
			}
		}
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			dispatch()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			fields = append(fields, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// Only "data:" lines matter; event:/id:/retry: are ignored.
	}
	if err := sc.Err(); err != nil && !isCanceled(err) {
		emit(llm.Event{Type: llm.EventError, Err: fmt.Errorf("chat: stream read: %v", err)})
	}
	dispatch()
	// A cancelled stream stops mid-frame; the caller sees the closed channel.
}

func isCanceled(err error) bool {
	return err != nil && strings.Contains(err.Error(), "context canceled")
}

// decodeEvent decodes one SSE payload into zero or more events.
func decodeEvent(data string) []llm.Event {
	if data == "[DONE]" {
		return []llm.Event{{Type: llm.EventDone, Stop: "done"}}
	}
	var env struct {
		Choices []struct {
			Delta struct {
				Content   *string `json:"content"`
				Reasoning *string `json:"reasoning_content"`
				ToolCalls []struct {
					Index    *int    `json:"index"`
					ID       *string `json:"id"`
					Function *struct {
						Name      *string `json:"name"`
						Arguments *string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			Finish *string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &env) != nil {
		return []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("chat: bad SSE frame: %.200s", data)}}
	}
	var out []llm.Event
	if len(env.Error) > 0 && string(env.Error) != "null" {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(env.Error, &msg)
		return []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("chat: provider error: %s", nonEmpty(msg.Message, string(env.Error)))}}
	}
	if len(env.Usage) > 0 && string(env.Usage) != "null" {
		var u struct {
			PromptTokens        int     `json:"prompt_tokens"`
			CompletionTokens    int     `json:"completion_tokens"`
			Cost                float64 `json:"cost"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		}
		if json.Unmarshal(env.Usage, &u) == nil {
			var cached int
			if u.PromptTokensDetails != nil {
				cached = u.PromptTokensDetails.CachedTokens
			}
			out = append(out, llm.Event{Type: llm.EventUsage, Usage: &llm.Usage{
				In: u.PromptTokens, Out: u.CompletionTokens, Cached: cached, Cost: u.Cost,
			}})
		}
	}
	for _, ch := range env.Choices {
		d := ch.Delta
		if d.Reasoning != nil && *d.Reasoning != "" {
			out = append(out, llm.Event{Type: llm.EventThinkingDelta, Thinking: *d.Reasoning})
		}
		if d.Content != nil && *d.Content != "" {
			out = append(out, llm.Event{Type: llm.EventTextDelta, TextDelta: *d.Content})
		}
		for i := range d.ToolCalls {
			tc := &d.ToolCalls[i]
			name, args := "", ""
			if tc.Function != nil {
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				if tc.Function.Arguments != nil {
					args = *tc.Function.Arguments
				}
			}
			var id string
			if tc.ID != nil {
				id = *tc.ID
			}
			// The index field is the stream key; fall back to array position.
			seq := i
			if tc.Index != nil {
				seq = *tc.Index
			}
			out = append(out, llm.Event{
				Type: llm.EventToolCall,
				Seq:  seq,
				Call: &llm.ToolCall{ID: id, Name: name, Args: args},
			})
		}
		if ch.Finish != nil {
			out = append(out, llm.Event{Type: llm.EventDone, Stop: *ch.Finish})
		}
	}
	return out
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
