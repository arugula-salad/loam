package agent

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

// Ports are found by polling /proc/net/tcp{,6}: there is no kernel event for
// "a socket entered LISTEN" that works without extra privileges or modules.
// A tick reads those two files; the expensive part, finding which process
// owns a socket (a walk over every /proc/<pid>/fd), happens only for a socket
// not seen on the previous tick. Nothing polls while nobody watches, and
// watching is not activity: a sandbox must not stay awake just because it is
// being watched.
const portPollInterval = 500 * time.Millisecond

// portWait is how long /ports/{port}/connect waits for a listener after it
// has just started the service that owns the port. Otherwise it doesn't wait:
// loam-api retries a closed port for up to 10 s itself (docs/API.md, "Ports"),
// and two waits would compound. A variable so that tests can shorten it.
var portWait = 10 * time.Second

// listener is one TCP socket in LISTEN.
type listener struct {
	addr  string // the bound IP, e.g. "0.0.0.0", "127.0.0.1", "::"
	port  int
	inode uint64
}

func (l listener) key() string { return net.JoinHostPort(l.addr, strconv.Itoa(l.port)) }

type openPort struct {
	listener
	pid  int      // 0 if the owner could not be found
	sess *Session // the exec session the owner belongs to, if any
}

func (p openPort) api() api.Port {
	out := api.Port{Port: p.port, Address: p.addr, PID: p.pid}
	if p.sess != nil {
		out.Session = p.sess.ID
	}
	return out
}

// portEvent is one change, for /ports/watch.
type portEvent struct {
	typ  string // opened, closed
	port api.Port
}

type portWatcher struct {
	mgr *Manager

	mu   sync.Mutex
	subs map[chan portEvent]struct{}
	open map[string]openPort // by listener.key(), while anyone watches
	stop chan struct{}
}

// subscribe starts notifications on a channel: the ports already open, then
// changes. A subscriber that falls behind by more than the channel holds is
// dropped (its channel closed), rather than stalling the others.
func (w *portWatcher) subscribe() (chan portEvent, []api.Port) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.subs) == 0 {
		w.subs, w.open = map[chan portEvent]struct{}{}, map[string]openPort{}
		w.scanLocked()
		w.stop = make(chan struct{})
		go w.poll(w.stop)
	}
	ch := make(chan portEvent, 256)
	w.subs[ch] = struct{}{}
	return ch, w.sortedLocked()
}

func (w *portWatcher) unsubscribe(ch chan portEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.subs[ch]; !ok {
		return
	}
	delete(w.subs, ch)
	if len(w.subs) == 0 {
		close(w.stop)
	}
}

// list is the ports open now: the watcher's view if it runs, else a scan.
func (w *portWatcher) list() []api.Port {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.subs) > 0 {
		return w.sortedLocked()
	}
	saved := w.open
	w.open = map[string]openPort{}
	w.scanLocked()
	out := w.sortedLocked()
	w.open = saved
	return out
}

func (w *portWatcher) poll(stop chan struct{}) {
	tick := time.NewTicker(portPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		w.mu.Lock()
		select {
		case <-stop: // the last subscriber left while we waited for the lock
		default:
			w.scanLocked()
		}
		w.mu.Unlock()
	}
}

func (w *portWatcher) sortedLocked() []api.Port {
	out := make([]api.Port, 0, len(w.open))
	for _, p := range w.open {
		out = append(out, p.api())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Port < out[j].Port || out[i].Port == out[j].Port && out[i].Address < out[j].Address
	})
	return out
}

// scanLocked diffs the listening sockets against the last scan and notifies subscribers.
func (w *portWatcher) scanLocked() {
	now := readListeners()
	for key, p := range w.open {
		// A different inode on the same address is a new socket: the old one closed.
		if l, ok := now[key]; !ok || l.inode != p.inode {
			delete(w.open, key)
			w.notifyLocked("closed", p)
		}
	}
	fresh := map[uint64]bool{}
	for key, l := range now {
		if _, ok := w.open[key]; !ok {
			fresh[l.inode] = true
		}
	}
	if len(fresh) == 0 {
		return
	}
	owners := socketOwners(fresh)
	for key, l := range now {
		if !fresh[l.inode] {
			continue
		}
		p := openPort{listener: l, pid: owners[l.inode]}
		if p.pid != 0 {
			p.sess = w.mgr.sessionOf(p.pid)
		}
		w.open[key] = p
		w.notifyLocked("opened", p)
	}
}

func (w *portWatcher) notifyLocked(typ string, p openPort) {
	for ch := range w.subs {
		select {
		case ch <- portEvent{typ, p.api()}:
		default:
			delete(w.subs, ch)
			close(ch)
			if len(w.subs) == 0 {
				close(w.stop)
			}
		}
	}
}

// readListeners returns the TCP sockets in LISTEN, keyed by address.
func readListeners() map[string]listener {
	out := map[string]listener{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		for _, l := range parseListeners(f) {
			// With SO_REUSEPORT several sockets share an address; one entry stands for them.
			if _, dup := out[l.key()]; !dup {
				out[l.key()] = l
			}
		}
		f.Close()
	}
	return out
}

// parseListeners reads one /proc/net/tcp{,6} file.
func parseListeners(r io.Reader) []listener {
	var out []listener
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode ...
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		ip, port, ok := parseProcAddr(fields[1])
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if !ok || err != nil {
			continue
		}
		out = append(out, listener{addr: ip.String(), port: port, inode: inode})
	}
	return out
}

// parseProcAddr decodes "0100007F:1F90": the address is hex in host byte
// order, one 32-bit word for IPv4 and four for IPv6.
func parseProcAddr(s string) (net.IP, int, bool) {
	host, portHex, ok := strings.Cut(s, ":")
	raw, err := hex.DecodeString(host)
	port, perr := strconv.ParseUint(portHex, 16, 16)
	if !ok || err != nil || perr != nil || len(raw)%4 != 0 {
		return nil, 0, false
	}
	for i := 0; i < len(raw); i += 4 { // little-endian words, on every arch we run on
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return net.IP(raw), int(port), true
}

// socketOwners finds, for each wanted socket inode, the lowest pid holding it
// open: the parent, when a server has forked workers that share its listener.
// It needs to read every process's fds, which root in its own VM can.
func socketOwners(want map[uint64]bool) map[uint64]int {
	owners := map[uint64]int{}
	entries, _ := os.ReadDir("/proc")
	pids := []int{}
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if len(owners) == len(want) {
			break
		}
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		fds, _ := os.ReadDir(dir)
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, _ := strconv.ParseUint(link[len("socket:["):len(link)-1], 10, 64)
			if _, found := owners[inode]; want[inode] && !found {
				owners[inode] = pid
			}
		}
	}
	return owners
}

// sessionOf finds the exec session a process belongs to: the one whose command
// leads its process group or session, or is an ancestor. A daemon that both
// double-forks and calls setsid escapes this, and is reported without one.
func (m *Manager) sessionOf(pid int) *Session {
	m.mu.Lock()
	roots := map[int]*Session{}
	for _, s := range m.sessions {
		roots[s.PID] = s
	}
	m.mu.Unlock()
	for depth := 0; pid > 1 && depth < 64; depth++ {
		ppid, pgrp, sid, ok := procStat(pid)
		if !ok {
			return nil
		}
		for _, id := range []int{pid, pgrp, sid} {
			if s := roots[id]; s != nil && !s.hasExited() {
				return s
			}
		}
		pid = ppid
	}
	return nil
}

// procStat reads a process's parent, process group and session from /proc/<pid>/stat.
func procStat(pid int) (ppid, pgrp, sid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, 0, false
	}
	// "pid (comm) state ppid pgrp session ...", and comm may itself contain ") ".
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 4 {
		return 0, 0, 0, false
	}
	ppid, _ = strconv.Atoi(fields[1])
	pgrp, _ = strconv.Atoi(fields[2])
	sid, _ = strconv.Atoi(fields[3])
	return ppid, pgrp, sid, true
}

func (s *Server) registerPorts(mux *http.ServeMux) {
	mux.HandleFunc("GET /ports", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.PortList{Ports: s.ports.list()})
	})
	mux.HandleFunc("GET /ports/watch", s.handlePortsWatch)
	mux.HandleFunc("GET /ports/{port}/connect", s.handlePortConnect)
}

// handlePortsWatch is Server-Sent Events: the ports open now as opened, then
// every change.
func (s *Server) handlePortsWatch(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	ch, current := s.ports.subscribe()
	defer s.ports.unsubscribe(ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	emit := func(typ string, p api.Port) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
	}
	for _, p := range current {
		emit("opened", p)
	}
	fl.Flush()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // fell behind; the client reconnects and lists again
			}
			emit(ev.typ, ev.port)
		case <-keepalive.C:
			io.WriteString(w, ": keepalive\n\n")
		case <-r.Context().Done():
			return
		}
		fl.Flush()
	}
}

// dialPort connects to the sandbox's own port, IPv4 first (most servers bind
// it), then IPv6, retrying until wait has passed (once, for no wait).
func dialPort(port int, wait time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		for _, host := range []string{"127.0.0.1", "::1"} {
			if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second); err == nil {
				return c, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("nothing listens on port %d", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// handlePortConnect is a raw TCP stream to 127.0.0.1:{port}, in binary
// frames both ways; loam-api carries the port URLs over it. A service that
// declares the port as its http_port is started first if it isn't running,
// and given portWait to listen. With nothing listening it answers 502
// port_closed before the upgrade.
func (s *Server) handlePortConnect(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "connect is a WebSocket")
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "port must be 1-65535")
		return
	}
	var wait time.Duration
	if s.services.ensureHTTP(port) {
		wait = portWait
	}
	s.touch()
	tcp, err := dialPort(port, wait)
	if err != nil {
		writeErr(w, http.StatusBadGateway, api.CodePortClosed, err.Error())
		return
	}
	defer tcp.Close()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// The sandbox's side to the socket.
	go func() {
		defer conn.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				s.touch()
				conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if conn.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				closeWS(conn, websocket.CloseNormalClosure, "")
				return
			}
		}
	}()
	// The socket's side to the sandbox.
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.BinaryMessage {
			s.touch()
			if _, err := tcp.Write(data); err != nil {
				return
			}
		}
	}
}
