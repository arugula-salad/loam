package agent

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair sized cols×rows: the master, which
// the agent reads and writes, and the terminal the command gets. It is the
// few ioctls a pty needs, which isn't worth a dependency.
func openPTY(cols, rows uint16) (ptmx, tty *os.File, err error) {
	ptmx, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n int
	err = control(ptmx, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return fmt.Errorf("unlockpt: %w", err)
		}
		var err error
		if n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN); err != nil {
			return fmt.Errorf("ptsname: %w", err)
		}
		return nil
	})
	if err != nil {
		ptmx.Close()
		return nil, nil, err
	}
	tty, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, err
	}
	setWinsize(ptmx, cols, rows)
	return ptmx, tty, nil
}

func setWinsize(f *os.File, cols, rows uint16) error {
	return control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	})
}

// control runs fn on f's descriptor without f.Fd(), which would put the
// master back in blocking mode: a read blocked there could then never be
// interrupted by closing it.
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}
