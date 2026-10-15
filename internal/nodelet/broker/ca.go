// Package broker is the nodelet's secret broker (docs/NODE.md, "Phase 3: the
// network boundary"; docs/API.md, "Secrets"): the proxy hands it a VM's TLS
// connection to one of its secrets' hosts, and it ends the TLS with a leaf
// from the broker CA, puts each secret's value where its placeholder is (or
// sets its header), sends each request on to the host on a connection of
// its own, and cuts any answer that carries a value back. Ported from
// managoat's broker (managoat_broker), whose README is the longer argument for
// most of what is here.
package broker

import (
	"container/list"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Leaves live a week and are made again a day before they end, so none that
// is handed out is close to expiring.
const (
	leafLife     = 7 * 24 * time.Hour
	leafRenew    = 24 * time.Hour
	maxLeaves    = 1024
	leafBackdate = time.Hour // clock skew between the node and the guest
)

// CA is the broker CA (the Secret loam-broker-ca, the same on every nodelet,
// so a sandbox that moves keeps trusting it) and the leaves it signs.
type CA struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  string
	now  func() time.Time

	mu     sync.Mutex
	leaves map[string]*list.Element // by server name
	lru    *list.List               // of *leaf, most recently used first
}

type leaf struct {
	name string
	cert *tls.Certificate
	end  time.Time
}

// LoadCA reads tls.crt and tls.key from dir, as a kubernetes.io/tls Secret
// mounts them.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, err
	}
	return NewCA(certPEM, keyPEM)
}

// NewCA is the CA whose root certificate and key are these PEM blocks. The
// certificate must be a CA's: a guest would trust it, but nothing it signed.
func NewCA(certPEM, keyPEM []byte) (*CA, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("broker CA: %w", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("broker CA: %w", err)
	}
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("broker CA: the certificate is not a CA's (basicConstraints CA and keyCertSign)")
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("broker CA: the key can't sign")
	}
	root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return &CA{cert: cert, key: key, pem: string(root), now: time.Now, leaves: map[string]*list.Element{}, lru: list.New()}, nil
}

// PEM is the root certificate, which every wake carries (nodeapi.Wake.CA).
func (ca *CA) PEM() string { return ca.pem }

// Leaf is a certificate for name signed by the CA, from the cache when it
// has one with more than a day left. The cache is bounded: its key is a name
// the guest chose (the proxy only terminates a secret's hosts, but a
// wildcard host is every name under it), so the least recently used leaf
// goes once it holds maxLeaves, and costs one signature if it comes back.
func (ca *CA) Leaf(name string) (*tls.Certificate, error) {
	now := ca.now()
	ca.mu.Lock()
	if el, ok := ca.leaves[name]; ok {
		l := el.Value.(*leaf)
		if now.Before(l.end.Add(-leafRenew)) {
			ca.lru.MoveToFront(el)
			ca.mu.Unlock()
			return l.cert, nil
		}
		ca.lru.Remove(el)
		delete(ca.leaves, name)
	}
	ca.mu.Unlock()

	// Signed outside the lock: two connections for a new name may both sign,
	// which costs a signature and changes nothing.
	l, err := ca.sign(name, now)
	if err != nil {
		return nil, err
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if el, ok := ca.leaves[name]; ok {
		ca.lru.Remove(el)
	}
	ca.leaves[name] = ca.lru.PushFront(l)
	for ca.lru.Len() > maxLeaves {
		old := ca.lru.Back()
		ca.lru.Remove(old)
		delete(ca.leaves, old.Value.(*leaf).name)
	}
	return l.cert, nil
}

func (ca *CA) sign(name string, now time.Time) (*leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	end := now.Add(leafLife)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-leafBackdate),
		NotAfter:     end,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("leaf for %s: %w", name, err)
	}
	return &leaf{name: name, end: end, cert: &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}}, nil
}
