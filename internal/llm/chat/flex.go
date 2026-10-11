package chat

import (
	"context"
	"time"

	"github.com/unix/ugh/internal/llm"
)

// Flex routes a request to OpenRouter's flex endpoints first: half price,
// best effort. OpenRouter itself moves on to normal routing when a flex
// endpoint errors (503, rate limit); a flex queue that is merely slow never
// errors, so a request that streams nothing within FirstToken is cancelled
// and resent without the flex order. Nothing has been emitted by then, so
// the resend is invisible to the caller.
type Flex struct {
	Endpoints  []string // provider tags, e.g. "google-ai-studio/flex"
	FirstToken time.Duration
	Root       bool // also for foreground (root agent) turns
}

func (f *Flex) applies(req llm.Request) bool {
	return f != nil && len(f.Endpoints) > 0 && (!req.Foreground || f.Root)
}

func (c *Client) streamFlex(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	fctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(c.Flex.FirstToken, cancel)
	prefs := map[string]any{"provider": map[string]any{"order": c.Flex.Endpoints, "allow_fallbacks": true}}
	ch, err := c.stream(fctx, req, prefs)
	if err == nil {
		if ev, ok := <-ch; ok && ev.Type != llm.EventError {
			timer.Stop()
			out := make(chan llm.Event, 16)
			out <- ev
			go func() {
				defer close(out)
				defer cancel()
				for ev := range ch {
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}()
			return out, nil
		}
		go func() {
			for range ch { // the cancelled pump closes it
			}
		}()
	}
	timer.Stop()
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return c.stream(ctx, req, nil)
}
