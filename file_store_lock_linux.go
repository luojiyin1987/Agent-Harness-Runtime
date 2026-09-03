package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// LockExecution provides nonblocking ownership on a local Linux filesystem.
// Separate open file descriptions contend even within one process. The kernel
// releases the lock when the owning process exits; the lock file must never be
// deleted, because replacing its inode would allow two simultaneous owners.
func (s *FileStore) LockExecution(ctx context.Context, executionID string) (func(), error) {
	if err := checkStoreContext(ctx); err != nil {
		return nil, err
	}
	if executionID == "" {
		return nil, fmt.Errorf("%w: execution ID is required", ErrInvalidRequest)
	}
	file, err := os.OpenFile(s.path(executionID)+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %q", ErrExecutionBusy, executionID)
		}
		return nil, err
	}
	release := func() {
		// Closing this descriptor also releases the lock if explicit unlock fails.
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
