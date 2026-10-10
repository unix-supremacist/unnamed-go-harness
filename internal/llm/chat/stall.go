package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// StallTimeout ends a model stream that delivers no bytes for this long,
// whether waiting for headers or mid-body. Long but steady streams are
// fine: there is no cap on total duration.
var StallTimeout = 3 * time.Minute

// NewHTTPClient is the default client for streaming providers: no total
// timeout (a long turn is legitimate); stalls are caught by Guard.
func NewHTTPClient() *http.Client { return &http.Client{} }

// Guard derives a request context that is cancelled once no bytes have
// flowed for StallTimeout. Call body(resp.Body) to wrap the response body
// (each read resets the clock) and err(e) to turn the resulting
// cancellation into a clear "stalled" error.
type Guard struct {
	timer   *time.Timer
	stalled atomic.Bool
	cancel  context.CancelFunc
}

func NewGuard(ctx context.Context) (context.Context, *Guard) {
	ctx, cancel := context.WithCancel(ctx)
	g := &Guard{cancel: cancel}
	g.timer = time.AfterFunc(StallTimeout, func() { g.stalled.Store(true); cancel() })
	return ctx, g
}

// Body wraps a response body so every read extends the deadline; closing
// it releases the guard.
func (g *Guard) Body(rc io.ReadCloser) io.ReadCloser { return &guardedBody{rc, g} }

// Stop releases the guard without a body (request failed).
func (g *Guard) Stop() { g.timer.Stop(); g.cancel() }

// Err rewrites errors caused by a stall.
func (g *Guard) Err(err error) error {
	if err != nil && g.stalled.Load() {
		return fmt.Errorf("stream stalled: no data for %s", StallTimeout)
	}
	return err
}

type guardedBody struct {
	io.ReadCloser
	g *Guard
}

func (b *guardedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.g.timer.Reset(StallTimeout)
	}
	if err != nil && err != io.EOF {
		err = b.g.Err(err)
	}
	return n, err
}

func (b *guardedBody) Close() error {
	b.g.timer.Stop()
	err := b.ReadCloser.Close()
	b.g.cancel()
	return err
}
