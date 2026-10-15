package nodeapi

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The agent stream round-trips bytes both ways once upgraded, and a refused
// upgrade comes back as the nodelet's error.
func TestAgentStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms/sb-missing/agent" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":"not_found","message":"no such VM"}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c, err := AcceptUpgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("echo " + line))
	}))
	defer srv.Close()
	cl := &Client{Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "tok"}

	s, err := cl.DialAgent(context.Background(), "sb-one")
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("hello\n"))
	got, _ := bufio.NewReader(s).ReadString('\n')
	if got != "echo hello\n" {
		t.Errorf("got %q", got)
	}
	s.Close()

	_, err = cl.DialAgent(context.Background(), "sb-missing")
	if !IsCode(err, "not_found") {
		t.Errorf("refused upgrade: %v, want not_found", err)
	}
	var _ net.Conn = s
}
