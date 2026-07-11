package audiostore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeS3 records the last PUT it received and reports the associated request
// details for tests to assert on. It stands in for a real Ceph RGW/S3 server.
type fakeS3 struct {
	method       string
	path         string
	storageClass string
	body         []byte
}

func newFakeS3(t *testing.T, rec *fakeS3) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.storageClass = r.Header.Get("X-Amz-Storage-Class")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		rec.body = body
		w.Header().Set("ETag", `"fake-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSaveStreamPathStyleAndStorageClass(t *testing.T) {
	var rec fakeS3
	srv := newFakeS3(t, &rec)

	store, err := New(Config{
		Endpoint:     srv.URL,
		PathStyle:    true,
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		Bucket:       "recordings",
		StorageClass: "REDUCED_REDUNDANCY",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	payload := []byte("fake pcm audio bytes")
	if err := store.SaveStream(context.Background(), "user-1/turn-1.pcm", bytes.NewReader(payload)); err != nil {
		t.Fatalf("SaveStream() error = %v", err)
	}

	if rec.method != http.MethodPut {
		t.Errorf("method = %q, want PUT", rec.method)
	}
	// Path-style addressing puts the bucket in the URL path rather than a
	// virtual-host subdomain — this is what Ceph RGW requires.
	if want := "/recordings/user-1/turn-1.pcm"; rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
	if rec.storageClass != "REDUCED_REDUNDANCY" {
		t.Errorf("storage class header = %q, want REDUCED_REDUNDANCY", rec.storageClass)
	}
	if !bytes.Equal(rec.body, payload) {
		t.Errorf("uploaded body = %q, want %q", rec.body, payload)
	}
}

func TestSaveStreamOmitsStorageClassWhenUnset(t *testing.T) {
	var rec fakeS3
	srv := newFakeS3(t, &rec)

	store, err := New(Config{
		Endpoint:  srv.URL,
		PathStyle: true,
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Bucket:    "recordings",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := store.SaveStream(context.Background(), "user-1/turn-1.pcm", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("SaveStream() error = %v", err)
	}

	if rec.storageClass != "" {
		t.Errorf("storage class header = %q, want empty when StorageClass is unset", rec.storageClass)
	}
}
