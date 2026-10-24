package netpolicy

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/arugula-salad/loam/pkg/api"
)

var SecretReference = regexp.MustCompile(`\$\{e2b\.secrets\.([^{}\x00-\x1f\x7f]+)\}`)
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// ValidateHeaders rejects routing/framing changes and ambiguous domain or header
// duplicates. These rules transform HTTPS requests; they never authorize egress.
func ValidateHeaders(rules []api.HeaderRule) error {
	if len(rules) > 128 {
		return fmt.Errorf("at most 128 header rules")
	}
	seen := map[string]bool{}
	for _, r := range rules {
		d := strings.ToLower(r.Domain)
		if strings.Contains(d, "/") {
			return fmt.Errorf("header rule domain cannot include a path")
		}
		if err := ValidHost(d); err != nil {
			return err
		}
		if seen[d] {
			return fmt.Errorf("duplicate header rule domain")
		}
		seen[d] = true
		if len(r.Headers) > 64 {
			return fmt.Errorf("at most 64 headers per rule")
		}
		headers := map[string]bool{}
		for k, v := range r.Headers {
			canonical := http.CanonicalHeaderKey(k)
			if !headerName.MatchString(k) || headers[canonical] {
				return fmt.Errorf("invalid or duplicate header name")
			}
			headers[canonical] = true
			switch strings.ToLower(k) {
			case "host", "connection", "content-length", "transfer-encoding", "trailer", "te", "upgrade", "proxy-connection", "proxy-authorization", "proxy-authenticate", "keep-alive":
				return fmt.Errorf("header %s frames or routes the request", k)
			}
			if len(v) > 16<<10 || strings.ContainsAny(v, "\r\n\x00") {
				return fmt.Errorf("invalid header value")
			}
			if strings.Contains(SecretReference.ReplaceAllString(v, ""), "${e2b.") {
				return fmt.Errorf("invalid secret reference")
			}
		}
	}
	return nil
}
