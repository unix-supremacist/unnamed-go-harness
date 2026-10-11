// Package llm defines the provider contract and normalized conversation
// types (§2 of PLAN.md). Model refs are "provider/model" strings.
package llm

import (
	"context"
	"fmt"
)

// Provider streams one assistant turn for a Chat-Completions-style API.
type Provider interface {
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}

// Request is one turn request. Thinking is "off|low|medium|high"; providers
// drop unsupported levels (never error).
type Request struct {
	Model    string // "model" part of the provider/model ref
	System   string
	Messages []Message
	Tools    []ToolSpec
	Thinking string
	// Foreground marks a turn a human is waiting on (the root agent), so
	// providers skip best-effort tiers such as OpenRouter flex.
	Foreground bool
}

// Role of a message in the history.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one function call requested by the model.
type ToolCall struct {
	ID   string
	Name string
	Args string // raw JSON arguments string
}

// Message is one entry in the conversation history.
//   - RoleUser/RoleSystem: Text carries the content.
//   - RoleAssistant: Text, optional Reasoning, optional ToolCalls.
//   - RoleTool: ToolCallID + Text carry the result of ToolCalls[i].ID.
type Message struct {
	Role       Role
	Text       string
	Reasoning  string     // assistant: reasoning content, kept for the log
	ToolCalls  []ToolCall // assistant: calls it made
	ToolCallID string     // tool result
	ToolName   string     // tool result
	// Native is an assistant turn's provider-native replay payload as a JSON
	// array string (codex: output items incl. encrypted reasoning), passed
	// back verbatim on later turns. Empty for Chat Completions providers.
	Native string
	// Route is the provider/model ref that produced an assistant turn; routes
	// that differ drop its Native/Reasoning payloads (see agent.adaptHistory).
	Route string
	// Ord is a tool result's ordinal among this conversation's tool results
	// (1-based; the context ladder and recall address it as "c<Ord>"). 0 for
	// results recorded before ordinals existed.
	Ord int `json:",omitempty"`
}

// ToolSpec declares a tool to the model.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Event is one streamed item. ToolCall events are fragments: the first
// frame for a stream index carries ID+Name, later frames append Args deltas;
// Seq is the stream index used to assemble them.
type Event struct {
	Type      EventType
	TextDelta string    // TextDelta
	Thinking  string    // ThinkingDelta
	Native    string    // Native: one provider-native item (JSON) for replay
	Seq       int       // ToolCall: stream index of the call
	Call      *ToolCall // ToolCall fragment
	Usage     *Usage    // Usage
	Stop      string    // Done: stop reason
	Err       error     // Error
}

type EventType string

const (
	EventTextDelta     EventType = "text_delta"
	EventThinkingDelta EventType = "thinking_delta"
	EventNative        EventType = "native"
	EventToolCall      EventType = "tool_call"
	EventUsage         EventType = "usage"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

// Usage is token accounting for one turn.
type Usage struct {
	In, Out, Cached int
	Cost            float64 // USD as reported by the provider; 0 when not reported
}

// HTTPError represents a structured, typed HTTP error from a provider endpoint.
type HTTPError struct {
	Provider   string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}
