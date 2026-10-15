package broker

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/arugula-salad/loam/internal/nodelet/network"
)

// applicable are the secrets whose hosts cover a request to name at path
// (the path as the client sent it, without the query).
func applicable(secrets []network.Secret, name, path string) []network.Secret {
	var out []network.Secret
	for _, s := range secrets {
		for _, h := range s.Hosts {
			if h.MatchName(name) && h.MatchPath(path) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// check says why a request can't be sent with secrets, or "". A secret it
// needs whose value the node doesn't have is a refusal, never a request sent
// without it: the origin's 401 would read as the sandbox's credential being
// wrong. So is a value that can't be written where its placeholder sits: a
// space or a control character would end the request target.
func check(secrets []network.Secret, target string, h http.Header) string {
	for _, s := range secrets {
		if s.Inject != nil {
			if s.Value == "" {
				return fmt.Sprintf("secret %s has no value on this node", s.Name)
			}
			continue
		}
		inTarget := strings.Contains(target, s.Placeholder)
		if s.Value == "" && (inTarget || inHeaders(h, s.Placeholder)) {
			return fmt.Sprintf("secret %s has no value on this node", s.Name)
		}
		if inTarget && strings.ContainsFunc(s.Value, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return fmt.Sprintf("secret %s holds a character a request target can't", s.Name)
		}
	}
	return ""
}

// inHeaders reports whether placeholder is in a header value, or in a Basic
// credential, decoded.
func inHeaders(h http.Header, placeholder string) bool {
	for _, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, placeholder) {
				return true
			}
			if dec, ok := basic(v); ok && strings.Contains(dec, placeholder) {
				return true
			}
		}
	}
	return false
}

// rewrite puts the secrets into a request that check passed: each value
// where its placeholder is, byte for byte, in the target and every header
// value, and inside a Basic credential, decoded (a client that puts a token
// in a URL's userinfo, git among them, sends it there); or, for a secret
// with inject, its header set, replacing the client's. Nothing is
// percent-encoded on the way in: the broker can't know which part of the URL
// a placeholder sits in, nor what the origin expects there. It returns the
// target to send.
func rewrite(secrets []network.Secret, target string, h http.Header) string {
	for _, s := range secrets {
		if s.Inject != nil || s.Value == "" {
			continue
		}
		target = strings.ReplaceAll(target, s.Placeholder, s.Value)
		for k, vs := range h {
			for i, v := range vs {
				if dec, ok := basic(v); ok && strings.Contains(dec, s.Placeholder) {
					v = v[:len("Basic ")] + base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(dec, s.Placeholder, s.Value)))
				}
				h[k][i] = strings.ReplaceAll(v, s.Placeholder, s.Value)
			}
		}
	}
	for _, s := range secrets {
		if s.Inject != nil {
			h.Set(s.Inject.Header, strings.Replace(s.Inject.Format, "{value}", s.Value, 1))
		}
	}
	return target
}

// basic decodes an Authorization: Basic credential.
func basic(v string) (string, bool) {
	if len(v) < len("Basic ") || !strings.EqualFold(v[:len("Basic ")], "Basic ") {
		return "", false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v[len("Basic "):]))
	return string(dec), err == nil
}
