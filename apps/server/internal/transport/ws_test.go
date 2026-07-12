package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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

// fakeHeaderIdentifier stands in for a real verified-identity mechanism
// (identity.OIDCIdentifier in production) in tests that only care about
// transport's fail-closed behavior: refuse before the WS upgrade when
// identity fails, connect when it succeeds.
type fakeHeaderIdentifier struct{ header string }

func (f fakeHeaderIdentifier) Identify(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.Header.Get(f.header)
	return v, v != ""
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
// wiring (does the handler load/seed/save correctly, mint/resume the right
// session, persist turns off the right events?), not SQL correctness — that
// lives in internal/store's own MySQL-backed tests. It mirrors MySQLStore's
// key behaviors that other tests here depend on: Save is a no-op until a
// turn has created the session row, and everything is scoped by
// (userID, sessionID) together.
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession // key: userID + "\x00" + sessionID
}

type fakeSession struct {
	userID  string
	meta    store.SessionMeta
	profile store.Profile
	turns   map[string]store.Turn // key: "<turn>|<role>"
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: make(map[string]*fakeSession)}
}

func fakeStoreKey(userID, sessionID string) string { return userID + "\x00" + sessionID }

func (f *fakeStore) Load(ctx context.Context, userID, sessionID string) (store.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.sessions[fakeStoreKey(userID, sessionID)]
	if d == nil {
		return store.Profile{}, nil
	}
	return store.Profile{Summary: d.profile.Summary, Recent: append([]llm.Message(nil), d.profile.Recent...)}, nil
}

func (f *fakeStore) Save(ctx context.Context, userID, sessionID string, p store.Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.sessions[fakeStoreKey(userID, sessionID)]
	if d == nil {
		return nil // matches MySQLStore.Save: only updates an already-created session row
	}
	d.profile = store.Profile{Summary: p.Summary, Recent: append([]llm.Message(nil), p.Recent...)}
	return nil
}

func (f *fakeStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeStoreKey(userID, sessionID)
	d := f.sessions[key]
	if d == nil {
		if turn != 1 || role != "user" {
			return nil // no session row yet and this isn't the turn that creates one
		}
		d = &fakeSession{userID: userID, meta: store.SessionMeta{ID: sessionID, Title: text}, turns: map[string]store.Turn{}}
		f.sessions[key] = d
	}
	tk := fmt.Sprintf("%d|%s", turn, role)
	t := d.turns[tk]
	t.Turn, t.Role, t.Text, t.Refined = turn, role, text, refined
	d.turns[tk] = t
	return nil
}

func (f *fakeStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.sessions[fakeStoreKey(userID, sessionID)]
	if d == nil {
		return nil
	}
	tk := fmt.Sprintf("%d|user", turn)
	t, ok := d.turns[tk]
	if !ok {
		return nil // matches MySQLStore.SaveCorrection: no-op if the turn isn't saved yet
	}
	cc := c
	t.Correction = &cc
	d.turns[tk] = t
	return nil
}

func (f *fakeStore) ListSessions(ctx context.Context, userID string) ([]store.SessionMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.SessionMeta{}
	for _, d := range f.sessions {
		if d.userID == userID {
			out = append(out, d.meta)
		}
	}
	return out, nil
}

func (f *fakeStore) SessionDetail(ctx context.Context, userID, sessionID string) (store.SessionMeta, []store.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.sessions[fakeStoreKey(userID, sessionID)]
	if d == nil {
		return store.SessionMeta{}, nil, store.ErrNotFound
	}
	turns := make([]store.Turn, 0, len(d.turns))
	for _, t := range d.turns {
		turns = append(turns, t)
	}
	sort.Slice(turns, func(i, j int) bool {
		if turns[i].Turn != turns[j].Turn {
			return turns[i].Turn < turns[j].Turn
		}
		return turns[i].Role == "user" // user sorts before assistant within a turn
	})
	return d.meta, turns, nil
}

func (f *fakeStore) Close() error { return nil }

// fakeAudioSaver is an in-memory AudioSaver: these tests only care that
// backupAudio is invoked with the right key/bytes, not real S3 semantics —
// that lives in internal/audiostore's own tests.
type fakeAudioSaver struct {
	mu    sync.Mutex
	saved map[string][]byte
}

func newFakeAudioSaver() *fakeAudioSaver {
	return &fakeAudioSaver{saved: make(map[string][]byte)}
}

func (f *fakeAudioSaver) SaveStream(ctx context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved[key] = b
	return nil
}

func (f *fakeAudioSaver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

func (f *fakeAudioSaver) snapshot() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.saved))
	for k, v := range f.saved {
		out[k] = v
	}
	return out
}

// ---- helpers -----------------------------------------------------------------

func newTestServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	return newTestServerWithAudio(t, st, nil)
}

func newTestServerWithAudio(t *testing.T, st store.Store, audio AudioSaver) *httptest.Server {
	t.Helper()
	pipe := &pipeline.Pipeline{
		FastSTT:            fakeSTT{text: "hello there"},
		SlowSTT:            fakeSTT{text: "hello there"},
		LLM:                fakeLLM{},
		MaxHistoryMessages: 20,
	}
	h := NewHandler(pipe, identity.NewCookieIdentifier(), st, audio)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	return newFakeStore()
}

// dial connects to srv. sessionID == "" mints a brand-new chat room, matching
// what the frontend does from the room list's "새 대화" action; passing the
// ID from an earlier ready event resumes that room instead.
func dial(t *testing.T, srv *httptest.Server, cookie, sessionID string) (*websocket.Conn, *http.Response) {
	t.Helper()
	hdr := http.Header{}
	if cookie != "" {
		hdr.Set("Cookie", "buddy_uid="+cookie)
	}
	path := "/"
	if sessionID != "" {
		path = "/?session=" + sessionID
	}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + path
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
	c, _ := dial(t, srv, "", "")

	if ev := readEvent(t, c); ev.Type != protocol.EvReady {
		t.Fatalf("first event = %+v, want ready", ev)
	}
}

func TestWSFirstVisitSetsAnonymousCookie(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	_, resp := dial(t, srv, "", "")

	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "buddy_uid" || cookies[0].Value == "" {
		t.Fatalf("expected a buddy_uid cookie to be set, got %+v", cookies)
	}
}

// TestWSRejectsWhenIdentityFails exercises the fail-closed contract: if
// Identify finds no verified identity (e.g. no valid Dex JWT), the handler
// must refuse before ever attempting the WS upgrade.
func TestWSRejectsWhenIdentityFails(t *testing.T) {
	pipe := &pipeline.Pipeline{FastSTT: fakeSTT{text: "hi"}, SlowSTT: fakeSTT{text: "hi"}, LLM: fakeLLM{}}
	h := NewHandler(pipe, fakeHeaderIdentifier{"X-Auth-Request-Email"}, newFakeStore(), nil)
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

// TestWSHeaderIdentityConnectsWhenHeaderPresent is the same fail-closed
// contract, but with identity resolving successfully.
func TestWSHeaderIdentityConnectsWhenHeaderPresent(t *testing.T) {
	pipe := &pipeline.Pipeline{FastSTT: fakeSTT{text: "hi"}, SlowSTT: fakeSTT{text: "hi"}, LLM: fakeLLM{}}
	h := NewHandler(pipe, fakeHeaderIdentifier{"X-Auth-Request-Email"}, newFakeStore(), nil)
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
	_, resp := dial(t, srv, "known-user", "")

	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Fatalf("expected no new cookie when one was already supplied, got %+v", cookies)
	}
}

func TestWSTextTurnRoundTrip(t *testing.T) {
	srv := newTestServer(t, newTestStore(t))
	c, _ := dial(t, srv, "", "")
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

// TestWSBinaryFrameBacksUpAudio verifies each incoming utterance's raw PCM is
// also streamed to the configured AudioSaver, independent of the STT/LLM
// pipeline — a nil AudioSaver (the zero-setup default, exercised by every
// other test via newTestServer) must skip this entirely instead of panicking.
func TestWSBinaryFrameBacksUpAudio(t *testing.T) {
	audio := newFakeAudioSaver()
	srv := newTestServerWithAudio(t, newTestStore(t), audio)
	c, _ := dial(t, srv, "", "")
	readEvent(t, c) // ready

	pcm := []byte("fake pcm bytes")
	if err := c.Write(context.Background(), websocket.MessageBinary, pcm); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	readUntil(t, c, protocol.EvFinal) // the pipeline consumed the frame

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && audio.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if audio.count() != 1 {
		t.Fatalf("audio saves = %d, want 1", audio.count())
	}
	for key, saved := range audio.snapshot() {
		if !strings.HasSuffix(key, ".pcm") {
			t.Errorf("key = %q, want a .pcm suffix", key)
		}
		if string(saved) != string(pcm) {
			t.Errorf("saved bytes = %q, want %q", saved, pcm)
		}
	}
}

// TestWSMemoryPersistsAcrossReconnects automates what was previously verified
// by hand against real Docker containers: the same room's conversation
// accumulates across separate connections that pass its session ID (via the
// store), seeded back into the session on each reconnect. Uses fakeStore —
// real store semantics are covered by internal/store's own MySQL-backed
// tests.
func TestWSMemoryPersistsAcrossReconnects(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	cookie := "test-user-abc123"
	var sessionID string

	send := func(text string) {
		c, _ := dial(t, srv, cookie, sessionID)
		ready := readEvent(t, c)
		if ready.Type != protocol.EvReady {
			t.Fatalf("first event = %+v, want ready", ready)
		}
		sessionID = ready.Session // resume this same room on the next send
		sendText(t, c, text)
		readUntil(t, c, protocol.EvAssistantDone)
		c.Close(websocket.StatusNormalClosure, "")
	}

	waitForRecentCount := func(want int) store.Profile {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second) // CI runners can be much slower than local
		for time.Now().Before(deadline) {
			p, err := st.Load(context.Background(), cookie, sessionID)
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
	// before opening the next connection with the same session, so it seeds
	// from this turn instead of racing it (same reasoning as production:
	// a very fast reconnect can still race the previous save).
	waitForRecentCount(2)

	send("What is my name?")
	profile := waitForRecentCount(4)

	if profile.Recent[0].Content != "My name is Alex." || profile.Recent[2].Content != "What is my name?" {
		t.Fatalf("accumulated turns out of order or wrong: %+v", profile.Recent)
	}
}

// TestWSOmittingSessionParamStartsNewSession is the new-behavior counterpart
// to the memory-persists test above: reconnecting *without* the previous
// room's session ID must NOT resume it — every plain connection is a fresh
// chat room, and only an explicit ?session=<id> resumes one.
func TestWSOmittingSessionParamStartsNewSession(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	cookie := "test-user-xyz"

	c1, _ := dial(t, srv, cookie, "")
	ready1 := readEvent(t, c1)
	sendText(t, c1, "first room's message")
	readUntil(t, c1, protocol.EvAssistantDone)
	c1.Close(websocket.StatusNormalClosure, "")

	// Wait for the first connection's turn to land before opening the second.
	deadline := time.Now().Add(10 * time.Second) // CI runners can be much slower than local
	for time.Now().Before(deadline) {
		if p, _ := st.Load(context.Background(), cookie, ready1.Session); len(p.Recent) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	c2, _ := dial(t, srv, cookie, "") // no session param: a new room
	ready2 := readEvent(t, c2)
	c2.Close(websocket.StatusNormalClosure, "")

	if ready2.Session == "" {
		t.Fatal("ready.Session should never be empty")
	}
	if ready2.Session == ready1.Session {
		t.Fatal("omitting ?session= should mint a new room, got the same session ID back")
	}
	p, err := st.Load(context.Background(), cookie, ready2.Session)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(p.Recent) != 0 {
		t.Fatalf("new session should start with no memory, got %+v", p.Recent)
	}
}

func TestWSDifferentCookiesAreIsolated(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)

	sessions := map[string]string{}
	for _, tc := range []struct{ cookie, text string }{
		{"user-a", "I am user A"},
		{"user-b", "I am user B"},
	} {
		c, _ := dial(t, srv, tc.cookie, "")
		ready := readEvent(t, c)
		sessions[tc.cookie] = ready.Session
		sendText(t, c, tc.text)
		readUntil(t, c, protocol.EvAssistantDone)
		c.Close(websocket.StatusNormalClosure, "")
	}

	deadline := time.Now().Add(10 * time.Second) // CI runners can be much slower than local
	for time.Now().Before(deadline) {
		a, _ := st.Load(context.Background(), "user-a", sessions["user-a"])
		b, _ := st.Load(context.Background(), "user-b", sessions["user-b"])
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

// TestWSFinalAndAssistantTurnsArePersisted checks the transport-layer hook
// (persistEvent in ws.go) that saves the visible parts of a turn — final
// transcript and assistant reply — into the session's durable transcript, not
// just its LLM-context Profile.
func TestWSFinalAndAssistantTurnsArePersisted(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	c, _ := dial(t, srv, "turn-user", "")
	ready := readEvent(t, c)

	sendText(t, c, "Hello Buddy")
	readUntil(t, c, protocol.EvAssistantDone)

	var turns []store.Turn
	deadline := time.Now().Add(10 * time.Second) // CI runners can be much slower than local
	for time.Now().Before(deadline) {
		_, ts, err := st.SessionDetail(context.Background(), "turn-user", ready.Session)
		if err == nil && len(ts) == 2 {
			turns = ts
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 persisted turns (user+assistant), got %+v", turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "Hello Buddy" {
		t.Fatalf("turn[0] = %+v, want user/\"Hello Buddy\"", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text == "" {
		t.Fatalf("turn[1] = %+v, want a non-empty assistant reply", turns[1])
	}
}

// TestWSListSessionsOnlyShowsSessionsWithMessages ensures a connection that
// never sends anything doesn't leave a phantom room in the list — matches
// MySQLStore.Save being a no-op until SaveTurn has created the session row.
func TestWSListSessionsOnlyShowsSessionsWithMessages(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	cookie := "list-user"

	c, _ := dial(t, srv, cookie, "")
	readEvent(t, c) // ready, but never send anything
	c.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond) // let the deferred save (a no-op) run
	sessions, err := st.ListSessions(context.Background(), cookie)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected no listed sessions before any message, got %+v", sessions)
	}
}

