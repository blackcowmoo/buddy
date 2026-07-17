package recording

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// failDriver is a database/sql driver whose every connection attempt errors
// immediately — a real (non-nil) *sql.DB whose calls fail fast without any
// actual database, network, or panic-on-nil-receiver risk. Used to exercise
// Save()'s S3 upload in isolation from its MySQL half.
type failDriver struct{}

func (failDriver) Open(string) (driver.Conn, error) { return nil, errors.New("failDriver: no connections") }

func newFailingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("recording-fail-driver-test", "")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func init() {
	sql.Register("recording-fail-driver-test", failDriver{})
}

// fakeS3 records the last PUT it received — mirrors
// internal/audiostore/audiostore_test.go's fakeS3, standing in for a real
// Ceph RGW/MinIO/S3 server so path-style addressing and the storage-class
// header can be asserted directly, without a real S3-compatible backend.
type fakeS3 struct {
	path         string
	storageClass string
}

func newFakeS3(t *testing.T, rec *fakeS3) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.storageClass = r.Header.Get("X-Amz-Storage-Class")
		w.Header().Set("ETag", `"fake-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNewS3ClientHonorsPathStyleAndStorageClass guards the actual bug this
// package's config previously had: PathStyle was hardcoded to true
// (regardless of S3Config.PathStyle) and StorageClass was never sent at all.
// Both are now driven by S3Config the same way internal/audiostore already
// does, since the two packages share one S3_* config block.
func TestNewS3ClientHonorsPathStyleAndStorageClass(t *testing.T) {
	var rec fakeS3
	srv := newFakeS3(t, &rec)

	client, err := newS3Client(S3Config{
		Endpoint:  srv.URL,
		PathStyle: true,
		AccessKey: "test-access",
		SecretKey: "test-secret",
	})
	if err != nil {
		t.Fatalf("newS3Client() error = %v", err)
	}

	_, err = client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:       aws.String("recordings"),
		Key:          aws.String("user-1/rec-1.wav"),
		Body:         bytes.NewReader([]byte("gzip bytes")),
		StorageClass: types.StorageClass("REDUCED_REDUNDANCY"),
	})
	if err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	// Path-style addressing puts the bucket in the URL path rather than a
	// virtual-host subdomain — this is what Ceph RGW/MinIO require, and what
	// PathStyle: true (S3_PATH_STYLE) must actually produce.
	if want := "/recordings/user-1/rec-1.wav"; rec.path != want {
		t.Errorf("path = %q, want %q (PathStyle not honored)", rec.path, want)
	}
	if rec.storageClass != "REDUCED_REDUNDANCY" {
		t.Errorf("storage class header = %q, want REDUCED_REDUNDANCY", rec.storageClass)
	}
}

// TestSaveOmitsStorageClassWhenUnset exercises the real Save() path (gzip +
// WAV + PutObjectInput assembly) against a fake S3 server, skipping the
// MySQL half entirely — Save's INSERT is expected to fail against a nil rw,
// but that happens after the S3 PutObject this test cares about, so the
// request already reached the fake server by the time Save() returns.
func TestSaveOmitsStorageClassWhenUnset(t *testing.T) {
	var rec fakeS3
	srv := newFakeS3(t, &rec)

	client, err := newS3Client(S3Config{Endpoint: srv.URL, PathStyle: true, AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatalf("newS3Client() error = %v", err)
	}
	// storageClass left zero-value (unset); rw is a real *sql.DB whose every
	// query fails immediately, so Save()'s later INSERT errors out cleanly —
	// but only after the S3 PutObject this test cares about already ran.
	st := &S3Store{s3: client, bucket: "recordings", rw: newFailingDB(t)}

	_, err = st.Save(context.Background(), "user-1", "session-1", "rec-1", []byte{1, 2, 3, 4}, 16000)
	if err == nil {
		t.Fatal("Save() should still fail at the INSERT step (failDriver), but the S3 PUT above it must have run")
	}

	if rec.storageClass != "" {
		t.Errorf("storage class header = %q, want empty when StorageClass is unset", rec.storageClass)
	}
	if rec.path == "" {
		t.Fatal("PutObject never reached the fake server")
	}
}
