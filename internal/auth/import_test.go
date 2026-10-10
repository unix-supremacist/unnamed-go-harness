package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a realistic slice of Pi's auth.json: two ugh static keys,
// unrelated static keys, unrelated oauth entries, an env-typed entry, and the
// codex OAuth entry.
const fixture = `{
  "opencode-go": {"type": "api_key", "key": "sk-oc-0123456789abcdefghijklmn"},
  "openrouter": {"type": "api_key", "key": "sk-or-v1-abcdefghij987654321"},
  "groq": {"type": "api_key", "key": "gsk_secret_secret_secret"},
  "llama.cpp": {"type": "env", "key": "LLAMA_KEY_VALUE"},
  "anthropic": {"type": "oauth", "access": "ant-access-token", "refresh": "ant-refresh-token", "expires": 1},
  "xai": {"type": "oauth", "access": "xai-access-token", "refresh": "xai-refresh-token", "expires": 2},
  "openai-codex": {"type": "oauth", "access": "codex-access-token", "refresh": "codex-refresh-token", "expires": 1767225600000, "accountId": "acct-123"}
}`

type importEnv struct {
	store  *Store
	piPath string
}

func newImportEnv(t *testing.T) importEnv {
	t.Helper()
	dir := t.TempDir()
	piPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(piPath, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	return importEnv{
		store:  NewStore(fileBackend{path: filepath.Join(dir, AppDir, FileName)}),
		piPath: piPath,
	}
}

// assertNoSecrets fails if any raw secret from the fixture leaks into output.
func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, secret := range []string{
		"sk-oc-0123456789", "sk-or-v1-abcdefghij", "gsk_secret", "codex-access-token",
		"codex-refresh-token", "ant-access-token", "xai-access-token", "LLAMA_KEY_VALUE",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks secret %q: %s", secret, out)
		}
	}
}

func TestImportPiDefaultsOnlyStaticUghKeys(t *testing.T) {
	env := newImportEnv(t)
	before, _ := os.ReadFile(env.piPath)

	res, err := ImportPi(env.store, ImportOptions{Path: env.piPath})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if len(res.Imported) != 2 {
		t.Fatalf("imported = %v, want opencode-go + openrouter", res.Imported)
	}
	if !strings.HasPrefix(res.Imported[0], "opencode-go ") || !strings.HasPrefix(res.Imported[1], "openrouter ") {
		t.Errorf("imported lines = %v", res.Imported)
	}
	for _, line := range res.Imported {
		if !strings.Contains(line, "…") {
			t.Errorf("import line not masked: %q", line)
		}
	}
	assertNoSecrets(t, strings.Join(append(append([]string{}, res.Imported...), res.Skipped...), " "))

	// Exactly the two default providers land in the store, with exact keys.
	names, creds, err := env.store.List()
	if err != nil {
		t.Fatal(err)
	}
	// The shared "opencode" entry is kept in step with the opencode-go import.
	if len(names) != 3 || names[0] != "opencode" || names[1] != "opencode-go" || names[2] != "openrouter" {
		t.Fatalf("store names = %v", names)
	}
	if creds["opencode-go"].Key != "sk-oc-0123456789abcdefghijklmn" {
		t.Error("opencode-go key mismatch")
	}
	if creds["opencode"].Key != creds["opencode-go"].Key {
		t.Error("shared opencode key mismatch")
	}
	if creds["openrouter"].Key != "sk-or-v1-abcdefghij987654321" {
		t.Error("openrouter key mismatch")
	}

	// Skipped entries are reported by name only and include codex + the rest.
	for _, want := range []string{"anthropic", "groq", "llama.cpp", "openai-codex", "xai"} {
		found := false
		for _, s := range res.Skipped {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("skipped = %v, missing %s", res.Skipped, want)
		}
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "ugh login codex") {
		t.Errorf("notes = %v, want the independent-session hint", res.Notes)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, codex was not imported", res.Warnings)
	}

	// Pi's file is never modified.
	after, _ := os.ReadFile(env.piPath)
	if string(before) != string(after) {
		t.Error("Pi's auth.json was modified")
	}
}

func TestImportPiNoOverwriteWithoutForce(t *testing.T) {
	env := newImportEnv(t)
	if _, err := ImportPi(env.store, ImportOptions{Path: env.piPath}); err != nil {
		t.Fatal(err)
	}
	// User replaces the stored key by hand, then re-imports.
	hand := Credential{Type: "api_key", Key: "hand-set-key-1234567890"}
	if err := env.store.Put("openrouter", hand); err != nil {
		t.Fatal(err)
	}

	res, err := ImportPi(env.store, ImportOptions{Path: env.piPath})
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := env.store.Get("openrouter")
	if got.Key != hand.Key {
		t.Error("existing entry overwritten without --force")
	}
	if len(res.Imported) != 0 {
		t.Errorf("imported without --force: %v", res.Imported)
	}

	res, err = ImportPi(env.store, ImportOptions{Path: env.piPath, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ = env.store.Get("openrouter")
	if got.Key != "sk-or-v1-abcdefghij987654321" {
		t.Error("--force did not overwrite")
	}
	if len(res.Imported) != 2 {
		t.Errorf("--force imported = %v", res.Imported)
	}
}

func TestImportPiCodexFlag(t *testing.T) {
	env := newImportEnv(t)
	res, err := ImportPi(env.store, ImportOptions{Path: env.piPath, Codex: true})
	if err != nil {
		t.Fatal(err)
	}
	cred, ok, err := env.store.Get("codex")
	if err != nil || !ok {
		t.Fatalf("codex not imported: ok=%v err=%v", ok, err)
	}
	if cred.Type != "oauth" || cred.Access != "codex-access-token" || cred.Refresh != "codex-refresh-token" {
		t.Errorf("codex credential = %+v", cred)
	}
	if cred.AccountID != "acct-123" || cred.Expires != 1767225600000 {
		t.Errorf("accountId/expires not mapped: %+v", cred)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "re-login") {
		t.Errorf("warnings = %v, want the Pi re-login warning", res.Warnings)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, codex was imported", res.Notes)
	}
	assertNoSecrets(t, strings.Join(res.Imported, " "))
}

func TestImportPiOnlyFilterAndErrors(t *testing.T) {
	env := newImportEnv(t)
	res, err := ImportPi(env.store, ImportOptions{Path: env.piPath, Only: []string{"openrouter"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Imported) != 1 || !strings.HasPrefix(res.Imported[0], "openrouter ") {
		t.Errorf("--only openrouter imported = %v", res.Imported)
	}
	if _, ok, _ := env.store.Get("opencode-go"); ok {
		t.Error("--only openrouter still imported opencode-go")
	}

	if _, err := ImportPi(env.store, ImportOptions{Path: env.piPath, Only: []string{"groq"}}); err == nil {
		t.Error("unknown --only provider accepted")
	}
	if _, err := ImportPi(env.store, ImportOptions{Path: env.piPath, Only: []string{"codex"}}); err == nil {
		t.Error("--only codex without --codex accepted")
	}
	if _, err := ImportPi(env.store, ImportOptions{Path: filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Error("missing Pi auth.json accepted")
	}
}
