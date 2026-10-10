package admission

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type registeredWaiter struct {
	id          string
	path        string
	fd          int
	req         Request
	requestedAt time.Time
}

func (w *registeredWaiter) cleanup() {
	if w.fd >= 0 {
		_ = unlock(w.fd)
		_ = closeFd(w.fd)
		w.fd = -1
	}
	if w.path != "" {
		_ = os.Remove(w.path)
		w.path = ""
	}
}

// registerRootWaiter registers a crash-safe lock-backed waiter for root requests.
func registerRootWaiter(waitersDir string, req Request) (*registeredWaiter, error) {
	if err := os.MkdirAll(waitersDir, 0700); err != nil {
		return nil, fmt.Errorf("%w: failed to create waiters directory: %v", ErrStorageUnavailable, err)
	}

	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	now := time.Now()
	waiterID := fmt.Sprintf("%019d_%d_%s", now.UnixNano(), os.Getpid(), hex.EncodeToString(nonce[:]))
	path := filepath.Join(waitersDir, fmt.Sprintf("waiter_%s.lock", waiterID))

	fd, err := openLockFile(path)
	if err != nil {
		return nil, err
	}

	if err := tryLockEx(fd); err != nil {
		_ = closeFd(fd)
		_ = os.Remove(path)
		return nil, fmt.Errorf("%w: failed to lock waiter registration: %v", ErrStorageUnavailable, err)
	}

	info := WaiterDiagnostics{
		WaiterID:    waiterID,
		PID:         os.Getpid(),
		RequestID:   req.RequestID,
		Purpose:     req.Purpose,
		Model:       req.Model,
		RequestedAt: now,
	}

	raw, _ := json.Marshal(info)
	_ = syscall.Ftruncate(fd, 0)
	_, _ = syscall.Seek(fd, 0, io.SeekStart)
	_, _ = syscall.Write(fd, raw)

	return &registeredWaiter{
		id:          waiterID,
		path:        path,
		fd:          fd,
		req:         req,
		requestedAt: now,
	}, nil
}

// cleanAndListWaiters cleans up any dead waiter registrations and returns active live root waiters.
// A waiter is confirmed dead if an exclusive nonblocking flock on its lock file succeeds.
func cleanAndListWaiters(waitersDir string) ([]WaiterDiagnostics, error) {
	entries, err := os.ReadDir(waitersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: failed to read waiters directory: %v", ErrStorageUnavailable, err)
	}

	var live []WaiterDiagnostics
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "waiter_") || !strings.HasSuffix(name, ".lock") {
			continue
		}

		path := filepath.Join(waitersDir, name)
		fd, err := openLockFile(path)
		if err != nil {
			continue
		}

		lockErr := tryLockEx(fd)
		if lockErr == nil {
			// Lock succeeded: the owning process has exited or crashed.
			// Clean up this dead waiter record immediately.
			_ = unlock(fd)
			_ = closeFd(fd)
			_ = os.Remove(path)
			continue
		}

		if isLockedError(lockErr) {
			// Lock held: owning process is actively waiting.
			_ = closeFd(fd)

			var diag WaiterDiagnostics
			data, err := os.ReadFile(path)
			if err == nil {
				_ = json.Unmarshal(data, &diag)
			}
			if diag.WaiterID == "" {
				waiterID := strings.TrimSuffix(strings.TrimPrefix(name, "waiter_"), ".lock")
				diag = WaiterDiagnostics{
					WaiterID: waiterID,
					PID:      0,
				}
			}
			live = append(live, diag)
		} else {
			_ = closeFd(fd)
		}
	}

	sort.Slice(live, func(i, j int) bool {
		return live[i].WaiterID < live[j].WaiterID
	})

	return live, nil
}
