//go:build !windows

package syncer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking flock on f.
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLocked
	}
	return err
}

// unlockFile releases the flock on f.
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
