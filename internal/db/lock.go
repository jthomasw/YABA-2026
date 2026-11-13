package db

import (
	"errors"
	"fmt"
	"os"
)

// ErrLocked means another process holds the database's lock: normally the running
// server, sometimes a second maintenance command.
var ErrLocked = errors.New("the database is in use by another process")

// Lock is an exclusive, advisory, whole-process hold on a database, taken by the
// server for its lifetime and by the maintenance commands that replace or rewrite the
// database underneath it (restore, reset, repair).
//
// Why a lock file rather than asking SQLite. In WAL mode an idle server connection
// holds no lock on the database file at all -- only a shared lock on a byte of the
// -shm -- so neither BEGIN EXCLUSIVE nor locking_mode=EXCLUSIVE can see that a server
// is running between requests; they would succeed, and the server's next write would
// land in a file restore had already moved aside. A separate lock file avoids all of
// that, and is reliable on both platforms we run on:
//
//   - Linux/macOS use flock(2). The kernel drops it when the process exits for any
//     reason, kill -9 and OOM included, so there is never a stale lock to clean up.
//     It is per open file, so it also excludes a second holder in the same process,
//     unlike POSIX fcntl locks, which SQLite uses on the database file itself and
//     which this deliberately does not touch.
//   - Windows uses LockFileEx, which has the same released-on-exit behaviour.
//
// The file itself is never deleted (unlinking a lock file races with the next
// opener) and is empty. It is opened read-only, which both APIs accept, and created
// 0644, so a lock file first created by root through `sudo yaba restore` can still be
// locked by the service account afterwards.
type Lock struct {
	f    *os.File
	path string
}

// LockPath is the lock file for the database at dbPath.
func LockPath(dbPath string) string { return dbPath + ".lock" }

// AcquireLock takes the database's lock without waiting, and returns an error
// wrapping ErrLocked if another process holds it. Release it when done; the operating
// system releases it anyway if the process dies.
func AcquireLock(dbPath string) (*Lock, error) {
	path := LockPath(dbPath)
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	held, err := lockFile(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if held {
		f.Close()
		return nil, fmt.Errorf("%w (%s is locked)", ErrLocked, path)
	}
	return &Lock{f: f, path: path}, nil
}

// Release gives the lock up. It is safe to call more than once, and on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockFile(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
