// /login: the async codex login from the TUI. When a valid credential is
// already stored the command says so; otherwise the device-code flow runs in
// the background and streams its status lines into the transcript, so the
// TUI stays usable while the human authorizes in a browser.
package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
)

// startLogin reports an existing credential or launches the background flow.
func (s *State) startLogin() {
	cfg, err := config.Load()
	if err != nil {
		s.addEntry(entry{kind: "error", text: "/login: " + err.Error()})
		s.refresh()
		return
	}
	st, err := auth.Open(cfg.Auth.Store)
	if err != nil {
		s.addEntry(entry{kind: "error", text: "/login: " + err.Error()})
		s.refresh()
		return
	}
	if cred, ok, gerr := st.Get("codex"); gerr == nil && ok &&
		cred.Type == "oauth" && cred.Access != "" &&
		(cred.Expires == 0 || cred.Expires > time.Now().UnixMilli()) {
		when := "no expiry recorded"
		if cred.Expires > 0 {
			when = "expires " + time.UnixMilli(cred.Expires).UTC().Format(time.RFC3339)
		}
		s.addEntry(entry{kind: "info", text: "· codex already logged in (" + when + ")"})
		s.refresh()
		return
	}
	s.addEntry(entry{kind: "info", text: "· starting codex login in the background — the TUI stays usable…"})
	s.refresh()
	p := s.prog.Load()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { // cancel the flow when the TUI quits
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		err := auth.LoginDevice(ctx, st, func(url, code string) error {
			if p != nil {
				p.Send(loginMsg{line: "· login: open " + url + " and enter code " + code})
			}
			return nil
		})
		if p == nil {
			return
		}
		switch {
		case err == nil:
			p.Send(loginMsg{line: "· codex login complete"})
		case ctx.Err() != nil:
			p.Send(loginMsg{line: "· codex login cancelled"})
		default:
			p.Send(loginMsg{line: fmt.Sprintf("· codex login failed: %v", err)})
		}
	}()
}
