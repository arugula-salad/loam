//go:build !unix

package main

import "os"

// onResize is a no-op without SIGWINCH; the size is sent once at the start.
func onResize(func()) (stop func()) { return func() {} }

var forwardedSignals = map[os.Signal]string{os.Interrupt: "INT"}
