package auth

// Path and permission helpers are salvaged from old/internal/auth/store.go by
// copy-and-trim: ConfigPath, checkDir and owned stay essentially as they were,
// while the old lock/JSON machinery was left behind. Writes are atomic
// (temp file + fsync + rename) and loads refuse group/world-accessible files.

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	AppDir   = "ugh"
	FileName = "credentials.json"
	maxFile  = 64 * 1024
)

// ConfigPath resolves the credentials file from an explicit environment
// snapshot. An absolute XDG_CONFIG_HOME takes precedence; otherwise
// HOME/.config is used.
func ConfigPath(home, xdgConfigHome string) (string, error) {
	base := xdgConfigHome
	if !filepath.IsAbs(base) {
		if !filepath.IsAbs(home) {
			return "", errors.New("auth: absolute HOME required")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, AppDir, FileName), nil
}

// DefaultFilePath resolves the credentials file from the current environment.
func DefaultFilePath() (string, error) {
	return ConfigPath(os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME"))
}

// checkDir creates missing path components without following symlinks. The
// application directory must be owned by this user and exactly 0700.
func checkDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("auth: absolute clean store path required")
	}
	cur := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(path, cur), cur) {
		cur = filepath.Join(cur, part)
		st, e := os.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) {
			if e = os.Mkdir(cur, 0700); e != nil && !errors.Is(e, os.ErrExist) {
				return errors.New("auth: create directory failed")
			}
			st, e = os.Lstat(cur)
		}
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("auth: unsafe directory component")
		}
	}
	st, e := os.Lstat(path)
	if e != nil || st.Mode().Perm() != 0700 || !owned(st) {
		return errors.New("auth: application directory must be private (0700) and owned by current user")
	}
	return nil
}

func owned(st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}

// fileBackend stores the document at $XDG_CONFIG_HOME/ugh/credentials.json
// (dir 0700, file 0600, checked on load, atomic writes).
type fileBackend struct{ path string }

func (f fileBackend) Name() string { return "file" }

// Path exposes the store location for `ugh auth list`.
func (f fileBackend) Path() string { return f.path }

func (f fileBackend) checkPath() error {
	if !filepath.IsAbs(f.path) || filepath.Clean(f.path) != f.path || filepath.Base(f.path) != FileName {
		return errors.New("auth: absolute canonical credentials path required")
	}
	return nil
}

func (f fileBackend) Load() (*Credentials, error) {
	if err := f.checkPath(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(f.path)
	// The directory must exist and be private whenever anything is stored.
	// (If neither dir nor file exist yet, the store is simply empty.)
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 || !owned(st) {
			return nil, errors.New("auth: credentials directory must be private (0700) and owned by current user")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("auth: credentials directory cannot be inspected")
	}
	// O_NOFOLLOW refuses symlink aliases; O_NONBLOCK avoids FIFO hangs.
	fd, e := syscall.Open(f.path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e == syscall.ENOENT {
		return newCredentials(), nil
	}
	if e != nil {
		return nil, errors.New("auth: credentials cannot be opened safely")
	}
	file := os.NewFile(uintptr(fd), f.path)
	defer file.Close()
	st, e := file.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || !owned(st) || st.Size() > maxFile {
		return nil, errors.New("auth: credentials file is unsafe (must be 0600, regular, owned, ungrouped/unworlded)")
	}
	raw, e := io.ReadAll(io.LimitReader(file, maxFile+1))
	if e != nil || len(raw) > maxFile {
		return nil, errors.New("auth: credentials read failed")
	}
	if len(raw) == 0 {
		return newCredentials(), nil
	}
	var c Credentials
	// Parser errors never echo the file contents (it holds secrets).
	if e := json.Unmarshal(raw, &c); e != nil {
		return nil, errors.New("auth: credentials file is corrupt")
	}
	if c.Version != 1 {
		return nil, errors.New("auth: credentials version unsupported")
	}
	if c.Providers == nil {
		c.Providers = map[string]Credential{}
	}
	for name, cred := range c.Providers {
		if cred.Type == "api_key" && checkKey(cred.Key) != nil {
			return nil, errors.New("auth: stored key invalid")
		}
		if name == "" {
			return nil, errors.New("auth: credentials file is corrupt")
		}
	}
	return &c, nil
}

func (f fileBackend) Save(c *Credentials) error {
	if err := f.checkPath(); err != nil {
		return err
	}
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Providers == nil {
		c.Providers = map[string]Credential{}
	}
	dir := filepath.Dir(f.path)
	if e := checkDir(dir); e != nil {
		return e
	}
	raw, e := json.Marshal(c)
	if e != nil || len(raw)+1 > maxFile {
		return errors.New("auth: credentials encoding failed")
	}
	raw = append(raw, '\n')
	tmp, e := os.CreateTemp(dir, ".credentials-*")
	if e != nil {
		return errors.New("auth: staging failed")
	}
	defer os.Remove(tmp.Name())
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(raw)
	}
	if e == nil {
		e = tmp.Sync()
	}
	if closeErr := tmp.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return errors.New("auth: staging failed")
	}
	if e = os.Rename(tmp.Name(), f.path); e != nil {
		return errors.New("auth: replacement failed")
	}
	df, e := os.Open(dir)
	if e != nil {
		return errors.New("auth: directory sync failed; replacement outcome uncertain")
	}
	e = df.Sync()
	_ = df.Close()
	if e != nil {
		return errors.New("auth: directory sync failed; replacement outcome uncertain")
	}
	return nil
}
