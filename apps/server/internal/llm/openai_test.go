package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamParsesSSEAndConcatenates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var body chatReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !body.Stream {
			t.Errorf("expected stream=true in the request body")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range []string{"Hel", "lo", "!"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL, "")
	var got []string
	full, err := c.ChatStream(context.Background(), "model", []Message{{Role: RoleUser, Content: "hi"}}, func(tok string) {
		got = append(got, tok)
	})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if full != "Hello!" {
		t.Fatalf("full = %q, want %q", full, "Hello!")
	}
	if strings.Join(got, "") != "Hello!" {
		t.Fatalf("onToken sequence = %v, want it to concatenate to %q", got, "Hello!")
	}
}

func TestChatStreamNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL, "")
	_, err := c.ChatStream(context.Background(), "model", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want it to mention status 500", err)
	}
}

func TestChatStreamUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately so the URL is unreachable

	c := NewOpenAI(srv.URL, "")
	_, err := c.ChatStream(context.Background(), "model", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want it to mention \"unreachable\"", err)
	}
}

func TestCompleteReturnsMessageContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Stream {
			t.Errorf("expected stream=false for Complete()")
		}
		if body.ResponseFormat == nil || body.ResponseFormat.Type != "json_object" {
			t.Errorf("expected response_format json_object when jsonMode=true, got %+v", body.ResponseFormat)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"corrected\":\"hi\"}"}}]}`)
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL, "key123")
	got, err := c.Complete(context.Background(), "model", []Message{{Role: RoleUser, Content: "hi"}}, true)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if want := `{"corrected":"hi"}`; got != want {
		t.Fatalf("Complete() = %q, want %q", got, want)
	}
}

func TestCompleteSendsBearerAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL, "secret-key")
	if _, err := c.Complete(context.Background(), "model", nil, false); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer secret-key")
	}
}

func TestCompleteNoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL, "")
	if _, err := c.Complete(context.Background(), "model", nil, false); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization header = %q, want empty when no API key is set", gotAuth)
	}
}

func TestCompleteEmptyChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	c := NewOpenAI(srv.URL, "")
	_, err := c.Complete(context.Background(), "model", nil, false)
	if err == nil {
		t.Fatal("expected an error for an empty choices array")
	}
}

func TestNewOpenAITrimsTrailingSlash(t *testing.T) {
	c := NewOpenAI("http://localhost:8081/v1/", "")
	if c.BaseURL != "http://localhost:8081/v1" {
		t.Fatalf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
}

func TestNewOpenAIAppendsMissingV1(t *testing.T) {
	c := NewOpenAI("http://localhost:8081", "")
	if c.BaseURL != "http://localhost:8081/v1" {
		t.Fatalf("BaseURL = %q, want /v1 appended", c.BaseURL)
	}
}

func TestNewOpenAIDoesNotDoubleV1(t *testing.T) {
	c := NewOpenAI("http://localhost:8081/v1", "")
	if c.BaseURL != "http://localhost:8081/v1" {
		t.Fatalf("BaseURL = %q, want /v1 kept as-is, not doubled", c.BaseURL)
	}
}
