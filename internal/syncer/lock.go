package syncer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"backuprepo/internal/apperr"
)

// ErrBusy means another bb process holds the sync lock.
var ErrBusy = fmt.Errorf("%w: another bb process is syncing right now", apperr.ErrSync)

// LockName is the lock file in bb's state folder (~/backup_repo).
const LockName = "sync.lock"

// Lock takes the cross-process sync lock in dir (~/backup_repo) without
// waiting, so only one process — the daemon, `bb sync`, `bb upload` or the web
// UI — syncs at a time and two of them can't both upload the same new file.
// The OS releases the lock when the process exits, so a crash leaves no stale
// lock. It returns ErrBusy when another process holds it; call unlock when
// done.
func Lock(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open lock: %v", apperr.ErrSync, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("%w: lock: %v", apperr.ErrSync, err)
	}
	return func() { _ = unlockFile(f); f.Close() }, nil
}

// errLocked is returned by lockFile when the lock is held elsewhere.
var errLocked = errors.New("locked")
