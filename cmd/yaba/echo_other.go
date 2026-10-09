//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package main

import "os"

// echoOff is unsupported here: the password is read visibly.
func echoOff(*os.File) func() { return nil }
