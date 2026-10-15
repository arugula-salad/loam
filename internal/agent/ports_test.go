package agent

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/loam/pkg/api"
)

func TestParseListeners(t *testing.T) {
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F91 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 12346 1 0000000000000000 20 4 30 10 -1
`
	ls := parseListeners(strings.NewReader(tcp))
	if len(ls) != 1 || ls[0].addr != "127.0.0.1" || ls[0].port != 8080 || ls[0].inode != 12345 {
		t.Fatalf("listeners %+v", ls)
	}
	ip, port, ok := parseProcAddr("00000000000000000000000000000000:0050")
	if !ok || ip.String() != "::" || port != 80 {
		t.Fatalf("ipv6 %v %d %v", ip, port, ok)
	}
}

func listen(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, ln.Addr().(*net.TCPAddr).Port
}

func TestPortsListAndWatch(t *testing.T) {
	h := newHarness(t)
	// Subscribe first, so the listener below is a change.
	resp, err := http.Get(h.ts.URL + "/ports/watch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events := make(chan [2]string, 1024)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{ev, v}
			}
		}
		close(events)
	}()
	time.Sleep(100 * time.Millisecond)

	ln, port := listen(t)
	want := func(typ string) api.Port {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-events:
				var p api.Port
				json.Unmarshal([]byte(ev[1]), &p)
				if ev[0] == typ && p.Port == port {
					return p
				}
			case <-deadline:
				t.Fatalf("no %s event for port %d", typ, port)
			}
		}
	}
	p := want("opened")
	if p.Address != "127.0.0.1" || p.PID != os.Getpid() {
		t.Fatalf("opened %+v (our pid %d)", p, os.Getpid())
	}
	var list api.PortList
	h.do("GET", "/ports", nil, &list)
	found := false
	for _, p := range list.Ports {
		found = found || p.Port == port
	}
	if !found {
		t.Fatalf("port %d not in %+v", port, list)
	}
	ln.Close()
	want("closed")
}

func TestPortListAttributesSession(t *testing.T) {
	if _, err := lookPath("python3", []string{"PATH=" + os.Getenv("PATH")}); err != nil {
		t.Skip("needs python3 to listen from a session")
	}
	h := newHarness(t)
	conn := h.dial("/exec", execQuery("python3", "-c",
		"import socket,time; s=socket.socket(); s.bind(('127.0.0.1',0)); s.listen(); print(s.getsockname()[1], flush=True); time.sleep(30)"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, _ := conn.ReadMessage()
	var started api.ExecMessage
	json.Unmarshal(data, &started)
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(strings.TrimSpace(string(data[1:])))
	var list api.PortList
	h.do("GET", "/ports", nil, &list)
	for _, p := range list.Ports {
		if p.Port == port {
			if p.Session != started.Session || p.PID != started.PID {
				t.Fatalf("port %+v, session %s pid %d", p, started.Session, started.PID)
			}
			return
		}
	}
	t.Fatalf("port %d not listed: %+v", port, list)
}

func TestPortConnect(t *testing.T) {
	h := newHarness(t)
	ln, port := listen(t)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 5)
		c.Read(buf)
		c.Write([]byte("echo:" + string(buf)))
		c.Close()
	}()
	conn := h.dial("/ports/"+strconv.Itoa(port)+"/connect", nil)
	conn.WriteMessage(websocket.BinaryMessage, []byte("hello"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []byte
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != websocket.CloseNormalClosure {
				t.Fatalf("close: %v", err)
			}
			break
		}
		if typ != websocket.BinaryMessage {
			t.Fatalf("frame type %d", typ)
		}
		got = append(got, data...)
	}
	if string(got) != "echo:hello" {
		t.Fatalf("got %q", got)
	}

	// Nothing listening: 502 port_closed at once, before the upgrade.
	ln.Close()
	start := time.Now()
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.ts.URL, "http")+"/ports/"+strconv.Itoa(port)+"/connect", nil)
	if err == nil || resp.StatusCode != 502 {
		t.Fatalf("closed port: %v %v", err, resp)
	}
	h.wantStatus(resp, 502, api.CodePortClosed)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("502 took %s", time.Since(start))
	}
}

func TestConnectStartsHTTPService(t *testing.T) {
	if _, err := lookPath("python3", []string{"PATH=" + os.Getenv("PATH")}); err != nil {
		t.Skip("needs python3 for a listening service")
	}
	h := newHarness(t)
	ln, port := listen(t)
	ln.Close() // a free port, for the service
	var svc api.Service
	spec := api.ServiceSpec{Cmd: []string{"python3", "-c",
		"import socket; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(('127.0.0.1'," + strconv.Itoa(port) + ")); s.listen(); c,_=s.accept(); c.sendall(b'hi'); c.close()"},
		HTTPPort: port}
	h.wantStatus(h.do("PUT", "/services/web", spec, &svc), 200, "")
	h.wantStatus(h.do("POST", "/services/web/stop", nil, &svc), 200, "")
	if svc.State != "stopped" {
		t.Fatalf("after stop %+v", svc)
	}
	conn := h.dial("/ports/"+strconv.Itoa(port)+"/connect", nil)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != "hi" {
		t.Fatalf("read %q %v", data, err)
	}
}
