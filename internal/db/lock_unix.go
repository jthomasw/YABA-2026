//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package db

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive flock without blocking. held reports that someone else
// has it, as distinct from the call failing.
func lockFile(f *os.File) (held bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func unlockFile(f *os.File) {
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// matchOwner gives path the owner and group of like. Only root can do that, and only
// root needs to: anyone else creates files they can already open.
func matchOwner(path string, like os.FileInfo) error {
	st, ok := like.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}
