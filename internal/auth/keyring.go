package auth

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Keyring backend: one Secret Service item (service=ugh, user=credentials)
// holding the whole JSON document, so secrets stay out of plain files.
const (
	keyringService = "ugh"
	keyringUser    = "credentials"
)

type keyringBackend struct{}

func (keyringBackend) Name() string { return "keyring" }

func (keyringBackend) Load() (*Credentials, error) {
	secret, err := keyring.Get(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		return newCredentials(), nil
	}
	if err != nil {
		return nil, errors.New("auth: keyring unavailable")
	}
	var c Credentials
	if json.Unmarshal([]byte(secret), &c) != nil {
		return nil, errors.New("auth: keyring credentials corrupt")
	}
	if c.Version != 1 {
		return nil, errors.New("auth: credentials version unsupported")
	}
	if c.Providers == nil {
		c.Providers = map[string]Credential{}
	}
	return &c, nil
}

func (keyringBackend) Save(c *Credentials) error {
	if c.Version == 0 {
		c.Version = 1
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return errors.New("auth: credentials encoding failed")
	}
	if err := keyring.Set(keyringService, keyringUser, string(raw)); err != nil {
		return errors.New("auth: keyring write failed")
	}
	return nil
}

// probeKeyring reports whether the Secret Service is reachable. It is a
// variable so tests can force the file fallback without touching a keyring.
var probeKeyring = func() error {
	_, err := keyringBackend{}.Load()
	return err
}

// Open picks the backend named by mode:
//
//	"keyring" — Secret Service only, error if unreachable
//	"file"    — $XDG_CONFIG_HOME/ugh/credentials.json
//	"auto"    — keyring when reachable, otherwise file (the default)
func Open(mode string) (*Store, error) {
	path, err := DefaultFilePath()
	if err != nil {
		return nil, err
	}
	return open(mode, path)
}

func open(mode, filePath string) (*Store, error) {
	switch mode {
	case "keyring":
		if err := probeKeyring(); err != nil {
			return nil, fmt.Errorf("auth: keyring backend selected but unavailable: %w", err)
		}
		return NewStore(keyringBackend{}), nil
	case "file":
		return NewStore(fileBackend{path: filePath}), nil
	case "auto":
		if err := probeKeyring(); err == nil {
			return NewStore(keyringBackend{}), nil
		}
		return NewStore(fileBackend{path: filePath}), nil
	default:
		return nil, fmt.Errorf("auth: unknown store backend %q", mode)
	}
}
