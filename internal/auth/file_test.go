package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func testStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), AppDir, FileName)
}

func TestFileRoundtripAndPermissions(t *testing.T) {
	f := fileBackend{path: testStorePath(t)}
	// Nothing stored yet: empty, and no directory created by a read.
	c, err := f.Load()
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(c.Providers) != 0 {
		t.Fatalf("expected empty store, got %d", len(c.Providers))
	}

	want := Credential{Type: "api_key", Key: "sk-or-v1-abcdef0123456789"}
	c.Providers["openrouter"] = want
	if err := f.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	dir := filepath.Dir(f.path)
	st, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("dir stat: %v", err)
	}
	if st.Mode().Perm() != 0700 {
		t.Errorf("dir perm = %o, want 0700", st.Mode().Perm())
	}
	st, err = os.Lstat(f.path)
	if err != nil {
		t.Fatalf("file stat: %v", err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("file perm = %o, want 0600", st.Mode().Perm())
	}
	// Atomic write leaves no staging files behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != FileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only %s", names, FileName)
	}

	got, err := f.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Providers["openrouter"] != want {
		t.Errorf("roundtrip mismatch: %+v", got.Providers["openrouter"])
	}
}

func TestFileRefusesGroupWorldAccessible(t *testing.T) {
	f := fileBackend{path: testStorePath(t)}
	c := newCredentials()
	c.Providers["openrouter"] = Credential{Type: "api_key", Key: "sk-or-v1-abcdef0123456789"}
	if err := f.Save(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := os.Chmod(f.path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); err == nil {
		t.Error("group-readable file was accepted")
	}

	if err := os.Chmod(f.path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(f.path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); err == nil {
		t.Error("world-searchable directory was accepted")
	}
}

func TestFileRefusesSymlinkAndCorruption(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(`{"version":1,"providers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(dir, AppDir)
	if err := os.Mkdir(appDir, 0700); err != nil {
		t.Fatal(err)
	}
	f := fileBackend{path: filepath.Join(appDir, FileName)}
	if err := os.Symlink(real, f.path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); err == nil {
		t.Error("symlinked credentials file was accepted")
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); err == nil {
		t.Error("corrupt credentials file was accepted")
	}
}
