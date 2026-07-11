package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/identity"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/stt"

	"github.com/coder/websocket"
)

// ---- test doubles -----------------------------------------------------------

type fakeSTT struct{ text string }

func (f fakeSTT) Name() string { return "fake" }
func (f fakeSTT) Transcribe(ctx context.Context, pcm []byte) (stt.Result, error) {
	return stt.Result{Text: f.text, Confidence: 1}, nil
}

var errFakeLLMUnavailable = errors.New("fake llm: unavailable")

// fakeLLM always fails, which drives the pipeline's fallback-reply path
// deterministically without a real model — these tests are about WS wiring
// and persistence, not chat content.
type fakeLLM struct{}

func (fakeLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (fakeLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return "", errFakeLLMUnavailable
}

// fakeStore is an in-memory store.Store: these tests are about WS/session
// wiring (does the handler load/seed/save correctly?), not SQL correctness —
// that lives in internal/store's own MySQL-backed tests.
type fakeStore struct {
	mu       sync.Mutex
	profiles map[string]store.Profile
}

func newFakeStore() *fakeStore {
	return &fakeStore{profiles: make(map[string]store.Profile)}
}

func (f *fakeStore) Load(ctx context.Context, userID string) (store.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.profiles[userID] // zero value if absent, matching Store's contract
	return store.Profile{Summary: p.Summary, Recent: append([]llm.Message(nil), p.Recent...)}, nil
}

func (f *fakeStore) Save(ctx context.Context, userID string, p store.Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[userID] = store.Profile{Summary: p.Summary, Recent: append([]llm.Message(nil), p.Recent...)}
	return nil
}

func (f *fakeStore) Close() error { return nil }

// ---- helpers -----------------------------------------------------------------

func newTestServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	pipe := &pipeline.Pipeline{
		FastSTT:            fakeSTT{text: "hello there"},
		SlowSTT:            fakeSTT{text: "hello there"},
		LLM:                fakeLLM{},
		MaxHistoryMessages: 20,
	}
	h := NewHandler(pipe, identity.NewCookieIdentifier(), st)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	return newFakeStore()
}

func dial(t *testing.T, srv *httptest.Server, cookie string) (*websocket.Conn, *http.Response) {
	t.Helper()
	hdr := http.Header{}
	if cookie != "" {
		hdr.Set("Cookie", "buddy_uid="+cookie)
	}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c, resp
}

func readEvent(t *testing.T, c *websocket.Conn) protocol.ServerEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	var ev protocol.ServerEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal event: %v (raw: %s)", err, data)
	}
	return ev
}

func readUntil(t *testing.T, c *websocket.Conn, want protocol.EventType) protocol.ServerEvent {
	t.Helper()
	for i := 0; i < 20; i++ {
		ev := readEvent(t, c)
		if ev.Type == want {
			return ev
		}
	}
	t.Fatalf("did not see event type %q within 20 messages", want)
	return protocol.ServerEvent{}
}

func sendText(t *testing.T, c *websocket.Conn, text string) {
	t.Helper()
	msg, err := json.Marshal(protocol.ClientMsg{Type: "text", Text: text})
	if err != nil {
		t.Fatalf("marshal ClientMsg: %v", err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

// ---- tests -----------------------------------------------------------------

func TestWSHandshakeSendsReady(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	c, _ := dial(t, srv, "")

	if ev := readEvent(t, c); ev.Type != protocol.EvReady {
		t.Fatalf("first event = %+v, want ready", ev)
	}
}

func TestWSFirstVisitSetsAnonymousCookie(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	_, resp := dial(t, srv, "")

	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "buddy_uid" || cookies[0].Value == "" {
		t.Fatalf("expected a buddy_uid cookie to be set, got %+v", cookies)
	}
}

// TestWSRejectsWhenIdentityFails exercises the auth-proxy path (e.g.
// oauth2-proxy in front of Dex): if HeaderIdentifier finds no configured
// header, the handler must refuse before ever attempting the WS upgrade.
func TestWSRejectsWhenIdentityFails(t *testing.T) {
	pipe := &pipeline.Pipeline{FastSTT: fakeSTT{text: "hi"}, SlowSTT: fakeSTT{text: "hi"}, LLM: fakeLLM{}}
	h := NewHandler(pipe, identity.NewHeaderIdentifier("X-Auth-Request-Email"), newFakeStore())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL) // no auth header set
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestWSHeaderIdentityConnectsWhenHeaderPresent is the same auth-proxy path,
// but with the header an authenticated request would actually carry.
func TestWSHeaderIdentityConnectsWhenHeaderPresent(t *testing.T) {
	pipe := &pipeline.Pipeline{FastSTT: fakeSTT{text: "hi"}, SlowSTT: fakeSTT{text: "hi"}, LLM: fakeLLM{}}
	h := NewHandler(pipe, identity.NewHeaderIdentifier("X-Auth-Request-Email"), newFakeStore())
	srv := httptest.NewServer(h)
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("X-Auth-Request-Email", "alex@example.com")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if ev := readEvent(t, c); ev.Type != protocol.EvReady {
		t.Fatalf("first event = %+v, want ready", ev)
	}
}

func TestWSReusedCookieSetsNoNewCookie(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	_, resp := dial(t, srv, "known-user")

	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Fatalf("expected no new cookie when one was already supplied, got %+v", cookies)
	}
}

func TestWSTextTurnRoundTrip(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	c, _ := dial(t, srv, "")
	readEvent(t, c) // ready

	sendText(t, c, "Hello Buddy")

	final := readUntil(t, c, protocol.EvFinal)
	if final.Text != "Hello Buddy" {
		t.Fatalf("final_transcript text = %q", final.Text)
	}
	done := readUntil(t, c, protocol.EvAssistantDone)
	if done.Text == "" {
		t.Fatalf("assistant_done had empty text")
	}
}

// TestWSMemoryPersistsAcrossReconnects automates what was previously verified
// by hand against real Docker containers: the same cookie's conversation
// accumulates across separate connections (via the store), seeded back into
// the session on each reconnect. Uses fakeStore — real store semantics are
// covered by internal/store's own MySQL-backed tests.
func TestWSMemoryPersistsAcrossReconnects(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	cookie := "test-user-abc123"

	send := func(text string) {
		c, _ := dial(t, srv, cookie)
		readEvent(t, c) // ready
		sendText(t, c, text)
		readUntil(t, c, protocol.EvAssistantDone)
		c.Close(websocket.StatusNormalClosure, "")
	}

	waitForRecentCount := func(want int) store.Profile {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			p, err := st.Load(context.Background(), cookie)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if len(p.Recent) >= want {
				return p
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d persisted messages", want)
		return store.Profile{}
	}

	send("My name is Alex.")
	// The connection's save runs in ServeHTTP's deferred cleanup, which is
	// async relative to the client-side close above — wait for it to land
	// before opening the next connection with the same cookie, so it seeds
	// from this turn instead of racing it (same reasoning as production:
	// a very fast reconnect can still race the previous save).
	waitForRecentCount(2)

	send("What is my name?")
	profile := waitForRecentCount(4)

	if profile.Recent[0].Content != "My name is Alex." || profile.Recent[2].Content != "What is my name?" {
		t.Fatalf("accumulated turns out of order or wrong: %+v", profile.Recent)
	}
}

func TestWSDifferentCookiesAreIsolated(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)

	for _, tc := range []struct{ cookie, text string }{
		{"user-a", "I am user A"},
		{"user-b", "I am user B"},
	} {
		c, _ := dial(t, srv, tc.cookie)
		readEvent(t, c) // ready
		sendText(t, c, tc.text)
		readUntil(t, c, protocol.EvAssistantDone)
		c.Close(websocket.StatusNormalClosure, "")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := st.Load(context.Background(), "user-a")
		b, _ := st.Load(context.Background(), "user-b")
		if len(a.Recent) > 0 && len(b.Recent) > 0 {
			if a.Recent[0].Content != "I am user A" || b.Recent[0].Content != "I am user B" {
				t.Fatalf("cross-contamination between users: a=%+v b=%+v", a, b)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for both users' profiles to persist")
}

func TestWSResetClearsVisibleTurnCounter(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	c, _ := dial(t, srv, "")
	readEvent(t, c) // ready

	sendText(t, c, "first")
	readUntil(t, c, protocol.EvAssistantDone)

	resetMsg, _ := json.Marshal(protocol.ClientMsg{Type: "reset"})
	if err := c.Write(context.Background(), websocket.MessageText, resetMsg); err != nil {
		t.Fatalf("Write(reset) error = %v", err)
	}

	sendText(t, c, "second")
	final := readUntil(t, c, protocol.EvFinal)
	if final.Turn != 1 {
		t.Fatalf("turn counter should restart after reset, got turn=%d", final.Turn)
	}
}
