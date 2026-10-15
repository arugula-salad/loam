package network

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// goHello is the ClientHello crypto/tls sends for serverName.
func goHello(t *testing.T, serverName string) []byte {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		tls.Client(a, &tls.Config{ServerName: serverName, InsecureSkipVerify: serverName == ""}).Handshake()
		a.Close()
	}()
	var hdr [5]byte
	if _, err := io.ReadFull(b, hdr[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[3:]))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}
	b.Close()
	return append(hdr[:], body...)
}

// handHello is a minimal ClientHello with the given extensions, each a type
// and its data.
func handHello(exts ...[]byte) []byte {
	var body []byte
	body = append(body, 3, 3)
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0)                   // session id
	body = append(body, 0, 2, 0x13, 0x01)    // one cipher suite
	body = append(body, 1, 0)                // null compression
	var e []byte
	for _, x := range exts {
		e = append(e, x...)
	}
	body = binary.BigEndian.AppendUint16(body, uint16(len(e)))
	body = append(body, e...)
	msg := append([]byte{handshakeHello, 0, byte(len(body) >> 8), byte(len(body))}, body...)
	return append([]byte{recordHandshake, 3, 1, byte(len(msg) >> 8), byte(len(msg))}, msg...)
}

func ext(typ uint16, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, typ)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

func sniExt(name string) []byte {
	entry := append([]byte{0}, binary.BigEndian.AppendUint16(nil, uint16(len(name)))...)
	entry = append(entry, name...)
	return ext(extServerName, append(binary.BigEndian.AppendUint16(nil, uint16(len(entry))), entry...))
}

func TestReadClientHello(t *testing.T) {
	hello := goHello(t, "api.github.com")
	trailing := []byte("more bytes the proxy must not read")
	h, raw, err := readClientHello(io.MultiReader(bytes.NewReader(hello), bytes.NewReader(trailing)))
	if err != nil || h.serverName != "api.github.com" {
		t.Fatalf("%+v %v", h, err)
	}
	if !bytes.Equal(raw, hello) {
		t.Error("raw is not exactly the hello's bytes")
	}

	// A hello split over two records.
	msg := hello[5:]
	split := append([]byte{recordHandshake, 3, 1, 0, 10}, msg[:10]...)
	rest := msg[10:]
	split = append(split, recordHandshake, 3, 1, byte(len(rest)>>8), byte(len(rest)))
	split = append(split, rest...)
	if h, raw, err := readClientHello(bytes.NewReader(split)); err != nil || h.serverName != "api.github.com" || !bytes.Equal(raw, split) {
		t.Errorf("split: %+v %v", h, err)
	}

	// An encrypted_client_hello (0xfe0d) is read past: the outer name is the one checked.
	if h, _, err := readClientHello(bytes.NewReader(handHello(sniExt("x.example.com"), ext(0xfe0d, []byte{1, 2, 3})))); err != nil || h.serverName != "x.example.com" {
		t.Errorf("ech: %+v %v", h, err)
	}
	if h, _, err := readClientHello(bytes.NewReader(handHello())); err != nil || h.serverName != "" {
		t.Errorf("no sni: %+v %v", h, err)
	}
	if h, _, err := readClientHello(bytes.NewReader(goHello(t, ""))); err != nil || h.serverName != "" {
		t.Errorf("go without a name: %+v %v", h, err)
	}
	if _, raw, err := readClientHello(bytes.NewReader([]byte("GET / HTTP/1.1\r\n\r\n"))); err != errNotHello || string(raw) != "GET /" {
		t.Errorf("http: %q %v", raw, err)
	}
	bad := handHello(sniExt("x.example.com"))
	bad[len(bad)-15] = 0xff // the name's length runs past the end
	if _, _, err := readClientHello(bytes.NewReader(bad)); err == nil {
		t.Error("a truncated server name parsed")
	}
}
