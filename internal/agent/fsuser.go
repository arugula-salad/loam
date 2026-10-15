package agent

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// asUser runs fn with the filesystem credentials of a (nil: root, as the
// agent is), so that a file operation with user= meets the same permission
// checks that user would (docs/API.md, "Files").
//
// Linux keeps the filesystem uid and gid and the supplementary groups per
// thread, and the raw syscalls here change only the calling thread's. fn runs
// on a goroutine locked to a thread of its own that is never unlocked, so the
// runtime destroys that thread when fn returns and no other goroutine ever
// runs with these credentials. Everything fn does must stay on its goroutine.
func asUser(a *account, fn func() error) error {
	if a == nil || a.self {
		return fn()
	}
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		gids := make([]int, 0, len(a.groups))
		for _, g := range a.groups {
			gids = append(gids, int(g))
		}
		if err := unix.Setgroups(gids); err != nil {
			errc <- fmt.Errorf("setgroups: %w", err)
			return
		}
		unix.Setfsgid(int(a.gid))
		unix.Setfsuid(int(a.uid))
		// setfsuid reports the previous value and fails silently; ask again
		// (an invalid id leaves it unchanged and returns the current one).
		if cur, _ := unix.SetfsuidRetUid(-1); cur != int(a.uid) {
			errc <- fmt.Errorf("could not switch to user %s", a.name)
			return
		}
		errc <- fn()
	}()
	return <-errc
}
