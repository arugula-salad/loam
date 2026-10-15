package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

func TestParseURL(t *testing.T) {
	spec, err := ParseURL("s3://loam-snapshots?endpoint=http://s3.garage.svc.cluster.local:3900&s3ForcePathStyle=true&region=home-cloud")
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{Scheme: "s3", Bucket: "loam-snapshots", Endpoint: "http://s3.garage.svc.cluster.local:3900", PathStyle: true, Region: "home-cloud"}
	if spec != want {
		t.Fatalf("got %+v\nwant %+v", spec, want)
	}
	if spec, err := ParseURL("file:///var/lib/loam/store"); err != nil || spec.Path != "/var/lib/loam/store" {
		t.Fatalf("file URL: %+v %v", spec, err)
	}
	for _, bad := range []string{
		"gs://bucket", "s3://", "s3://b/prefix", "s3://b?typo=1", "s3://user:pw@b",
		"s3://b?endpoint=ftp://x", "s3://b?endpoint=http://k:s@host", "file://host/x", "file:///x?y=1",
	} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

// The example from AWS's Signature Version 4 documentation for S3 ("GET Object" with a range,
// examplebucket, 20130524).
func TestSigV4AWSExample(t *testing.T) {
	b, err := NewS3(Spec{Bucket: "examplebucket", Region: "us-east-1"},
		S3Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"})
	if err != nil {
		t.Fatal(err)
	}
	s := b.(*s3Backend)
	s.endpoint, _ = url.Parse("https://s3.amazonaws.com")
	u := s.objectURL("test.txt", nil)
	if u.String() != "https://examplebucket.s3.amazonaws.com/test.txt" {
		t.Fatalf("url %s", u)
	}
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set("Range", "bytes=0-9")
	now, _ := time.Parse("20060102T150405Z", "20130524T000000Z")
	s.sign(req, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
}

func TestObjectURLEscaping(t *testing.T) {
	b, _ := NewS3(Spec{Bucket: "bkt", Endpoint: "http://127.0.0.1:3900", PathStyle: true, Region: "garage"}, S3Credentials{AccessKeyID: "a", SecretAccessKey: "b"})
	u := b.(*s3Backend).objectURL("snapshots/sb.@3/meta.json", url.Values{"uploads": {""}})
	if got := u.String(); got != "http://127.0.0.1:3900/bkt/snapshots/sb.%403/meta.json?uploads=" {
		t.Fatalf("url %s", got)
	}
	if got := b.(*s3Backend).objectURL("", url.Values{"prefix": {"a b"}, "list-type": {"2"}}).String(); got != "http://127.0.0.1:3900/bkt?list-type=2&prefix=a%20b" {
		t.Fatalf("list url %s", got)
	}
}

// fakeS3 is enough of S3 for the backend's requests, in memory, path-style. It checks every
// request carries a signature for the right region, not that the signature is right (the AWS
// example does that).
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	n       int
	fail500 int
}

func newFakeS3(t *testing.T) (*fakeS3, Backend) {
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b, err := NewS3(Spec{Bucket: "bkt", Endpoint: srv.URL, PathStyle: true, Region: "garage"}, S3Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return f, b
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(r.Header.Get("Authorization"), "/garage/s3/aws4_request") {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	if f.fail500 > 0 {
		f.fail500--
		http.Error(w, "<Error><Code>InternalError</Code></Error>", http.StatusInternalServerError)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/bkt/")
	if !ok && r.URL.Path != "/bkt" {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	notFound := func() { http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound) }
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		// Two keys a page, to exercise continuation.
		start := 0
		if tok := q.Get("continuation-token"); tok != "" {
			start, _ = strconv.Atoi(tok)
		}
		end := min(start+2, len(keys))
		fmt.Fprint(w, "<ListBucketResult>")
		for _, k := range keys[start:end] {
			fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
		}
		if end < len(keys) {
			fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
		}
		fmt.Fprint(w, "</ListBucketResult>")
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.n++
		id := strconv.Itoa(f.n)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		parts := f.uploads[q.Get("uploadId")]
		if parts == nil {
			http.Error(w, "<Error><Code>NoSuchUpload</Code></Error>", http.StatusNotFound)
			return
		}
		if len(body) == 0 {
			// As Garage: "400 InvalidRequest: Bad request: Empty body".
			http.Error(w, "<Error><Code>InvalidRequest</Code><Message>Bad request: Empty body</Message></Error>", http.StatusBadRequest)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts[n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := f.uploads[q.Get("uploadId")]
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		xml.Unmarshal(body, &req)
		var obj []byte
		for _, p := range req.Parts {
			if p.ETag != fmt.Sprintf(`"etag-%d"`, p.PartNumber) {
				fmt.Fprint(w, "<Error><Code>InvalidPart</Code></Error>")
				return
			}
			obj = append(obj, parts[p.PartNumber]...)
		}
		f.objects[key] = obj
		delete(f.uploads, q.Get("uploadId"))
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet:
		obj, ok := f.objects[key]
		if !ok {
			notFound()
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var a, b int
			fmt.Sscanf(rng, "bytes=%d-%d", &a, &b)
			b = min(b, len(obj)-1)
			w.WriteHeader(http.StatusPartialContent)
			w.Write(obj[a : b+1])
			return
		}
		w.Write(obj)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "<Error><Code>NotImplemented</Code></Error>", http.StatusNotImplemented)
	}
}

// backendRoundTrip is what the store needs of a backend; the Garage test runs it too.
func backendRoundTrip(t *testing.T, b Backend) {
	ctx := context.Background()
	if err := b.Put(ctx, "snapshots/x.@1/meta.json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Get(ctx, "snapshots/x.@1/meta.json"); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("get: %q %v", got, err)
	}
	if _, err := b.Get(ctx, "snapshots/none/meta.json"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("get of a missing object: %v", err)
	}
	// An empty layer is an empty object (S3 servers refuse an empty part).
	if res, err := UploadFramed(ctx, b, "layers/e/disk.zst", bytes.NewReader(nil), DefaultCompressConfig()); err != nil || res.Size != 0 {
		t.Fatalf("empty upload: %+v %v", res, err)
	}
	if got, err := b.Get(ctx, "layers/e/disk.zst"); err != nil || len(got) != 0 {
		t.Fatalf("the empty object: %q %v", got, err)
	}
	b.Delete(ctx, "layers/e/disk.zst")
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 3<<20)
	r.Read(data[:1<<20])
	cfg := DefaultCompressConfig()
	cfg.FrameSize = 256 << 10
	cfg.PartSize = 64 << 10
	for _, ct := range []header.CompressionType{header.CompressionZstd, header.CompressionLZ4, header.CompressionNone} {
		cfg.Type = ct
		key := "layers/l/memory" + ct.Suffix()
		res, err := UploadFramed(ctx, b, key, bytes.NewReader(data), cfg)
		if err != nil {
			t.Fatalf("%s: %v", ct, err)
		}
		if res.Size != int64(len(data)) {
			t.Fatalf("%s: uploaded %d bytes, want %d", ct, res.Size, len(data))
		}
		if ct == header.CompressionNone && res.Frames != nil || ct != header.CompressionNone && res.Frames.NumFrames() != 12 {
			t.Fatalf("%s: frames %v", ct, res.Frames)
		}
		// Every frame read back through a fresh chunk directory, then a warm one.
		root := t.TempDir()
		for pass := range 2 {
			stats := &Stats{}
			cd := NewChunkDir(root, b, stats)
			for off := int64(0); off < int64(len(data)); off += int64(cfg.FrameSize) {
				rc, err := cd.OpenChunk(ctx, key, off, int64(cfg.FrameSize), res.Frames)
				if err != nil {
					t.Fatalf("%s pass %d: open %d: %v", ct, pass, off, err)
				}
				got, err := io.ReadAll(rc)
				if err != nil || rc.Close() != nil || !bytes.Equal(got, data[off:off+int64(cfg.FrameSize)]) {
					t.Fatalf("%s pass %d: frame at %d differs (%v)", ct, pass, off, err)
				}
			}
			cd.Wait()
			if pass == 1 && stats.Fetches.Load() != 0 {
				t.Fatalf("%s: warm pass fetched %d", ct, stats.Fetches.Load())
			}
		}
	}
	keys, err := b.List(ctx, "layers/")
	if err != nil || len(keys) != 3 {
		t.Fatalf("list: %v %v", keys, err)
	}
	for _, k := range append(keys, "snapshots/x.@1/meta.json", "snapshots/none/meta.json") {
		if err := b.Delete(ctx, k); err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
	}
	if keys, _ := b.List(ctx, ""); len(keys) != 0 {
		t.Fatalf("left after delete: %v", keys)
	}
}

func TestFSBackend(t *testing.T) {
	b, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backendRoundTrip(t, b)
	if err := b.Put(context.Background(), "../escape", nil); err == nil {
		t.Fatal("wrote outside the base")
	}
}

func TestS3BackendAgainstFake(t *testing.T) {
	f, b := newFakeS3(t)
	backendRoundTrip(t, b)
	// A 5xx is retried.
	f.mu.Lock()
	f.fail500 = 2
	f.mu.Unlock()
	if err := b.Put(context.Background(), "a", []byte("x")); err != nil {
		t.Fatalf("put after two 500s: %v", err)
	}
	// An aborted multipart upload leaves nothing.
	m, err := b.NewMultipart(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	m.UploadPart(context.Background(), 1, []byte("part"))
	m.Abort(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 0 || f.objects["b"] != nil {
		t.Fatal("aborted upload left something")
	}
}
