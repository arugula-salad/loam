package apiserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// randB32 is n lowercase base32 characters from crypto/rand.
func randB32(n int) string {
	b := make([]byte, (n*5+7)/8)
	rand.Read(b)
	return b32.EncodeToString(b)[:n]
}

func newSandboxID() string { return "sb-" + randB32(12) }
func newKeyID() string     { return "key-" + randB32(10) }

// newKeySecret is an API key: loam_ and 32 base32 characters (160 bits).
func newKeySecret() string { return "loam_" + randB32(32) }

func hashKey(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

var (
	idRE = regexp.MustCompile(`^sb-[a-z2-7]{12}$`)
	// labelRE is the name rule for projects and sandboxes (docs/API.md): a DNS
	// label of at most 32 characters. Neither may contain "--", which is what
	// separates the parts of a port URL's host.
	labelRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	keyRE    = regexp.MustCompile(`^loam_[a-z2-7]{32}$`)
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validName(s string) bool { return labelRE.MatchString(s) && !strings.Contains(s, "--") }

// maxHostLabel bounds name+project: "<port>--<name>--<project>" must be one
// DNS label (63 characters), and a port takes up to 5 of them and the two
// separators 4.
const maxHostLabel = 63 - 5 - 4

var (
	adjectives = []string{"amber", "brisk", "calm", "deep", "eager", "fern", "gentle", "hazel", "ivory", "jolly",
		"keen", "lucid", "mellow", "nimble", "olive", "plucky", "quiet", "rustic", "sunny", "tidy", "umber",
		"vivid", "warm", "young", "zesty"}
	nouns = []string{"acorn", "badger", "cedar", "dune", "ember", "finch", "grove", "heron", "inlet", "juniper",
		"kestrel", "loam", "meadow", "newt", "otter", "pine", "quail", "river", "sorrel", "thistle", "upland",
		"vale", "willow", "yarrow", "zephyr"}
)

func pick(list []string) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	return list[n.Int64()]
}

// generateName is <adjective>-<noun>-<4 digits> (docs/API.md).
func generateName() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	return fmt.Sprintf("%s-%s-%04d", pick(adjectives), pick(nouns), n.Int64())
}
