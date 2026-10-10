package auth

// OpenCode Console login: the documented OAuth device grant (RFC 8628) against
// https://opencode.ai/console, so BYOK connections made in the Console can be
// used without copying any key. Tokens live in the store as the oauth
// credential "opencode-console" (AccountID holds the org id). Refresh tokens
// are single-use: a refreshed pair is persisted before it is used.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ConsoleCredential is the store key for the Console login.
const ConsoleCredential = "opencode-console"

// ConsoleURL is the Console base (overridable in tests).
var ConsoleURL = "https://opencode.ai/console"

const consoleClientID = "ugh"

var consoleMu sync.Mutex // serializes refresh within this process

type consoleToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	OrgID        string `json:"org_id"`
	Error        string `json:"error"`
}

func consolePost(ctx context.Context, path string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ConsoleURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
}

// LoginConsole runs the device flow. show receives the absolute URL to open
// and the user code; the call blocks until approval, expiry or ctx cancel.
func LoginConsole(ctx context.Context, s *Store, show func(url, code string)) error {
	var dc struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	code, err := consolePost(ctx, "/auth/device/code", url.Values{"client_id": {consoleClientID}, "supports_org_scope": {"true"}}, &dc)
	if err != nil || code != http.StatusOK || dc.DeviceCode == "" {
		return fmt.Errorf("console login: device code request failed (HTTP %d, %v)", code, err)
	}
	u, err := url.Parse(ConsoleURL)
	if err != nil {
		return err
	}
	// verification_uri_complete is a path relative to the site origin.
	show(u.Scheme+"://"+u.Host+dc.VerificationURIComplete, dc.UserCode)
	interval := time.Duration(dc.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		var tok consoleToken
		code, err := consolePost(ctx, "/auth/device/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {dc.DeviceCode},
			"client_id":   {consoleClientID},
		}, &tok)
		if err != nil && code == 0 {
			return err
		}
		switch {
		case code == http.StatusOK && tok.AccessToken != "":
			return saveConsole(s, tok)
		case tok.Error == "authorization_pending":
		case tok.Error == "slow_down":
			interval += 5 * time.Second
		default:
			return fmt.Errorf("console login: %s (HTTP %d)", tok.Error, code)
		}
	}
	return errors.New("console login: timed out waiting for approval")
}

func saveConsole(s *Store, t consoleToken) error {
	if t.RefreshToken == "" {
		return errors.New("console login: no refresh token returned")
	}
	return s.Put(ConsoleCredential, Credential{
		Type: "oauth", Access: t.AccessToken, Refresh: t.RefreshToken,
		Expires: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli(), AccountID: t.OrgID,
	})
}

// HasConsole reports whether a Console login is stored.
func HasConsole(s *Store) bool {
	_, ok, err := s.Get(ConsoleCredential)
	return err == nil && ok
}

// ConsoleToken returns a valid access token and org id, refreshing (and
// persisting the rotated pair first) when it expires within a minute.
func ConsoleToken(ctx context.Context, s *Store) (access, org string, err error) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	cred, ok, err := s.Get(ConsoleCredential)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", errors.New("auth: no Console login (run `ugh login opencode-console`)")
	}
	if cred.Expires == 0 || time.UnixMilli(cred.Expires).After(time.Now().Add(time.Minute)) {
		return cred.Access, cred.AccountID, nil
	}
	var tok consoleToken
	code, err := consolePost(ctx, "/auth/device/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {cred.Refresh}, "client_id": {consoleClientID},
	}, &tok)
	if err != nil || code != http.StatusOK || tok.AccessToken == "" {
		return "", "", fmt.Errorf("console token refresh failed (HTTP %d); run `ugh login opencode-console` again", code)
	}
	if tok.OrgID == "" {
		tok.OrgID = cred.AccountID
	}
	if err := saveConsole(s, tok); err != nil {
		return "", "", err
	}
	return tok.AccessToken, tok.OrgID, nil
}
