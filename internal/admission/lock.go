package admission

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"
)

// openLockFile opens or creates a lock file with O_CLOEXEC and sets CloseOnExec.
func openLockFile(path string) (int, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return -1, fmt.Errorf("%w: failed to open lock file %s: %v", ErrStorageUnavailable, filepath.Base(path), err)
	}
	syscall.CloseOnExec(fd)
	return fd, nil
}

// tryLockEx attempts a non-blocking exclusive advisory flock.
func tryLockEx(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlock releases an advisory lock.
func unlock(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_UN)
}

// closeFd closes a file descriptor safely.
func closeFd(fd int) error {
	return syscall.Close(fd)
}

// isLockedError checks if an error indicates that an advisory lock is held by another process.
func isLockedError(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// lockAdmin acquires the brief serialization lock for an account.
// It spins with 5ms backoff until acquired or context is cancelled.
func lockAdmin(ctx context.Context, accountDir string) (func(), error) {
	path := filepath.Join(accountDir, "admin.lock")
	fd, err := openLockFile(path)
	if err != nil {
		return nil, err
	}

	for {
		err := tryLockEx(fd)
		if err == nil {
			return func() {
				_ = unlock(fd)
				_ = closeFd(fd)
			}, nil
		}
		if !isLockedError(err) {
			_ = closeFd(fd)
			return nil, fmt.Errorf("%w: admin lock failed: %v", ErrStorageUnavailable, err)
		}

		select {
		case <-ctx.Done():
			_ = closeFd(fd)
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
