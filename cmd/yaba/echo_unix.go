//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// echoOff stops a terminal echoing what is typed, so a password prompt does
// not leave the password on the screen. It returns the function that puts the
// terminal back, or nil when f is not a terminal (piped input, or a test), in
// which case nothing was changed.
func echoOff(f *os.File) (restore func()) {
	if f == nil {
		return nil
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return nil
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
}
