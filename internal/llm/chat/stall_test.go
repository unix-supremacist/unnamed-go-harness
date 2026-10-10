package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
)

func TestStalledStreamFailsSteadyStreamDoesNot(t *testing.T) {
	old := StallTimeout
	StallTimeout = 150 * time.Millisecond
	defer func() { StallTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// Steady: 6 chunks 100ms apart (600ms total, longer than the stall
		// timeout, but never silent for 150ms). Then go silent.
		for i := 0; i < 6; i++ {
			w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"))
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		if r.URL.Query().Get("stall") == "" {
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	for _, stall := range []bool{false, true} {
		c := New(srv.URL, "k", nil)
		if stall {
			c.base += "?stall=1#"
		}
		ch, err := c.Stream(context.Background(), llm.Request{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		var gotErr error
		for ev := range ch {
			if ev.Type == llm.EventTextDelta {
				text.WriteString(ev.TextDelta)
			}
			if ev.Type == llm.EventError {
				gotErr = ev.Err
			}
		}
		if text.String() != "xxxxxx" {
			t.Errorf("stall=%v text = %q", stall, text.String())
		}
		if stall != (gotErr != nil && strings.Contains(gotErr.Error(), "stalled")) {
			t.Errorf("stall=%v err = %v", stall, gotErr)
		}
	}
}
