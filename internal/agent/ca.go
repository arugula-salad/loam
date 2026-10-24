package agent

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The broker CA (docs/AGENT.md): the root the sandbox's node signs brokered
// hosts' certificates with. The agent puts it where the guest's TLS clients
// look, at every wake, because a package update that rewrites a system bundle
// drops it and a sandbox that moved nodes must still trust the same root.

const (
	// caFile is where the root alone is kept, for NODE_EXTRA_CA_CERTS:
	// Node adds that file to its own built-in roots and ignores the system's.
	caFile = "/etc/loam/ca.pem"
	// E2B applications discover this path before starting Node subprocesses.
	e2bCAFile = "/usr/local/share/ca-certificates/e2b-ca.crt"
	// caMarker opens and closes the root in a system bundle, so the next wake
	// replaces it where it is instead of adding another copy.
	caMarker = "# loam broker CA"
)

// caBundles are the system bundles, in the order the first one found is
// the one the environment names.
var caBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine, Arch
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
	"/etc/ssl/cert.pem",                  // Alpine's and macOS's name; often a link to the first
}

// trustPath is p under the configured trust root ("/" in a guest).
func (s *Server) trustPath(p string) string { return filepath.Join(s.cfg.TrustRoot, p) }

// installCA writes the root to the Loam and E2B paths and every system bundle the
// disk has. A bundle reached through a symlink is the file it names, and
// two names for one file are written once.
func (s *Server) installCA(certPEM string) error {
	certs, err := checkCA(certPEM)
	if err != nil {
		return err
	}
	for _, path := range []string{caFile, e2bCAFile} {
		ca := s.trustPath(path)
		if err := os.MkdirAll(filepath.Dir(ca), 0o755); err != nil {
			return err
		}
		if err := writeIfChanged(ca, certs, 0o644); err != nil {
			return err
		}
	}
	done := map[string]bool{}
	for _, b := range caBundles {
		real, err := filepath.EvalSymlinks(s.trustPath(b))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if done[real] {
			continue
		}
		done[real] = true
		fi, err := os.Stat(real)
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		old, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		if err := writeIfChanged(real, withCA(old, certs), fi.Mode().Perm()); err != nil {
			return fmt.Errorf("%s: %w", b, err)
		}
	}
	return nil
}

// checkCA accepts PEM that is certificates and nothing else, and returns it
// re-encoded, so what goes into a bundle is exactly the certificates.
func checkCA(certPEM string) ([]byte, error) {
	rest := []byte(certPEM)
	var out bytes.Buffer
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the CA holds a %q block; only certificates belong there", b.Type)
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return nil, fmt.Errorf("the CA: %w", err)
		}
		pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes})
	}
	if out.Len() == 0 || len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("the CA is not PEM certificates")
	}
	return out.Bytes(), nil
}

// withCA is bundle with certs between marker lines: in place of the first
// marked block it has, or at the end. Any other marked block goes, and a
// marker left open runs to the end of the file, so a bundle is never left
// with two roots or half of one.
func withCA(bundle, certs []byte) []byte {
	block := append(append([]byte(caMarker+"\n"), certs...), caMarker+"\n"...)
	var out []byte
	placed, inside := false, false
	for len(bundle) > 0 {
		line := bundle
		if i := bytes.IndexByte(bundle, '\n'); i >= 0 {
			line = bundle[:i+1]
		}
		bundle = bundle[len(line):]
		if string(bytes.TrimRight(line, "\r\n")) == caMarker {
			if !inside && !placed {
				out = append(out, block...)
				placed = true
			}
			inside = !inside
			continue
		}
		if !inside {
			out = append(out, line...)
		}
	}
	if !placed {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, block...)
	}
	return out
}

// writeIfChanged replaces p with data through a rename in its directory, so
// a TLS client reading the bundle meanwhile sees the old one or the new one;
// and leaves it alone when it already holds data.
func writeIfChanged(p string, data []byte, mode fs.FileMode) error {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".loam-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// caEnv is the broker CA's variables for a new process, beneath everything
// else it gets (docs/AGENT.md, "Users and processes"), so an image or a
// sandbox that sets one of them wins. They follow the root on disk rather
// than the last wake: a wake without one removes nothing, and the root it
// left is still in the bundles.
func (s *Server) caEnv() []string {
	ca := s.trustPath(caFile)
	if _, err := os.Stat(ca); err != nil {
		return nil
	}
	env := []string{"NODE_EXTRA_CA_CERTS=" + ca}
	for _, b := range caBundles {
		p := s.trustPath(b)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			// uv uses its own bundled roots unless told to use the system's.
			return append(env, "SSL_CERT_FILE="+p, "REQUESTS_CA_BUNDLE="+p, "CARGO_HTTP_CAINFO="+p, "UV_NATIVE_TLS=1")
		}
	}
	return env
}
