package main

// `ugh login opencode` / `ugh login <configured-provider>`: API-key entry.
// The key is read via readKey (never argv), validated with a /models probe
// where the provider has one (a 401/403 rejects it unstored), then stored.
// One `opencode` credential backs opencode-go, opencode-zen and
// opencode-openrouter.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/provider"
)

const zenModelsURL = "https://opencode.ai/zen/v1/models"

// probeKey checks a key against a /models URL. Only an explicit auth
// rejection fails; network errors and other statuses don't block storing.
var probeKey = func(url, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("warning: could not validate key:", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("key rejected (HTTP %d); nothing stored", resp.StatusCode)
	}
	return nil
}

func loginKey(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ugh login " + args[0] + " (key is read from stdin)")
	}
	name, probe := args[0], ""
	if name == auth.ConsoleCredential {
		return loginConsole()
	}
	if name == "opencode" {
		probe = zenModelsURL
	} else {
		spec, ok := provider.Lookup(name)
		if !ok {
			return fmt.Errorf("login: unknown provider %q (try opencode, codex, or define [providers.%s] in config)", name, name)
		}
		probe = spec.BaseURL + "/models"
	}
	key, err := readKey()
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("login: empty key")
	}
	if err := probeKey(probe, key); err != nil {
		return fmt.Errorf("login %s: %w", name, err)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	if name == "opencode" {
		err = st.PutOpenCode(key, false)
	} else {
		err = st.Put(name, auth.Credential{Type: "api_key", Key: key})
	}
	if err != nil {
		return err
	}
	fmt.Printf("stored %s (%s)\n", name, auth.MaskKey(key))
	return nil
}

// loginConsole runs the Console device flow: no key is copied; connections
// set up in the Console UI are then discovered as providers.
func loginConsole() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = auth.LoginConsole(ctx, st, func(url, code string) {
		fmt.Printf("authorize at: %s\nuser code: %s\n", url, code)
		if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
			_ = exec.Command("xdg-open", url).Start()
		}
	})
	if err != nil {
		return err
	}
	fmt.Println("stored opencode-console; connected providers appear in `ugh models`")
	return nil
}
