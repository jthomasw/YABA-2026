//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package db

import "os"

// On a platform without a supported locking call the lock is a no-op rather than an
// error, so the server still starts there. The maintenance commands then lose their
// guard against running beside the server, as they had none before.
func lockFile(*os.File) (bool, error) { return false, nil }

func unlockFile(*os.File) {}

func matchOwner(string, os.FileInfo) error { return nil }
