//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// onResize calls f whenever the terminal changes size, until stop is called.
func onResize(f func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				f()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// forwardedSignals are the local signals a non-TTY exec passes to the remote
// process group, by their `kill -l` names.
var forwardedSignals = map[os.Signal]string{
	syscall.SIGINT:  "INT",
	syscall.SIGTERM: "TERM",
	syscall.SIGHUP:  "HUP",
	syscall.SIGQUIT: "QUIT",
}
