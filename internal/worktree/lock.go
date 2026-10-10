package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RepoLock holds an advisory file lock on a repository's Git common directory.
type RepoLock struct {
	mu   sync.Mutex
	fd   int
	path string
}

// Unlock releases the advisory lock and closes the file descriptor.
func (l *RepoLock) Unlock() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fd < 0 {
		return nil
	}
	defer func() {
		_ = syscall.Close(l.fd)
		l.fd = -1
	}()
	return syscall.Flock(l.fd, syscall.LOCK_UN)
}

// FindGitCommonDir determines the shared Git common directory for dir.
// For standard repositories, this is dir/.git.
// For worktrees, this resolves to the shared parent common directory.
func FindGitCommonDir(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" {
			if !filepath.IsAbs(p) {
				p = filepath.Clean(filepath.Join(dir, p))
			}
			return p, nil
		}
	}

	// Fallback to searching upwards for .git
	for d := dir; d != "" && d != "/"; d = filepath.Dir(d) {
		candidate := filepath.Join(d, ".git")
		fi, statErr := os.Stat(candidate)
		if statErr == nil {
			if fi.IsDir() {
				return candidate, nil
			}
			// If .git is a file (linked worktree), read gitdir
			data, readErr := os.ReadFile(candidate)
			if readErr == nil {
				text := strings.TrimSpace(string(data))
				if strings.HasPrefix(text, "gitdir:") {
					gitDir := strings.TrimSpace(strings.TrimPrefix(text, "gitdir:"))
					if !filepath.IsAbs(gitDir) {
						gitDir = filepath.Clean(filepath.Join(d, gitDir))
					}
					// Check for commondir inside gitDir
					commonPath := filepath.Join(gitDir, "commondir")
					commonData, cErr := os.ReadFile(commonPath)
					if cErr == nil {
						cRel := strings.TrimSpace(string(commonData))
						if !filepath.IsAbs(cRel) {
							return filepath.Clean(filepath.Join(gitDir, cRel)), nil
						}
						return cRel, nil
					}
					return gitDir, nil
				}
			}
		}
	}

	return "", fmt.Errorf("git common dir not found for %s", dir)
}

// LockRepo serializes worktree lifecycle and integration operations across processes
// using an OS advisory flock on a lock file in the shared Git common directory.
// The file descriptor is explicitly opened with O_CLOEXEC and CloseOnExec to prevent
// leaking lock descriptors into spawned child processes.
func LockRepo(ctx context.Context, gitCommonDir string) (*RepoLock, error) {
	if gitCommonDir == "" {
		return nil, errors.New("git common directory required for lock")
	}
	if err := os.MkdirAll(gitCommonDir, 0o755); err != nil {
		return nil, fmt.Errorf("create git common dir: %w", err)
	}

	lockPath := filepath.Join(gitCommonDir, "ugh-worktree.lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worktree lock %s: %w", lockPath, err)
	}
	syscall.CloseOnExec(fd)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = syscall.Close(fd)
			return nil, ctx.Err()
		default:
		}

		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &RepoLock{fd: fd, path: lockPath}, nil
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("flock %s: %w", lockPath, err)
		}

		select {
		case <-ctx.Done():
			_ = syscall.Close(fd)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// WithRepoLock executes fn while holding the shared repository lock.
func WithRepoLock(ctx context.Context, gitCommonDir string, fn func() error) error {
	lock, err := LockRepo(ctx, gitCommonDir)
	if err != nil {
		return err
	}
	defer func() {
		_ = lock.Unlock()
	}()
	return fn()
}
