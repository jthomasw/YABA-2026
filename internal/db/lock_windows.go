//go:build windows

package db

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on the file's first byte without blocking. held
// reports that someone else has it, as distinct from the call failing. Locking past
// the end of an empty file is allowed.
func lockFile(f *os.File) (held bool, err error) {
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}

func unlockFile(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// matchOwner is a no-op: Windows files inherit their ACL from the directory, which is
// what gives the service account access.
func matchOwner(string, os.FileInfo) error { return nil }
