// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/storage.go,
// storage_url.go and object_path.go at commit cf536c2 (Copyright 2023 FoundryLabs, Inc.,
// Apache-2.0, see ../LICENSE-e2b). Modified for loam: one small Backend interface instead of
// StorageProvider/Blob/Seekable, only the s3:// and file:// schemes, no server-side encryption,
// signed URLs, object metadata or soft-delete.

// Package storage is where the snapshot store keeps its objects: an S3-compatible bucket (AWS S3,
// MinIO, Garage...) or a local directory, plus the framed compression layer objects are written
// with and the node-local chunk directory reads go through.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// ErrNotExist is returned for an object that isn't there.
var ErrNotExist = errors.New("object does not exist")

// Backend is a bucket of objects named by slash-separated keys.
type Backend interface {
	// Get returns a whole object.
	Get(ctx context.Context, key string) ([]byte, error)
	// Put writes a whole object, replacing any with the key.
	Put(ctx context.Context, key string, data []byte) error
	// OpenRange returns a reader for n bytes of an object from off. Reading fewer is an error
	// the caller sees as io.ErrUnexpectedEOF.
	OpenRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
	// NewMultipart starts writing an object in parts; nothing is visible until Complete.
	NewMultipart(ctx context.Context, key string) (Multipart, error)
	// List returns the keys that start with prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes an object. A missing object is not an error.
	Delete(ctx context.Context, key string) error
	String() string
}

// Multipart is an object being written in parts.
type Multipart interface {
	// UploadPart writes part n (from 1) as the concatenation of data. Parts may be uploaded
	// concurrently and in any order.
	UploadPart(ctx context.Context, n int, data ...[]byte) error
	// Complete assembles the parts in order into the object.
	Complete(ctx context.Context) error
	// Abort discards the parts. It is a no-op after Complete.
	Abort(ctx context.Context) error
}

// Spec is a parsed store URL.
type Spec struct {
	// Scheme is "s3" or "file".
	Scheme string
	// Bucket for s3://.
	Bucket string
	// Path is the base directory for file://.
	Path string
	// Endpoint overrides the S3 endpoint (Garage's); empty means AWS.
	Endpoint string
	// PathStyle forces https://host/bucket/key addressing instead of https://bucket.host/key,
	// which most S3-compatible servers (Garage among them) need.
	PathStyle bool
	// Region is the S3 region the requests are signed for; Garage checks it against its own
	// s3_region.
	Region string
}

// ParseURL parses a store URL, following the gocloud.dev blob URL dialect:
//
//	s3://bucket?endpoint=http://host:3900&s3ForcePathStyle=true&region=garage
//	s3://bucket?region=us-east-1
//	file:///var/lib/loam/store
//
// Unknown query parameters are refused so typos fail fast. Credentials are not accepted in URLs;
// they come from AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (and AWS_SESSION_TOKEN).
func ParseURL(raw string) (Spec, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// Don't echo the raw URL: it may carry credentials. url.Error embeds it too.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return Spec{}, fmt.Errorf("invalid store URL: %w", err)
	}
	if u.User != nil {
		return Spec{}, errors.New("store URL: credentials in URLs are not supported")
	}
	switch strings.ToLower(u.Scheme) {
	case "s3":
		return parseS3URL(u)
	case "file":
		if len(u.Query()) > 0 {
			return Spec{}, errors.New("store URL: file: takes no query parameters")
		}
		if u.Opaque != "" {
			return Spec{Scheme: "file", Path: u.Opaque}, nil
		}
		if u.Host != "" && u.Host != "localhost" {
			return Spec{}, errors.New("store URL: file:// must not have a host (use file:///abs/path)")
		}
		if u.Path == "" {
			return Spec{}, errors.New("store URL: file:// needs a path")
		}
		return Spec{Scheme: "file", Path: u.Path}, nil
	default:
		return Spec{}, fmt.Errorf("store URL: unsupported scheme %q (want s3 or file)", u.Scheme)
	}
}

func parseS3URL(u *url.URL) (Spec, error) {
	if u.Host == "" {
		return Spec{}, errors.New("store URL: missing bucket name")
	}
	if u.Path != "" && u.Path != "/" {
		return Spec{}, errors.New("store URL: key prefixes are not supported (bucket only)")
	}
	spec := Spec{Scheme: "s3", Bucket: u.Host}
	for key, values := range u.Query() {
		value := values[len(values)-1]
		switch key {
		case "endpoint":
			e, err := url.Parse(value)
			if err != nil || e.Scheme != "http" && e.Scheme != "https" || e.Host == "" {
				return Spec{}, errors.New("store URL: endpoint must be an absolute http(s) URL")
			}
			if e.User != nil {
				return Spec{}, errors.New("store URL: credentials in URLs are not supported")
			}
			spec.Endpoint = value
		case "s3ForcePathStyle":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return Spec{}, fmt.Errorf("store URL: invalid s3ForcePathStyle %q", value)
			}
			spec.PathStyle = v
		case "region":
			spec.Region = value
		default:
			return Spec{}, fmt.Errorf("store URL: unsupported query parameter %q (want endpoint, s3ForcePathStyle or region)", key)
		}
	}
	return spec, nil
}

// Open opens the backend a store URL names.
func Open(raw string) (Backend, error) {
	spec, err := ParseURL(raw)
	if err != nil {
		return nil, err
	}
	switch spec.Scheme {
	case "file":
		return NewFS(spec.Path)
	default:
		return NewS3(spec, S3Credentials{
			AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		})
	}
}

// ValidKey accepts only keys io/fs considers valid paths, minus its root ".": unrooted,
// slash-separated UTF-8 with no empty, "." or ".." element. An object store takes a key
// literally while the filesystem backend and the chunk directory resolve it against a
// directory, so a key in any other form could name a different object per backend, or one
// outside the backend's root.
func ValidKey(key string) error {
	if key == "." || !fs.ValidPath(key) {
		return fmt.Errorf("invalid object key %q", key)
	}
	return nil
}
