package auth

// Codex OAuth login (PKCE browser + device code) and token refresh: compact
// copy-and-trim of old/internal/auth/{codex_flow,oauth_primitives,codex}.go.
// The strict JSON/surrogate/journal machinery is gone; the browser callback
// still accepts extra provider parameters (e.g. RFC 9207 iss) and ignores
// them. Refresh takes an exclusive flock on $XDG_RUNTIME_DIR/ugh/auth.lock,
// re-reads the store under the lock, refreshes only when the stored token is
// expired, and persists rotated tokens before they are used (§3.1). Pi's
// codex credential is never imported implicitly; this flow is independent.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	codexProvider      = "codex"
	codexClient        = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexHost          = "https://auth.openai.com"
	browserRedirect    = "http://localhost:1455/auth/callback"
	deviceRedirect     = codexHost + "/deviceauth/callback"
	deviceVerification = codexHost + "/codex/device"
	codexExpirySkew    = 30 * time.Second // within this of expiry counts as expired
)

var codexHTTP = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// codexPost posts one body to the fixed authorization server and returns the
// status plus a best-effort JSON object. Bodies are never echoed into errors.
func codexPost(ctx context.Context, path, contentType, body string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexHost+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "unnamedgoharness/0.1")
	res, err := codexHTTP.Do(req)
	if err != nil {
		return 0, nil, errors.New("auth: codex authorization server unreachable")
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	return res.StatusCode, m, nil
}

// pkce returns a 32-byte random verifier and its S256 challenge.
func pkce() (verifier, challenge string, err error) {
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", errors.New("auth: randomness unavailable")
	}
	verifier = base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func oauthState() (string, error) {
	var secret [16]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", errors.New("auth: randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

// CodexPlanType reads the unverified chatgpt_plan_type claim of a Codex access
// token ("plus", "pro", ...). It is a hint for local burn-rate presets, not
// proof of entitlement. Empty when absent.
func CodexPlanType(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(claims.Auth.Plan))
}

// codexAccountID reads the unverified chatgpt_account_id JWT claim. It does
// not authenticate the token or grant entitlement.
func codexAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// codexCredential turns a token response into a stored credential. The
// account id comes from the access token claim, falling back to oldID on
// refresh; the refresh token falls back to the current one when the server
// does not rotate it.
func codexCredential(m map[string]any, oldRefresh, oldID string) (Credential, error) {
	access, _ := m["access_token"].(string)
	refresh, _ := m["refresh_token"].(string)
	if refresh == "" {
		refresh = oldRefresh
	}
	expires, _ := m["expires_in"].(float64)
	id := codexAccountID(access)
	if id == "" {
		id = oldID
	}
	if access == "" || refresh == "" || id == "" || expires < 60 {
		return Credential{}, errors.New("auth: codex token response incomplete")
	}
	return Credential{
		Type: "oauth", Access: access, Refresh: refresh, AccountID: id,
		Expires: time.Now().Add(time.Duration(expires) * time.Second).UnixMilli(),
	}, nil
}

func codexExchange(ctx context.Context, code, verifier, redirect string) (Credential, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {codexClient},
		"code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect},
	}
	status, m, err := codexPost(ctx, "/oauth/token", "application/x-www-form-urlencoded", form.Encode())
	if err != nil {
		return Credential{}, err
	}
	if status != http.StatusOK {
		return Credential{}, errors.New("auth: codex authorization rejected")
	}
	return codexCredential(m, "", "")
}

// LoginStatus is the persisted state of a (possibly backgrounded) login, so
// the owner can see the authorization URL and whether the flow finished even
// after the starting process is gone.
type LoginStatus struct {
	Mode     string `json:"mode"`                // browser | device
	State    string `json:"state"`               // starting | waiting | done | error
	URL      string `json:"url,omitempty"`       // where the owner authorizes
	UserCode string `json:"user_code,omitempty"` // device flow code
	Detail   string `json:"detail,omitempty"`    // safe to display; never a secret
	At       int64  `json:"at"`
}

// RuntimeDir is $XDG_RUNTIME_DIR/ugh (per-uid temp fallback), 0700.
func RuntimeDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(base) {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("ugh-%d", os.Getuid()))
	}
	dir := filepath.Join(base, AppDir)
	_ = os.MkdirAll(dir, 0700)
	return dir
}

func loginStatusPath() string { return filepath.Join(RuntimeDir(), "codex-login.json") }

// WriteLoginStatus persists the login state (no secrets ever).
func WriteLoginStatus(st LoginStatus) error {
	st.At = time.Now().UnixMilli()
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(loginStatusPath(), raw, 0600)
}

// ReadLoginStatus returns the last persisted login state.
func ReadLoginStatus() (LoginStatus, error) {
	var st LoginStatus
	raw, err := os.ReadFile(loginStatusPath())
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// LoginBrowser runs the PKCE browser flow and stores the result as the codex
// credential. present (optional) opens the authorize URL in the owner's
// browser — best effort; the URL is always written to the status file so the
// owner can open it manually. The callback accepts one code + one matching
// state and ignores any extra provider parameters.
func LoginBrowser(ctx context.Context, st *Store, present func(url string) error) error {
	verifier, challenge, err := pkce()
	if err != nil {
		return err
	}
	state, err := oauthState()
	if err != nil {
		return err
	}
	q := url.Values{
		"response_type": {"code"}, "client_id": {codexClient},
		"redirect_uri": {browserRedirect}, "scope": {"openid profile email offline_access"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {state},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"},
		"originator": {"unnamedgoharness"},
	}
	authURL := codexHost + "/oauth/authorize?" + q.Encode()
	ln, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		return errors.New("auth: codex callback port 1455 unavailable")
	}
	defer ln.Close()
	codes := make(chan string, 1)
	var once atomic.Bool
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			vals, _ := url.ParseQuery(r.URL.RawQuery)
			code := vals.Get("code")
			ok := r.Method == http.MethodGet && r.URL.Path == "/auth/callback" &&
				len(vals["code"]) == 1 && len(vals["state"]) == 1 && vals.Get("state") == state &&
				code != "" && once.CompareAndSwap(false, true)
			if !ok {
				http.Error(w, "Invalid callback", http.StatusBadRequest)
				return
			}
			codes <- code
			fmt.Fprintln(w, "Authentication completed. You may close this window.")
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// Sane bound: the flow gives up after 15 minutes like the salvaged one.
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_ = WriteLoginStatus(LoginStatus{Mode: "browser", State: "waiting", URL: authURL,
		Detail: "open the URL in a browser and approve access"})
	if present != nil {
		_ = present(authURL)
	}
	var code string
	select {
	case <-waitCtx.Done():
		_ = WriteLoginStatus(LoginStatus{Mode: "browser", State: "error", Detail: "login cancelled or timed out"})
		return waitCtx.Err()
	case code = <-codes:
	}
	cred, err := codexExchange(waitCtx, code, verifier, browserRedirect)
	if err != nil {
		_ = WriteLoginStatus(LoginStatus{Mode: "browser", State: "error", Detail: "token exchange failed"})
		return err
	}
	if err := st.Put(codexProvider, cred); err != nil {
		return err
	}
	_ = WriteLoginStatus(LoginStatus{Mode: "browser", State: "done", Detail: "codex credential stored"})
	return nil
}

// LoginDevice runs the device-code flow: the owner opens the verification URL
// anywhere, enters the user code, and the flow polls until granted. present
// (optional) receives the verification URL and the user code.
func LoginDevice(ctx context.Context, st *Store, present func(url, userCode string) error) error {
	fail := func(detail string, err error) error {
		_ = WriteLoginStatus(LoginStatus{Mode: "device", State: "error", Detail: detail})
		return err
	}
	status, m, err := codexPost(ctx, "/api/accounts/deviceauth/usercode", "application/json",
		`{"client_id":"`+codexClient+`"}`)
	if err != nil {
		return fail("device flow unavailable", err)
	}
	device, _ := m["device_auth_id"].(string)
	user, _ := m["user_code"].(string)
	if status != http.StatusOK || device == "" || user == "" {
		return fail("device flow unavailable", errors.New("auth: codex device flow unavailable"))
	}
	if s, ok := m["expires_in"].(float64); ok && s > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(s)*time.Second)
		defer cancel()
	}
	_ = WriteLoginStatus(LoginStatus{Mode: "device", State: "waiting", URL: deviceVerification,
		UserCode: user, Detail: "open the URL and enter the code"})
	if present != nil {
		_ = present(deviceVerification, user)
	}
	poll, _ := json.Marshal(map[string]string{"device_auth_id": device, "user_code": user})
	interval := 5 * time.Second
	for i := 0; i < 900; i++ {
		if ctx.Err() != nil {
			return fail("device authorization timed out", ctx.Err())
		}
		status, m, err = codexPost(ctx, "/api/accounts/deviceauth/token", "application/json", string(poll))
		if err != nil {
			return fail("device flow unavailable", err)
		}
		if status == http.StatusOK {
			code, _ := m["authorization_code"].(string)
			verifier, _ := m["code_verifier"].(string)
			if code == "" || verifier == "" {
				return fail("device response incomplete", errors.New("auth: codex device response incomplete"))
			}
			cred, err := codexExchange(ctx, code, verifier, deviceRedirect)
			if err != nil {
				return fail("token exchange failed", err)
			}
			if err := st.Put(codexProvider, cred); err != nil {
				return err
			}
			_ = WriteLoginStatus(LoginStatus{Mode: "device", State: "done", Detail: "codex credential stored"})
			return nil
		}
		switch deviceCode(m) {
		case "deviceauth_authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return fail("device authorization was not granted", errors.New("auth: codex device authorization was not granted"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	return fail("device authorization timed out", errors.New("auth: codex device authorization timed out"))
}

// deviceCode reads the safe error code from a device poll response.
func deviceCode(m map[string]any) string {
	if s, ok := m["error"].(string); ok {
		return s
	}
	if obj, ok := m["error"].(map[string]any); ok {
		s, _ := obj["code"].(string)
		return s
	}
	return ""
}

// CodexTokens returns a valid access token + account id for the codex API.
// Expired credentials are refreshed under an exclusive flock on
// $XDG_RUNTIME_DIR/ugh/auth.lock: the store is re-read under the lock, the
// refresh happens only if the token is still expired (another process may
// have refreshed already), and rotated tokens are persisted before use.
func CodexTokens(ctx context.Context, st *Store) (string, string, error) {
	cred, ok, err := st.Get(codexProvider)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", errors.New("auth: no codex credential; run `ugh login codex`")
	}
	if !codexExpired(cred) {
		return cred.Access, cred.AccountID, nil
	}
	unlock, err := lockAuth(ctx)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	cred, ok, err = st.Get(codexProvider) // re-read under the lock
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", errors.New("auth: codex credential removed concurrently")
	}
	if !codexExpired(cred) {
		return cred.Access, cred.AccountID, nil
	}
	return codexRefreshLocked(ctx, st, cred)
}

// CodexRefreshNow forces a token refresh after the server rejected a token
// that still looked valid locally (revoked, rotated elsewhere). It reports
// whether a fresh credential is now stored.
func CodexRefreshNow(ctx context.Context, st *Store) error {
	unlock, err := lockAuth(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cred, ok, err := st.Get(codexProvider)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("auth: no codex credential; run `ugh login codex`")
	}
	_, _, err = codexRefreshLocked(ctx, st, cred)
	return err
}

func codexRefreshLocked(ctx context.Context, st *Store, cred Credential) (string, string, error) {
	form := url.Values{
		"grant_type": {"refresh_token"}, "client_id": {codexClient},
		"refresh_token": {cred.Refresh},
	}
	status, m, err := codexPost(ctx, "/oauth/token", "application/x-www-form-urlencoded", form.Encode())
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", errors.New("auth: codex refresh rejected; run `ugh login codex` again")
	}
	next, err := codexCredential(m, cred.Refresh, cred.AccountID)
	if err != nil {
		return "", "", err
	}
	if next.AccountID != cred.AccountID {
		return "", "", errors.New("auth: codex refresh changed account identity")
	}
	if err := st.Put(codexProvider, next); err != nil {
		return "", "", err
	}
	return next.Access, next.AccountID, nil // persisted before use
}

func codexExpired(c Credential) bool {
	return c.Expires <= time.Now().Add(codexExpirySkew).UnixMilli()
}

// lockAuth takes the exclusive advisory lock used to serialize token
// refreshes across ugh processes; the returned func releases it.
func lockAuth(ctx context.Context) (func(), error) {
	path := filepath.Join(RuntimeDir(), "auth.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("auth: cannot open refresh lock")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = syscall.Close(fd)
			}, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			_ = syscall.Close(fd)
			return nil, errors.New("auth: refresh lock busy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
