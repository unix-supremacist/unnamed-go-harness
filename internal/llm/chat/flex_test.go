package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
)

// flexServer answers flex-ordered requests with flex(w) and plain requests
// with the text fixture, recording whether each request carried the order.
func flexServer(t *testing.T, flex func(w http.ResponseWriter, r *http.Request)) (*Client, *[]bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "text.sse"))
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		seen []bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Provider *struct {
				Order []string `json:"order"`
			} `json:"provider"`
		}
		json.Unmarshal(b, &body)
		isFlex := body.Provider != nil && len(body.Provider.Order) > 0
		mu.Lock()
		seen = append(seen, isFlex)
		mu.Unlock()
		if isFlex {
			flex(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "sk-test", srv.Client())
	c.Flex = &Flex{Endpoints: []string{"google-ai-studio/flex"}, FirstToken: 200 * time.Millisecond}
	return c, &seen
}

func streamText(t *testing.T, c *Client, req llm.Request) string {
	t.Helper()
	ch, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var got collected
	got.run(t, ch)
	return got.text.String()
}

func TestFlexServesWhenFast(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join("testdata", "text.sse"))
	c, seen := flexServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(raw)
	})
	if streamText(t, c, llm.Request{Model: "google/gemini-x"}) == "" {
		t.Fatal("no text")
	}
	if len(*seen) != 1 || !(*seen)[0] {
		t.Fatalf("requests = %v, want one flex request", *seen)
	}
}

func TestFlexFallsBackOnError(t *testing.T) {
	c, seen := flexServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":503}}`, http.StatusServiceUnavailable)
	})
	if streamText(t, c, llm.Request{Model: "google/gemini-x"}) == "" {
		t.Fatal("no text after fallback")
	}
	if len(*seen) != 2 || !(*seen)[0] || (*seen)[1] {
		t.Fatalf("requests = %v, want flex then plain", *seen)
	}
}

func TestFlexFallsBackWhenSilent(t *testing.T) {
	c, seen := flexServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // queued: comments only, never a token
	})
	start := time.Now()
	if streamText(t, c, llm.Request{Model: "google/gemini-x"}) == "" {
		t.Fatal("no text after fallback")
	}
	if len(*seen) != 2 || (*seen)[1] {
		t.Fatalf("requests = %v, want flex then plain", *seen)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("fallback took %s", d)
	}
}

func TestFlexSkipsForeground(t *testing.T) {
	c, seen := flexServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("foreground turn went to flex")
	})
	streamText(t, c, llm.Request{Model: "google/gemini-x", Foreground: true})
	if len(*seen) != 1 || (*seen)[0] {
		t.Fatalf("requests = %v, want one plain request", *seen)
	}
}
