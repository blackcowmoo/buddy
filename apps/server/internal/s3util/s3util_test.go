package s3util

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestNewClientHonorsPathStyleAndEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, true, "access", "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("user-1/rec-1.wav"),
	}); err != nil {
		t.Fatalf("PutObject() error = %v", err)
	}

	// Path-style addressing puts the bucket in the URL path rather than a
	// virtual-host subdomain — what PathStyle: true must produce.
	if want := "/bucket/user-1/rec-1.wav"; gotPath != want {
		t.Errorf("path = %q, want %q (PathStyle not honored)", gotPath, want)
	}
}

func TestDeleteAllRemovesEveryKey(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/bucket/"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, true, "access", "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	keys := []string{"a", "b", "c"}
	if err := DeleteAll(context.Background(), client, "bucket", keys); err != nil {
		t.Fatalf("DeleteAll() error = %v", err)
	}

	sort.Strings(deleted)
	if want := []string{"a", "b", "c"}; !equalStrings(deleted, want) {
		t.Errorf("deleted = %v, want %v", deleted, want)
	}
}

func TestDeleteAllIsNoopForEmptyKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, true, "access", "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := DeleteAll(context.Background(), client, "bucket", nil); err != nil {
		t.Fatalf("DeleteAll() error = %v, want nil (no-op)", err)
	}
}

func TestDeleteAllReturnsErrorOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, true, "access", "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if err := DeleteAll(context.Background(), client, "bucket", []string{"a"}); err == nil {
		t.Fatal("DeleteAll() error = nil, want an error when the server rejects the delete")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
