package perm

import (
	"os"
	"regexp"
	"strings"
	"sync"
)

// scrub.go ports pisup's scrub (PLAN.md §4.3) plus the loaded-secret
// registry from §3.1: secrets never appear in decision logs, prompts, session
// files or classifier state.

var (
	secretNameRx = regexp.MustCompile(`(KEY|TOKEN|SECRET|PASSW|AUTH|CREDENTIAL)`)
	bearerRx     = regexp.MustCompile(`(?i)\bBearer\s+[^\s'";,]+`)
	kvRx         = regexp.MustCompile(`(?i)\b([\w.-]*(?:KEY|TOKEN|SECRET|PASSW|AUTH|CREDENTIAL)[\w.-]*)\s*([=:])\s*([^\s,;]+)`)
	// .env-style lines are redacted to the end of the line only when the
	// name looks secret: a shell variable like JAR=/x.jar must stay visible
	// (blanking it hid ~10% of commands from the classifiers).
	envLineRx   = regexp.MustCompile(`(?i)(^|\n)([\w.-]*(?:KEY|TOKEN|SECRET|PASSW|AUTH|CREDENTIAL)[\w.-]*\s*=\s*)[^\r\n]+`)
	quotedKeyRx = regexp.MustCompile(`(?i)"(authorization|x-api-key|api-key|token)"\s*:\s*"[^"]*"`)
)

var (
	secretsMu sync.Mutex
	secrets   []string // exact secret values loaded at startup (≥6 bytes)
	envSecret []string // values of env vars whose names look secret
	envOnce   sync.Once
)

// RegisterSecret adds a loaded secret value to the redaction list. Values
// shorter than 6 bytes are ignored (too likely to match by accident).
func RegisterSecret(v string) {
	if len(v) < 6 {
		return
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	for _, s := range secrets {
		if s == v {
			return
		}
	}
	secrets = append(secrets, v)
}

func envSecrets() []string {
	envOnce.Do(func() {
		for _, kv := range os.Environ() {
			name, value, ok := strings.Cut(kv, "=")
			if ok && secretNameRx.MatchString(name) && len(value) >= 4 {
				envSecret = append(envSecret, value)
			}
		}
	})
	return envSecret
}

// Scrub redacts secret-looking material from s.
func Scrub(s string) string {
	if s == "" {
		return s
	}
	for _, v := range envSecrets() {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	secretsMu.Lock()
	for _, v := range secrets {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	secretsMu.Unlock()
	s = bearerRx.ReplaceAllString(s, "Bearer [REDACTED]")
	s = quotedKeyRx.ReplaceAllString(s, `"$1": "[REDACTED]"`)
	s = kvRx.ReplaceAllString(s, "$1$2[REDACTED]")
	s = envLineRx.ReplaceAllString(s, "${1}${2}[REDACTED]")
	return s
}
