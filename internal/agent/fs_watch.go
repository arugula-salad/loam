package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// Filesystem watch: a WebSocket that reports inotify events under one path
// as {"type","path"} text frames. inotify watches directories one at a time,
// so a recursive watch is a watch per directory, extended as the tree grows.
//
// The events are not activity: they only say that something in the guest,
// which is accounted for on its own terms, touched the disk.

// The kernel allows root some 15k inotify watches in a 2 GB guest; leave most
// of that for the sandbox's own tools. (A variable so that tests can lower it.)
var maxWatchDirs int64 = 8192

const (
	maxWatchConns = 32
	// watchQueue bounds what a slow reader can make us hold. Past it the
	// socket is closed (1008): a watcher that silently lost events would
	// believe a stale picture of the disk.
	watchQueue = 1024
	watchPing  = 30 * time.Second
)

var watchDirsInUse, watchConnsInUse atomic.Int64

type fsWatcher struct {
	w        *fsnotify.Watcher
	out      chan api.WatchEvent
	overflow chan struct{}
	once     sync.Once

	mu   sync.Mutex
	dirs map[string]watchedDir
	file string // a file watched by name, through its parent

	lastRename string // pump goroutine only; see handle
}

type watchedDir struct {
	recursive bool
	// all is false for a directory watched only for the sake of the file
	// inside it: its other children are not reported.
	all bool
}

func (s *Server) fsWatch(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "watch is a WebSocket")
		return
	}
	p, a, err := s.fsCall(r)
	if err != nil {
		fsFail(w, err)
		return
	}
	recursive, err := parseBool(r.URL.Query().Get("recursive"), false)
	if err != nil {
		fsFail(w, invalidf("recursive must be true or false"))
		return
	}
	if watchConnsInUse.Add(1) > maxWatchConns {
		watchConnsInUse.Add(-1)
		writeErr(w, http.StatusConflict, api.CodeConflict, fmt.Sprintf("at most %d concurrent filesystem watches", maxWatchConns))
		return
	}
	defer watchConnsInUse.Add(-1)
	nw, err := fsnotify.NewWatcher()
	if err != nil {
		fsFail(w, err)
		return
	}
	fw := &fsWatcher{w: nw, out: make(chan api.WatchEvent, watchQueue), overflow: make(chan struct{}),
		dirs: map[string]watchedDir{}}
	defer fw.close()
	// The watch is placed as the caller (so it sees only what it may), and
	// before the upgrade, so a path that can't be watched is an HTTP error.
	err = asUser(a, func() error {
		real, err := filepath.EvalSymlinks(p) // walking does not follow symlinks
		if err != nil {
			return err
		}
		info, err := os.Stat(real)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fw.addTree(real, recursive, false)
		}
		// Watch the parent, not the inode: editors replace files by rename,
		// which would silently end a watch on the file itself.
		fw.file = real
		_, err = fw.addDir(filepath.Dir(real), watchedDir{})
		return err
	})
	if err != nil {
		if errors.Is(err, errWatchLimit) {
			writeErr(w, http.StatusConflict, api.CodeConflict, err.Error())
			return
		}
		fsFail(w, err)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)
	go fw.pump(done)
	go fw.write(conn, done)

	// A client that vanished without closing is noticed by pings going unanswered.
	conn.SetReadLimit(1 << 16)
	conn.SetReadDeadline(time.Now().Add(3 * watchPing))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(3 * watchPing)) })
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (fw *fsWatcher) close() {
	fw.w.Close()
	fw.mu.Lock()
	watchDirsInUse.Add(-int64(len(fw.dirs)))
	fw.dirs = map[string]watchedDir{}
	fw.mu.Unlock()
}

// send never blocks: the inotify reader must keep draining whatever the client does.
func (fw *fsWatcher) send(m api.WatchEvent) {
	select {
	case fw.out <- m:
	default:
		fw.once.Do(func() { close(fw.overflow) })
	}
}

func (fw *fsWatcher) write(conn *websocket.Conn, done <-chan struct{}) {
	ping := time.NewTicker(watchPing)
	defer ping.Stop()
	defer conn.Close() // unblocks the reader, which owns the cleanup
	for {
		var err error
		select {
		case <-done:
			return
		case <-fw.overflow:
			closeWS(conn, websocket.ClosePolicyViolation, "client too slow: events were dropped")
			return
		case m := <-fw.out:
			conn.SetWriteDeadline(time.Now().Add(watchPing))
			err = conn.WriteJSON(m)
		case <-ping.C:
			err = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(watchPing))
		}
		if err != nil {
			return
		}
	}
}

var errWatchLimit = errors.New("watch limit reached (directories, across all watchers); not watching")

// addDir watches one directory, or widens what an existing watch reports.
func (fw *fsWatcher) addDir(dir string, want watchedDir) (added bool, err error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if have, known := fw.dirs[dir]; known {
		fw.dirs[dir] = watchedDir{recursive: have.recursive || want.recursive, all: have.all || want.all}
		return false, nil
	}
	if watchDirsInUse.Add(1) > maxWatchDirs {
		watchDirsInUse.Add(-1)
		return false, errWatchLimit
	}
	if err := fw.w.Add(dir); err != nil {
		watchDirsInUse.Add(-1)
		return false, err
	}
	fw.dirs[dir] = want
	return true, nil
}

// addTree watches dir and, if recursive, everything under it. With announce it
// also reports what it finds as created: that is for a directory that appeared
// in a watched tree, whose first contents can beat the watch being placed.
//
// It is all or nothing: a tree too big for the watch budget is not left half
// watched, holding the budget and reporting an arbitrary part of itself.
func (fw *fsWatcher) addTree(dir string, recursive, announce bool) error {
	if !recursive {
		_, err := fw.addDir(dir, watchedDir{all: true})
		return err
	}
	var added []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // vanished or unreadable while walking: nothing to watch
		}
		if announce && p != dir {
			fw.event(p, "create")
		}
		if !d.IsDir() {
			return nil
		}
		ok, err := fw.addDir(p, watchedDir{recursive: true, all: true})
		if ok {
			added = append(added, p)
		}
		return err
	})
	if err != nil {
		fw.mu.Lock()
		for _, d := range added {
			fw.w.Remove(d)
			delete(fw.dirs, d)
		}
		watchDirsInUse.Add(-int64(len(added)))
		fw.mu.Unlock()
	}
	return err
}

// forget drops bookkeeping for a directory that was removed or renamed away,
// and everything under it. inotify watches follow the inode, so after a rename
// the descendants' watches would report paths that no longer exist.
func (fw *fsWatcher) forget(dir string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for d := range fw.dirs {
		if d == dir || strings.HasPrefix(d, dir+"/") {
			fw.w.Remove(d) // already gone if the directory was deleted
			delete(fw.dirs, d)
			watchDirsInUse.Add(-1)
		}
	}
}

func (fw *fsWatcher) pump(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case _, ok := <-fw.w.Errors:
			if !ok {
				return
			}
			// The kernel's queue overflowed: events are lost, as for a slow client.
			fw.once.Do(func() { close(fw.overflow) })
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			fw.handle(ev)
		}
	}
}

func (fw *fsWatcher) handle(ev fsnotify.Event) {
	fw.mu.Lock()
	self, wasDir := fw.dirs[ev.Name]
	parent := fw.dirs[filepath.Dir(ev.Name)]
	wanted := parent.all || self.all || ev.Name == fw.file
	fw.mu.Unlock()

	if wasDir && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
		fw.forget(ev.Name)
	}
	if !wanted {
		return
	}
	// A watched directory that moves is reported twice, by its parent's watch and
	// by its own. Nothing can rename the same path twice in a row without a
	// create in between, so an immediate repeat is that echo.
	if ev.Op == fsnotify.Rename && ev.Name == fw.lastRename {
		fw.lastRename = ""
		return
	}
	fw.lastRename = ""
	if ev.Has(fsnotify.Rename) {
		fw.lastRename = ev.Name
	}
	for _, op := range []fsnotify.Op{fsnotify.Create, fsnotify.Write, fsnotify.Remove, fsnotify.Rename, fsnotify.Chmod} {
		if ev.Has(op) {
			fw.event(ev.Name, strings.ToLower(op.String()))
		}
	}
	if parent.recursive && ev.Has(fsnotify.Create) {
		if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
			// A tree too big for the budget is simply not watched further;
			// what was already watched keeps reporting.
			fw.addTree(ev.Name, true, true)
		}
	}
}

func (fw *fsWatcher) event(path, op string) {
	fw.send(api.WatchEvent{Type: op, Path: path})
}
