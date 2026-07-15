package stt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPTranscriberPostsMultipartWithModel(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		gotModel = r.FormValue("model")
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if len(b) == 0 {
			t.Errorf("expected a non-empty WAV file body")
		}
		io.WriteString(w, `{"text":"hello world"}`)
	}))
	defer srv.Close()

	h := NewHTTPTranscriber("whisper", []string{srv.URL}, []string{"whisper-large-v3-turbo"})
	res, err := h.Transcribe(context.Background(), []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if res.Text != "hello world" {
		t.Fatalf("Text = %q, want %q", res.Text, "hello world")
	}
	if gotPath != "/audio/transcriptions" {
		t.Fatalf("path = %q, want /audio/transcriptions", gotPath)
	}
	if gotModel != "whisper-large-v3-turbo" {
		t.Fatalf("model field = %q, want whisper-large-v3-turbo", gotModel)
	}
}

func TestHTTPTranscriberOmitsModelFieldWhenEmpty(t *testing.T) {
	var sawModelField bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		_, sawModelField = r.MultipartForm.Value["model"]
		io.WriteString(w, `{"text":"ok"}`)
	}))
	defer srv.Close()

	h := NewHTTPTranscriber("whisper", []string{srv.URL}, []string{""})
	if _, err := h.Transcribe(context.Background(), []byte{1, 2}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if sawModelField {
		t.Fatal("expected no \"model\" field when the paired model is empty")
	}
}

func TestHTTPTranscriberOmitsModelFieldWhenModelsShorterThanURLs(t *testing.T) {
	var sawModelField bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		_, sawModelField = r.MultipartForm.Value["model"]
		io.WriteString(w, `{"text":"ok"}`)
	}))
	defer srv.Close()

	h := NewHTTPTranscriber("whisper", []string{srv.URL}, nil)
	if _, err := h.Transcribe(context.Background(), []byte{1, 2}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if sawModelField {
		t.Fatal("expected no \"model\" field when Models is shorter than URLs")
	}
}

func TestHTTPTranscriberRoundRobinsAcrossURLsWithPairedModels(t *testing.T) {
	var hits [2]int32
	var models [2]string
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits[i], 1)
			r.ParseMultipartForm(1 << 20)
			models[i] = r.FormValue("model")
			io.WriteString(w, `{"text":"ok"}`)
		}))
	}
	srv0, srv1 := mk(0), mk(1)
	defer srv0.Close()
	defer srv1.Close()

	h := NewHTTPTranscriber("whisper", []string{srv0.URL, srv1.URL}, []string{"model-a", "model-b"})
	for i := 0; i < 4; i++ {
		if _, err := h.Transcribe(context.Background(), []byte{1}); err != nil {
			t.Fatalf("Transcribe() error = %v", err)
		}
	}
	if hits[0] != 2 || hits[1] != 2 {
		t.Fatalf("expected 2 hits on each replica, got %v", hits)
	}
	if models[0] != "model-a" || models[1] != "model-b" {
		t.Fatalf("each replica should always see its paired model, got %v", models)
	}
}

func TestHTTPTranscriberNoURLsIsError(t *testing.T) {
	h := NewHTTPTranscriber("whisper", nil, []string{"m"})
	_, err := h.Transcribe(context.Background(), []byte{1})
	if err == nil || !strings.Contains(err.Error(), "no server URLs") {
		t.Fatalf("err = %v, want it to mention no server URLs configured", err)
	}
}

func TestHTTPTranscriberNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer srv.Close()

	h := NewHTTPTranscriber("whisper", []string{srv.URL}, []string{"m"})
	_, err := h.Transcribe(context.Background(), []byte{1})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want it to mention status 500", err)
	}
}

func TestHTTPTranscriberUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed immediately so the URL is unreachable

	h := NewHTTPTranscriber("whisper", []string{srv.URL}, []string{"m"})
	_, err := h.Transcribe(context.Background(), []byte{1})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want it to mention \"unreachable\"", err)
	}
}

func TestHTTPTranscriberName(t *testing.T) {
	h := NewHTTPTranscriber("whisper", []string{"http://x", "http://y"}, []string{"whisper-large-v3-turbo", "whisper-large-v3-turbo"})
	if got, want := h.Name(), "whisper-server(whisper-large-v3-turbo)"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

func TestHTTPTranscriberNameListsDistinctModels(t *testing.T) {
	h := NewHTTPTranscriber("whisper", []string{"http://x", "http://y"}, []string{"model-a", "model-b"})
	if got, want := h.Name(), "whisper-server(model-a,model-b)"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}
