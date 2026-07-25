package audiostore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
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

// fakeObjectStore is a minimal in-memory S3 stand-in that answers
// ListObjectsV2 (GET ?list-type=2) and DeleteObject (DELETE) requests against
// a fixed set of keys — enough to exercise DeleteBySession's
// list-then-delete loop without needing a real S3-compatible server.
type fakeObjectStore struct {
	mu      sync.Mutex
	keys    map[string]bool
	deleted []string
}

func newFakeObjectStore(t *testing.T, keys ...string) (*httptest.Server, *fakeObjectStore) {
	t.Helper()
	store := &fakeObjectStore{keys: make(map[string]bool)}
	for _, k := range keys {
		store.keys[k] = true
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		defer store.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			prefix := r.URL.Query().Get("prefix")
			var matched []string
			for k := range store.keys {
				if strings.HasPrefix(k, prefix) {
					matched = append(matched, k)
				}
			}
			sort.Strings(matched)
			var contents strings.Builder
			for _, k := range matched {
				fmt.Fprintf(&contents, "<Contents><Key>%s</Key><Size>1</Size></Contents>", k)
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Name>recordings</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
%s
</ListBucketResult>`, prefix, len(matched), contents.String())
		case r.Method == http.MethodDelete:
			key := strings.TrimPrefix(r.URL.Path, "/recordings/")
			delete(store.keys, key)
			store.deleted = append(store.deleted, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, store
}

func newTestStore(t *testing.T, srv *httptest.Server) *Store {
	t.Helper()
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
	return store
}

func TestDeleteRemovesOnlyTheGivenBackup(t *testing.T) {
	srv, fake := newFakeObjectStore(t, "alex/s1/a.pcm", "alex/s1/b.pcm")
	store := newTestStore(t, srv)

	if err := store.Delete(context.Background(), "alex", "s1", "a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if fake.keys["alex/s1/a.pcm"] {
		t.Errorf("alex/s1/a.pcm should have been deleted")
	}
	if !fake.keys["alex/s1/b.pcm"] {
		t.Errorf("alex/s1/b.pcm should still be present")
	}
}

func TestDeleteIsNoopForUnknownID(t *testing.T) {
	srv, fake := newFakeObjectStore(t, "alex/s1/a.pcm")
	store := newTestStore(t, srv)

	if err := store.Delete(context.Background(), "alex", "s1", "no-such-id"); err != nil {
		t.Fatalf("Delete() error = %v, want nil (no-op)", err)
	}
	if !fake.keys["alex/s1/a.pcm"] {
		t.Errorf("alex/s1/a.pcm should still be present")
	}
}

func TestDeleteBySessionRemovesOnlyThatSessionsBackups(t *testing.T) {
	srv, fake := newFakeObjectStore(t, "alex/s1/a.pcm", "alex/s1/b.pcm", "alex/s2/c.pcm")
	store := newTestStore(t, srv)

	if err := store.DeleteBySession(context.Background(), "alex", "s1"); err != nil {
		t.Fatalf("DeleteBySession() error = %v", err)
	}

	sort.Strings(fake.deleted)
	if want := []string{"alex/s1/a.pcm", "alex/s1/b.pcm"}; !slices.Equal(fake.deleted, want) {
		t.Errorf("deleted = %v, want %v", fake.deleted, want)
	}
	if !fake.keys["alex/s2/c.pcm"] {
		t.Errorf("alex/s2/c.pcm should still be present")
	}
}

func TestDeleteBySessionIsNoopWhenNoneMatch(t *testing.T) {
	srv, fake := newFakeObjectStore(t, "alex/s2/c.pcm")
	store := newTestStore(t, srv)

	if err := store.DeleteBySession(context.Background(), "alex", "s1"); err != nil {
		t.Fatalf("DeleteBySession() error = %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("deleted = %v, want none", fake.deleted)
	}
}
