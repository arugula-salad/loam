//go:build garage

package storage

import (
	"os"
	"testing"
)

// TestGarageBackend runs the backend round trip against a real bucket: LOAM_STORE_URL (an empty
// bucket, e.g. s3://loam-store-test?endpoint=http://127.0.0.1:39000&s3ForcePathStyle=true&region=home-cloud)
// with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. docs/STORE.md has the steps.
func TestGarageBackend(t *testing.T) {
	url := os.Getenv("LOAM_STORE_URL")
	if url == "" {
		t.Fatal("LOAM_STORE_URL is not set")
	}
	b, err := Open(url)
	if err != nil {
		t.Fatal(err)
	}
	backendRoundTrip(t, b)
}
