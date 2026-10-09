//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// echoOff stops the console echoing what is typed, so a password prompt does
// not leave the password on the screen. It returns the function that restores
// the console, or nil when f is not a console (piped input, or a test).
func echoOff(f *os.File) (restore func()) {
	if f == nil {
		return nil
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}
