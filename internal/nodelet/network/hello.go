package network

import (
	"encoding/binary"
	"errors"
	"io"
)

// clientHello is what the proxy reads of a TLS ClientHello: the server name
// (SNI). With encrypted_client_hello this is the outer name, and that is the
// one checked. Chrome sends the extension on every connection as GREASE,
// so refusing it would refuse Chrome; and a real ECH needs keys from an HTTPS
// record, which the forwarder never hands a restricted VM, so the outer name
// is the name the client meant.
type clientHello struct {
	serverName string
}

const (
	recordHandshake    = 22
	handshakeHello     = 1
	extServerName      = 0
	maxRecordPayload   = 16384 + 2048 // TLS 1.2's ciphertext bound; a hello record is plaintext and smaller
	maxClientHelloSize = 64 << 10
)

var errNotHello = errors.New("not a TLS ClientHello")

// readClientHello reads the records of a ClientHello from r, which may span
// several, and returns it with every byte it read, which the caller replays
// to wherever the connection goes: the proxy only looks.
func readClientHello(r io.Reader) (clientHello, []byte, error) {
	var raw, msg []byte
	need := -1 // the handshake message's length with its header, once known
	for need < 0 || len(msg) < need {
		var hdr [5]byte
		got, err := io.ReadFull(r, hdr[:])
		raw = append(raw, hdr[:got]...)
		if err != nil {
			return clientHello{}, raw, err
		}
		if hdr[0] != recordHandshake || hdr[1] != 3 {
			return clientHello{}, raw, errNotHello
		}
		n := int(binary.BigEndian.Uint16(hdr[3:]))
		if n == 0 || n > maxRecordPayload {
			return clientHello{}, raw, errNotHello
		}
		frag := make([]byte, n)
		got, err = io.ReadFull(r, frag)
		raw = append(raw, frag[:got]...)
		if err != nil {
			return clientHello{}, raw, err
		}
		msg = append(msg, frag...)
		if need < 0 && len(msg) >= 4 {
			if msg[0] != handshakeHello {
				return clientHello{}, raw, errNotHello
			}
			need = 4 + (int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]))
			if need > maxClientHelloSize {
				return clientHello{}, raw, errNotHello
			}
		}
	}
	h, err := parseClientHello(msg[4:need])
	return h, raw, err
}

// parseClientHello reads a ClientHello's body (RFC 8446, 4.1.2).
func parseClientHello(b []byte) (clientHello, error) {
	c := cursor(b)
	var h clientHello
	if !c.skip(2+32) || // legacy_version, random
		!c.skipVec(1) || // legacy_session_id
		!c.skipVec(2) || // cipher_suites
		!c.skipVec(1) { // legacy_compression_methods
		return h, errNotHello
	}
	if len(c) == 0 {
		return h, nil // no extensions, so no name
	}
	exts, ok := c.vec(2)
	if !ok || len(c) != 0 {
		return h, errNotHello
	}
	for len(exts) > 0 {
		typ, ok1 := exts.u16()
		data, ok2 := exts.vec(2)
		if !ok1 || !ok2 {
			return h, errNotHello
		}
		switch typ {
		case extServerName:
			list, ok := data.vec(2)
			if !ok || len(data) != 0 {
				return h, errNotHello
			}
			for len(list) > 0 {
				kind, ok1 := list.u8()
				name, ok2 := list.vec(2)
				if !ok1 || !ok2 {
					return h, errNotHello
				}
				if kind == 0 { // host_name; the only kind there is
					h.serverName = string(name)
				}
			}
		}
	}
	return h, nil
}

// cursor reads a TLS message front to back.
type cursor []byte

func (c *cursor) skip(n int) bool {
	if len(*c) < n {
		return false
	}
	*c = (*c)[n:]
	return true
}

func (c *cursor) u8() (byte, bool) {
	if len(*c) < 1 {
		return 0, false
	}
	v := (*c)[0]
	*c = (*c)[1:]
	return v, true
}

func (c *cursor) u16() (uint16, bool) {
	if len(*c) < 2 {
		return 0, false
	}
	v := binary.BigEndian.Uint16(*c)
	*c = (*c)[2:]
	return v, true
}

// vec reads a vector with a length prefix of size bytes (1 or 2).
func (c *cursor) vec(size int) (cursor, bool) {
	if len(*c) < size {
		return nil, false
	}
	n := int((*c)[0])
	if size == 2 {
		n = int(binary.BigEndian.Uint16(*c))
	}
	if len(*c) < size+n {
		return nil, false
	}
	v := (*c)[size : size+n]
	*c = (*c)[size+n:]
	return v, true
}

func (c *cursor) skipVec(size int) bool {
	_, ok := c.vec(size)
	return ok
}
