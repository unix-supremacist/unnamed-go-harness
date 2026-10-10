package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/perm"
)

// Console connections (BYOK providers set up in the OpenCode Console UI) are
// discovered from GET <console>/api/v2/config with the Console login token and
// become providers named opencode-<connection name> (e.g. opencode-openrouter).

var (
	consoleOnce  sync.Once
	consoleSpecs []Spec
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

type consoleConfig struct {
	Providers map[string]struct {
		Name     string `json:"name"`
		Settings struct {
			BaseURL string `json:"baseURL"`
		} `json:"settings"`
		Options struct {
			BaseURL string `json:"baseURL"`
		} `json:"options"`
		Headers map[string]string `json:"headers"`
	} `json:"providers"`
}

// parseConsoleConfig turns the workspace config into specs for the custom
// (BYOK) connections only; Zen/Go keep their own providers.
func parseConsoleConfig(body []byte, org string) ([]Spec, error) {
	var cc consoleConfig
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, err
	}
	var out []Spec
	seen := map[string]bool{}
	for key, p := range cc.Providers {
		base := p.Settings.BaseURL
		if base == "" {
			base = p.Options.BaseURL
		}
		if !strings.Contains(base, "/inference/custom/") {
			continue
		}
		name := slug(p.Name)
		if name == "" {
			name = slug(key)
		}
		name = strings.TrimPrefix(name, "default-") // Console names the first connection "Default <Provider>"
		if !strings.HasPrefix(name, "opencode") {
			name = "opencode-" + name
		}
		if seen[name] || name == "opencode-go" || name == "opencode-zen" {
			name += "-" + slug(key)
		}
		seen[name] = true
		h := map[string]string{}
		for k, v := range p.Headers {
			h[k] = v
		}
		if org != "" && h["x-opencode-org-id"] == "" {
			h["x-opencode-org-id"] = org
		}
		out = append(out, Spec{Name: name, BaseURL: strings.TrimRight(base, "/"), Headers: h, Session: true, Usage: "none", Console: true})
	}
	return out, nil
}

// consoleCreds returns the Console credential: the device-login token when
// present, else the stored/env OpenCode key (a service-account key works as a
// Console key). login reports which one was used.
func consoleCreds(ctx context.Context) (tok, org string, login bool, err error) {
	st, err := openStore()
	if err != nil {
		return "", "", false, err
	}
	if auth.HasConsole(st) {
		tok, org, err = auth.ConsoleToken(ctx, st)
		return tok, org, true, err
	}
	tok, err = auth.Resolve("opencode-go", os.LookupEnv, st)
	return tok, "", false, err
}

// consoleProviders fetches connections once per process. With only an API key
// (no login) a rejection is silent: it just means the key isn't a Console key.
func consoleProviders() []Spec {
	consoleOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tok, org, login, err := consoleCreds(ctx)
		if err != nil {
			return
		}
		perm.RegisterSecret(tok)
		body, err := fetchConsoleConfig(ctx, tok, org)
		if err == nil {
			consoleSpecs, err = parseConsoleConfig(body, org)
		}
		if err != nil && login {
			fmt.Fprintln(os.Stderr, "warning: opencode console providers unavailable:", err)
		}
	})
	return consoleSpecs
}

func fetchConsoleConfig(ctx context.Context, tok, org string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, auth.ConsoleURL+"/api/v2/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if org != "" {
		req.Header.Set("x-org-id", org)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("console config: HTTP %d", resp.StatusCode)
	}
	var buf strings.Builder
	dec := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	buf.Write(raw)
	return []byte(buf.String()), nil
}

// consoleKey supplies the Console credential for a discovered connection.
func consoleKey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tok, _, _, err := consoleCreds(ctx)
	if err == nil {
		perm.RegisterSecret(tok)
	}
	return tok, err
}
