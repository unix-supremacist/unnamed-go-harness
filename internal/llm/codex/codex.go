// Package codex is the Codex Responses SSE client (PLAN.md §3), compact
// copy-and-trim of old/internal/provider/codex/provider.go. Kept: store:false,
// encrypted reasoning passthrough (reasoning items replay verbatim through
// llm.Message.Native) and the chatgpt-account-id header. Dropped: strict
// JSON/order validation, native-payload reconciliation and journal binding.
package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/llm/chat"
)

// Endpoint is the fixed ChatGPT inference host.
const Endpoint = "https://chatgpt.com/backend-api/codex/responses"

// Tokens supplies the current OAuth access token and account id (the auth
// package refreshes expired tokens under its store lock).
type Tokens func(ctx context.Context) (access, accountID string, err error)

// Client implements llm.Provider over Codex Responses SSE.
type Client struct {
	tokens   Tokens
	session  string // prompt_cache_key / request id
	endpoint string // overridden by the fixture test only
	http     *http.Client
}

// New returns a client. The optional http client is injected by tests.
func New(t Tokens, hc *http.Client) *Client {
	if hc == nil {
		hc = chat.NewHTTPClient()
	}
	return &Client{tokens: t, session: newID(), endpoint: Endpoint, http: hc}
}

// Stream posts one turn and pumps decoded SSE events into the channel.
func (c *Client) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	body, err := buildRequest(req, c.session)
	if err != nil {
		return nil, err
	}
	access, account, err := c.tokens(ctx)
	if err != nil {
		return nil, err
	}
	gctx, guard := chat.NewGuard(ctx)
	r, err := http.NewRequestWithContext(gctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex: build request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+access)
	r.Header.Set("chatgpt-account-id", account)
	r.Header.Set("originator", "pi")
	r.Header.Set("OpenAI-Beta", "responses=experimental")
	r.Header.Set("User-Agent", "unnamedgoharness/0.1")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("session-id", c.session)
	r.Header.Set("x-client-request-id", c.session)
	resp, err := c.http.Do(r)
	if err != nil {
		err = guard.Err(err)
		guard.Stop()
		return nil, fmt.Errorf("codex: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		guard.Stop()
		return nil, &llm.HTTPError{Provider: "codex", StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	ct := resp.Header.Get("Content-Type")
	// The live endpoint streams SSE without a Content-Type header; only
	// reject a present, wrong type.
	if ct != "" && !strings.Contains(strings.ToLower(ct), "event-stream") {
		resp.Body.Close()
		guard.Stop()
		return nil, fmt.Errorf("codex: expected SSE, got %q", ct)
	}
	resp.Body = guard.Body(resp.Body)
	events := make(chan llm.Event, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		chat.PumpSSE(ctx, resp.Body, events, decodeEvent)
	}()
	return events, nil
}

// buildRequest serializes the Responses body: history becomes input items,
// assistant turns replay their native items verbatim when present (that is
// how encrypted reasoning round-trips), store stays false.
func buildRequest(req llm.Request, session string) ([]byte, error) {
	input := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			input = append(input, map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": m.Text}}})
		case llm.RoleAssistant:
			if m.Native != "" {
				var items []json.RawMessage
				if json.Unmarshal([]byte(m.Native), &items) == nil {
					for _, it := range items {
						input = append(input, it)
					}
					continue
				}
			}
			if m.Text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": m.Text, "annotations": []any{}}},
					"status":  "completed"})
			}
			for _, call := range m.ToolCalls {
				args := call.Args
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{"type": "function_call",
					"call_id": call.ID, "name": call.Name, "arguments": args})
			}
		case llm.RoleTool:
			input = append(input, map[string]any{"type": "function_call_output",
				"call_id": m.ToolCallID, "output": m.Text})
		}
	}
	body := map[string]any{
		"model": req.Model, "store": false, "stream": true,
		"instructions": req.System, "input": input,
		"include":          []string{"reasoning.encrypted_content"},
		"prompt_cache_key": session, "tool_choice": "auto", "parallel_tool_calls": true,
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name,
				"description": t.Description, "parameters": t.Parameters, "strict": nil})
		}
		body["tools"] = tools
	}
	if req.Thinking != "" && req.Thinking != "off" {
		body["reasoning"] = map[string]any{"effort": req.Thinking}
	}
	return json.Marshal(body)
}

// decodeEvent turns one SSE payload into events. Items arrive complete in
// response.output_item.done and are emitted as native replay items; text,
// thinking and function-call arguments arrive as deltas keyed by the output
// index (the stream index shared with the call's id/name frames).
func decodeEvent(data string) []llm.Event {
	if data == "[DONE]" {
		return []llm.Event{{Type: llm.EventDone, Stop: "done"}}
	}
	var env struct {
		Type   string          `json:"type"`
		Delta  string          `json:"delta"`
		Output int             `json:"output_index"`
		Item   json.RawMessage `json:"item"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Response *struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				InputDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(data), &env) != nil {
		return []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("codex: bad SSE frame: %.200s", data)}}
	}
	switch env.Type {
	case "response.output_text.delta":
		if env.Delta != "" {
			return []llm.Event{{Type: llm.EventTextDelta, TextDelta: env.Delta}}
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if env.Delta != "" {
			return []llm.Event{{Type: llm.EventThinkingDelta, Thinking: env.Delta}}
		}
	case "response.function_call_arguments.delta":
		if env.Delta != "" {
			return []llm.Event{{Type: llm.EventToolCall, Seq: env.Output, Call: &llm.ToolCall{Args: env.Delta}}}
		}
	case "response.output_item.added":
		return itemCall(env.Output, env.Item) // id/name only; items are incomplete here
	case "response.output_item.done":
		return itemDone(env.Output, env.Item)
	case "response.completed", "response.done":
		out := []llm.Event{}
		if env.Response != nil && env.Response.Usage != nil {
			u := env.Response.Usage
			out = append(out, llm.Event{Type: llm.EventUsage, Usage: &llm.Usage{
				In: u.InputTokens, Out: u.OutputTokens, Cached: u.InputDetails.CachedTokens}})
		}
		return append(out, llm.Event{Type: llm.EventDone, Stop: "stop"})
	case "response.failed", "response.incomplete", "response.cancelled", "error":
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		if msg == "" && env.Response != nil && env.Response.Error != nil {
			msg = env.Response.Error.Message
		}
		return []llm.Event{{Type: llm.EventError, Err: fmt.Errorf("codex: %s", nonEmpty(msg, env.Type))}}
	}
	return nil
}

// itemCall announces a function call's id/name on output_item.added (its
// arguments stream separately under the same output index).
func itemCall(seq int, raw json.RawMessage) []llm.Event {
	var it struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &it) != nil || it.Type != "function_call" {
		return nil
	}
	return []llm.Event{{Type: llm.EventToolCall, Seq: seq, Call: &llm.ToolCall{ID: it.CallID, Name: it.Name}}}
}

// itemDone projects one completed output item: function calls announce their
// id/name (arguments stream separately) and join the native replay payload,
// message and reasoning items become native replay items — reasoning only
// when encrypted, since that is the only form the API accepts back.
func itemDone(seq int, raw json.RawMessage) []llm.Event {
	var it struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Encrypted string `json:"encrypted_content"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &it) != nil {
		return nil
	}
	switch it.Type {
	case "function_call":
		return []llm.Event{
			{Type: llm.EventToolCall, Seq: seq, Call: &llm.ToolCall{ID: it.CallID, Name: it.Name}},
			{Type: llm.EventNative, Native: string(raw)},
		}
	case "message":
		return []llm.Event{{Type: llm.EventNative, Native: string(raw)}}
	case "reasoning":
		if it.Encrypted == "" {
			return nil
		}
		return []llm.Event{{Type: llm.EventNative, Native: string(raw)}}
	}
	return nil
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// newID returns a random request/session id.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ugh-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}
