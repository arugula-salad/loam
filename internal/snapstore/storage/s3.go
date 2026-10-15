// The operations and their semantics follow github.com/e2b-dev/infra,
// packages/shared/pkg/storage/storage_aws.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc.,
// Apache-2.0, see ../LICENSE-e2b), which uses the AWS SDK. Loam talks to S3 directly instead:
// the store needs eight requests, and the SDK is a dozen modules. The request signing is AWS
// Signature Version 4 as documented by AWS.

package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// S3Credentials sign every request.
type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

type s3Backend struct {
	client    *http.Client
	endpoint  *url.URL
	bucket    string
	region    string
	pathStyle bool
	creds     S3Credentials
}

// NewS3 returns a backend for an S3 bucket, or an S3-compatible server's (spec.Endpoint).
func NewS3(spec Spec, creds S3Credentials) (Backend, error) {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, errors.New("s3 store: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set")
	}
	region := spec.Region
	if region == "" {
		region = "us-east-1"
	}
	ep := spec.Endpoint
	if ep == "" {
		ep = "https://s3." + region + ".amazonaws.com"
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("s3 store: endpoint: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &s3Backend{
		client:    &http.Client{},
		endpoint:  u,
		bucket:    spec.Bucket,
		region:    region,
		pathStyle: spec.PathStyle,
		creds:     creds,
	}, nil
}

func (b *s3Backend) String() string {
	return fmt.Sprintf("s3://%s (%s)", b.bucket, b.endpoint.Host)
}

// objectURL is the URL of a key ("" for the bucket itself), its path escaped the way SigV4's
// canonical URI is, so the request line and the signature agree.
func (b *s3Backend) objectURL(key string, query url.Values) *url.URL {
	u := *b.endpoint
	path := u.Path
	if b.pathStyle {
		path += "/" + b.bucket
	} else {
		u.Host = b.bucket + "." + u.Host
	}
	if key != "" || !b.pathStyle {
		path += "/" + key
	}
	u.Path = path
	u.RawPath = awsEscape(path, false)
	u.RawQuery = canonicalQuery(query)
	return &u
}

// awsEscape is SigV4's URI encoding: everything but the unreserved characters is
// percent-encoded, and "/" too unless it separates path segments.
func awsEscape(s string, encodeSlash bool) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' && !encodeSlash {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k, true)+"="+awsEscape(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sign adds SigV4 headers to req for a body whose SHA-256 is payloadHash.
func (b *s3Backend) sign(req *http.Request, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if b.creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", b.creds.SessionToken)
	}
	signed := map[string]string{"host": req.URL.Host}
	for name, vs := range req.Header {
		if l := strings.ToLower(name); strings.HasPrefix(l, "x-amz-") || l == "content-md5" || l == "range" {
			signed[l] = strings.TrimSpace(strings.Join(vs, ","))
		}
	}
	names := make([]string, 0, len(signed))
	for n := range signed {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + signed[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := date + "/" + b.region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+b.creds.SecretAccessKey), date)
	key = hmacSHA256(key, b.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		b.creds.AccessKeyID, scope, signedHeaders, sig))
}

// s3Error is S3's XML error body.
type s3Error struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *s3Error) Error() string {
	return fmt.Sprintf("s3: %d %s: %s", e.Status, e.Code, e.Message)
}

// do sends one request, signed, retrying network errors and 5xx/429 answers a few times: every
// request the store makes is idempotent and its body is in memory. A 404 is ErrNotExist. The
// caller closes the response body.
func (b *s3Backend) do(ctx context.Context, method string, u *url.URL, body []byte, header http.Header) (*http.Response, error) {
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, errors.Join(lastErr, ctx.Err())
			case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.URL = u
		req.ContentLength = int64(len(body))
		for k, vs := range header {
			req.Header[k] = vs
		}
		b.sign(req, payloadHash, time.Now())
		resp, err := b.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			lastErr = err
			continue
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		e := &s3Error{Status: resp.StatusCode}
		xml.Unmarshal(data, e)
		if e.Code == "" {
			e.Code = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == http.StatusNotFound && e.Code != "NoSuchBucket" && e.Code != "NoSuchUpload" {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, u.Path)
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = e
			continue
		}
		return nil, e
	}
	return nil, lastErr
}

func (b *s3Backend) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	resp, err := b.do(ctx, http.MethodGet, b.objectURL(key, nil), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (b *s3Backend) Put(ctx context.Context, key string, data []byte) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	resp, err := b.do(ctx, http.MethodPut, b.objectURL(key, nil), data, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (b *s3Backend) OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	if n <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	h := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+n-1)}}
	resp, err := b.do(ctx, http.MethodGet, b.objectURL(key, nil), nil, h)
	if err != nil {
		return nil, err
	}
	// A server that ignores Range answers 200 with the whole object; take our bytes from it.
	if resp.StatusCode == http.StatusOK {
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("s3: skip to %d of %s: %w", off, key, err)
		}
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(resp.Body, n), resp.Body}, nil
	}
	return resp.Body, nil
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

func (b *s3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := b.do(ctx, http.MethodGet, b.objectURL("", q), nil, nil)
		if err != nil {
			return nil, err
		}
		var res listResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: list %q: %w", prefix, err)
		}
		for _, c := range res.Contents {
			keys = append(keys, c.Key)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
	}
	sort.Strings(keys)
	return keys, nil
}

func (b *s3Backend) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	resp, err := b.do(ctx, http.MethodDelete, b.objectURL(key, nil), nil, nil)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (b *s3Backend) NewMultipart(ctx context.Context, key string) (Multipart, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	resp, err := b.do(ctx, http.MethodPost, b.objectURL(key, url.Values{"uploads": {""}}), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("s3: initiate multipart upload of %s: %w", key, err)
	}
	defer resp.Body.Close()
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&res); err != nil || res.UploadID == "" {
		return nil, fmt.Errorf("s3: initiate multipart upload of %s: no upload id (%v)", key, err)
	}
	return &s3Multipart{b: b, key: key, id: res.UploadID, etags: map[int]string{}}, nil
}

type s3Multipart struct {
	b     *s3Backend
	key   string
	id    string
	mu    sync.Mutex
	etags map[int]string
	done  bool
}

func (m *s3Multipart) UploadPart(ctx context.Context, n int, data ...[]byte) error {
	body := bytes.Join(data, nil)
	q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {m.id}}
	resp, err := m.b.do(ctx, http.MethodPut, m.b.objectURL(m.key, q), body, nil)
	if err != nil {
		return fmt.Errorf("s3: upload part %d of %s: %w", n, m.key, err)
	}
	resp.Body.Close()
	m.mu.Lock()
	m.etags[n] = resp.Header.Get("ETag")
	m.mu.Unlock()
	return nil
}

func (m *s3Multipart) Complete(ctx context.Context) error {
	type part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	var req struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}
	m.mu.Lock()
	for n, etag := range m.etags {
		req.Parts = append(req.Parts, part{n, etag})
	}
	m.mu.Unlock()
	sort.Slice(req.Parts, func(i, j int) bool { return req.Parts[i].PartNumber < req.Parts[j].PartNumber })
	body, err := xml.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := m.b.do(ctx, http.MethodPost, m.b.objectURL(m.key, url.Values{"uploadId": {m.id}}), body, nil)
	if err != nil {
		return fmt.Errorf("s3: complete multipart upload of %s: %w", m.key, err)
	}
	defer resp.Body.Close()
	// S3 can answer 200 and still fail the completion, with an error document as the body.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("s3: complete multipart upload of %s: %w", m.key, err)
	}
	if bytes.Contains(data, []byte("<Error>")) {
		e := &s3Error{Status: resp.StatusCode}
		xml.Unmarshal(data, e)
		return fmt.Errorf("s3: complete multipart upload of %s: %w", m.key, e)
	}
	m.done = true
	return nil
}

func (m *s3Multipart) Abort(ctx context.Context) error {
	if m.done {
		return nil
	}
	resp, err := m.b.do(ctx, http.MethodDelete, m.b.objectURL(m.key, url.Values{"uploadId": {m.id}}), nil, nil)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
