package broker

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/arugula-salad/loam/internal/nodelet/network"
)

// The sandbox never holds a secret's value, so the one way left for it to
// get one is an answer that repeats it: an origin that echoes the request, an
// error page from whatever sits in front of it, a debug endpoint. Only the
// broker knows the values, so only the broker can look. Every answer to a
// brokered request is searched for every value the VM's secrets hold, and is
// cut, not scrubbed, if it has one: rewriting a framed stream means framing
// it again, and a scrubber that misses one spelling still ships the rest of
// an answer the origin should never have sent. What is recognised is a
// value's exact bytes, which any longer string holding it contains, and its
// JSON spelling with "/" written "\/". Not base64, hex or any other
// transformation: this narrows what an accidental echo can do, and is no
// defence against an origin that sets out to exfiltrate. (managoat's
// ReflectionGuard.)

var errReflected = errors.New("the answer holds a secret's value")

// needles are the byte strings the values of secrets are recognised by.
func needles(secrets []network.Secret) [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	for _, s := range secrets {
		if s.Value == "" {
			continue
		}
		for _, n := range []string{s.Value, strings.ReplaceAll(s.Value, "/", `\/`)} {
			if !seen[n] {
				seen[n] = true
				out = append(out, []byte(n))
			}
		}
	}
	return out
}

func holds(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

// headHolds reports whether a response's head (its status, header names and
// values, or trailers) holds a needle.
func headHolds(status string, h http.Header, needles [][]byte) bool {
	if holds([]byte(status), needles) {
		return true
	}
	for k, vs := range h {
		if holds([]byte(k), needles) {
			return true
		}
		for _, v := range vs {
			if holds([]byte(v), needles) {
				return true
			}
		}
	}
	return false
}

// scanner is a response body that fails, rather than hand over, any byte of
// a needle. It never accumulates the body. Bytes are released only once they
// can't be the start of a needle: the longest suffix of what it has that is
// a proper prefix of one waits for the next read, so a value split across
// two reads is caught before its first half is forwarded. Ordinarily that
// suffix is empty and nothing waits. The trailers, which arrive with the
// end of the body, are searched then.
type scanner struct {
	body    io.ReadCloser
	resp    *http.Response
	needles [][]byte
	held    []byte // read but not released
	out     []byte // released, not yet returned
	err     error
}

func (s *scanner) Read(p []byte) (int, error) {
	for len(s.out) == 0 && s.err == nil {
		buf := make([]byte, 32<<10)
		n, err := s.body.Read(buf)
		data := append(s.held, buf[:n]...)
		if holds(data, s.needles) {
			s.held, s.err = nil, errReflected
			break
		}
		if err != nil {
			if err == io.EOF && headHolds("", s.resp.Trailer, s.needles) {
				s.held, s.err = nil, errReflected
				break
			}
			s.out, s.held, s.err = data, nil, err
			break
		}
		keep := 0
		for _, n := range s.needles {
			keep = max(keep, prefixSuffix(data, n))
		}
		s.out = data[:len(data)-keep]
		s.held = append([]byte(nil), data[len(data)-keep:]...)
	}
	if len(s.out) > 0 {
		n := copy(p, s.out)
		s.out = s.out[n:]
		return n, nil
	}
	return 0, s.err
}

func (s *scanner) Close() error { return s.body.Close() }

// prefixSuffix is the length of the longest suffix of data that is a proper
// prefix of needle. Candidates are where the needle's first byte occurs in
// the last len(needle)-1 bytes, earliest (longest) first.
func prefixSuffix(data, needle []byte) int {
	for i := len(data) - min(len(data), len(needle)-1); i < len(data); i++ {
		if data[i] == needle[0] && bytes.Equal(data[i:], needle[:len(data)-i]) {
			return len(data) - i
		}
	}
	return 0
}
