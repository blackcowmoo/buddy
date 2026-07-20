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
	"buddy/server/internal/recording"
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

func (f *fakeStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error {
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
	t.Turn, t.Role, t.Text, t.Refined, t.Source = turn, role, text, refined, source
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

func (f *fakeStore) SaveTranslation(ctx context.Context, userID, sessionID string, turn int, role, translation string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.sessions[fakeStoreKey(userID, sessionID)]
	if d == nil {
		return nil
	}
	tk := fmt.Sprintf("%d|%s", turn, role)
	t, ok := d.turns[tk]
	if !ok {
		return nil // matches MySQLStore.SaveTranslation: no-op if the turn isn't saved yet
	}
	t.Translation = translation
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

func (f *fakeStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, fakeStoreKey(userID, sessionID))
	return nil
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

// Delete removes one saved key, mirroring the userID/sessionID/id.pcm
// convention Handler.backupAudio writes (see ws.go).
func (f *fakeAudioSaver) Delete(ctx context.Context, userID, sessionID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.saved, userID+"/"+sessionID+"/"+id+".pcm")
	return nil
}

// DeleteBySession removes every saved key prefixed userID/sessionID/ — the
// same prefix convention Handler.backupAudio writes (see ws.go).
func (f *fakeAudioSaver) DeleteBySession(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := userID + "/" + sessionID + "/"
	for k := range f.saved {
		if strings.HasPrefix(k, prefix) {
			delete(f.saved, k)
		}
	}
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

// fakeRecordingStore is an in-memory recording.Store: these tests are about
// WS wiring (is Save called with the right userID/audio on a binary frame?),
// not S3/SQL correctness — that lives in internal/recording's own
// container-backed tests.
type fakeRecordingStore struct {
	mu    sync.Mutex
	saved []recordingSave
}

type recordingSave struct {
	userID    string
	sessionID string
	id        string
	pcm       []byte
}

func (f *fakeRecordingStore) Save(ctx context.Context, userID, sessionID, id string, pcm []byte, sampleRate int) (recording.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, recordingSave{userID, sessionID, id, append([]byte(nil), pcm...)})
	return recording.Recording{ID: id, UserID: userID, SessionID: sessionID}, nil
}

func (f *fakeRecordingStore) List(ctx context.Context, userID string) ([]recording.Recording, error) {
	return nil, nil
}

func (f *fakeRecordingStore) Open(ctx context.Context, userID, id string) (recording.Recording, io.ReadCloser, error) {
	return recording.Recording{}, nil, errors.New("fakeRecordingStore: Open not implemented")
}

func (f *fakeRecordingStore) Delete(ctx context.Context, userID, id string) (recording.Recording, error) {
	return recording.Recording{}, errors.New("fakeRecordingStore: Delete not implemented")
}

func (f *fakeRecordingStore) DeleteBySession(ctx context.Context, userID, sessionID string) error {
	return errors.New("fakeRecordingStore: DeleteBySession not implemented")
}

func (f *fakeRecordingStore) Close() error { return nil }

func (f *fakeRecordingStore) all() []recordingSave {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordingSave(nil), f.saved...)
}

// ---- helpers -----------------------------------------------------------------

func newTestServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	return newTestServerFull(t, st, nil, nil)
}

func newTestServerWithAudio(t *testing.T, st store.Store, audio AudioSaver) *httptest.Server {
	t.Helper()
	return newTestServerFull(t, st, audio, nil)
}

func newTestServerWithRecordings(t *testing.T, st store.Store, recordings recording.Store) *httptest.Server {
	t.Helper()
	return newTestServerFull(t, st, nil, recordings)
}

func newTestServerFull(t *testing.T, st store.Store, audio AudioSaver, recordings recording.Store) *httptest.Server {
	t.Helper()
	pipe := &pipeline.Pipeline{
		STT:                []stt.Recognizer{fakeSTT{text: "hello there"}},
		LLM:                fakeLLM{},
		MaxHistoryMessages: 20,
	}
	h := NewHandler(pipe, identity.NewCookieIdentifier(), st, audio, recordings)
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

// tryReadEvent is readEvent but tolerant of a timeout — used to assert an
// event does NOT arrive within a window, which a t.Fatalf-on-error read can't
// express.
func tryReadEvent(t *testing.T, c *websocket.Conn, timeout time.Duration) (protocol.ServerEvent, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		return protocol.ServerEvent{}, false
	}
	var ev protocol.ServerEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal event: %v (raw: %s)", err, data)
	}
	return ev, true
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

// readUntilTurn is readUntil plus a turn check, needed wherever a brand-new
// session's opening-greeting events (turn 0, see pipeline.StartConversation)
// could otherwise be mistaken for the real turn's events of the same Type.
func readUntilTurn(t *testing.T, c *websocket.Conn, want protocol.EventType, turn int) protocol.ServerEvent {
	t.Helper()
	for i := 0; i < 20; i++ {
		ev := readEvent(t, c)
		if ev.Type == want && ev.Turn == turn {
			return ev
		}
	}
	t.Fatalf("did not see event type %q turn %d within 20 messages", want, turn)
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
	pipe := &pipeline.Pipeline{STT: []stt.Recognizer{fakeSTT{text: "hi"}}, LLM: fakeLLM{}}
	h := NewHandler(pipe, fakeHeaderIdentifier{"X-Auth-Request-Email"}, newFakeStore(), nil, nil)
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
	pipe := &pipeline.Pipeline{STT: []stt.Recognizer{fakeSTT{text: "hi"}}, LLM: fakeLLM{}}
	h := NewHandler(pipe, fakeHeaderIdentifier{"X-Auth-Request-Email"}, newFakeStore(), nil, nil)
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
	// Turn 1 specifically (not just "the first assistant_done seen") — a
	// brand-new session like this one also gets an opening-greeting
	// assistant_done on turn 0 (see pipeline.StartConversation), which could
	// otherwise be mistaken for the reply to "Hello Buddy".
	done := readUntilTurn(t, c, protocol.EvAssistantDone, 1)
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
	ready := readEvent(t, c) // ready

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
	// The key must be prefixed with the session ID (not just the user ID) so
	// AudioSaver.DeleteBySession can find it once the room is deleted.
	wantPrefix := "/" + ready.Session + "/"
	for key, saved := range audio.snapshot() {
		if !strings.HasSuffix(key, ".pcm") {
			t.Errorf("key = %q, want a .pcm suffix", key)
		}
		if !strings.Contains(key, wantPrefix) {
			t.Errorf("key = %q, want it to contain session prefix %q", key, wantPrefix)
		}
		if string(saved) != string(pcm) {
			t.Errorf("saved bytes = %q, want %q", saved, pcm)
		}
	}
}

// TestWSBinaryFrameSharesIDBetweenBackupAndRecording verifies the audio
// backup and the durable recording archive save the same utterance under the
// same id — the correlation httpserver.recordingDeleteHandler relies on to
// cascade deleting one recording to its matching temporary backup, since
// otherwise the two independently-generated ids would have nothing in common
// (see AudioSaver.Delete's and recording.Store.Save's doc comments).
func TestWSBinaryFrameSharesIDBetweenBackupAndRecording(t *testing.T) {
	audio := newFakeAudioSaver()
	rec := &fakeRecordingStore{}
	srv := newTestServerFull(t, newTestStore(t), audio, rec)
	c, _ := dial(t, srv, "", "")
	readEvent(t, c) // ready

	if err := c.Write(context.Background(), websocket.MessageBinary, []byte("fake pcm bytes")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	readUntil(t, c, protocol.EvFinal)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (audio.count() == 0 || len(rec.all()) == 0) {
		time.Sleep(20 * time.Millisecond)
	}
	saves := rec.all()
	if len(saves) != 1 {
		t.Fatalf("recording saves = %d, want 1", len(saves))
	}
	wantSuffix := "/" + saves[0].id + ".pcm"
	for key := range audio.snapshot() {
		if !strings.HasSuffix(key, wantSuffix) {
			t.Errorf("audio backup key = %q, want it to end with the recording's id %q", key, wantSuffix)
		}
	}
}

// TestWSAudioBackupDeleteBySessionRemovesOnlyThatSessionsBackups verifies the
// cascade-delete contract AudioSaver.DeleteBySession promises: only backups
// under the given (userID, sessionID) prefix are removed, so deleting one
// chat room's temporary audio backups can never touch another room's.
func TestWSAudioBackupDeleteBySessionRemovesOnlyThatSessionsBackups(t *testing.T) {
	audio := newFakeAudioSaver()
	if err := audio.SaveStream(context.Background(), "alex/s1/a.pcm", strings.NewReader("a")); err != nil {
		t.Fatalf("SaveStream() error = %v", err)
	}
	if err := audio.SaveStream(context.Background(), "alex/s2/b.pcm", strings.NewReader("b")); err != nil {
		t.Fatalf("SaveStream() error = %v", err)
	}

	if err := audio.DeleteBySession(context.Background(), "alex", "s1"); err != nil {
		t.Fatalf("DeleteBySession() error = %v", err)
	}

	remaining := audio.snapshot()
	if _, ok := remaining["alex/s1/a.pcm"]; ok {
		t.Errorf("alex/s1/a.pcm should have been deleted")
	}
	if _, ok := remaining["alex/s2/b.pcm"]; !ok {
		t.Errorf("alex/s2/b.pcm should still be present")
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
		readUntilTurn(t, c, protocol.EvAssistantDone, 1) // turn 1: this send's own reply, not the opening greeting's turn 0
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
	// a very fast reconnect can still race the previous save). 3, not 2:
	// the first connection is brand-new, so its opening greeting (see
	// pipeline.StartConversation) lands in history as well, ahead of this turn.
	waitForRecentCount(3)

	send("What is my name?")
	profile := waitForRecentCount(5)

	if profile.Recent[1].Content != "My name is Alex." || profile.Recent[3].Content != "What is my name?" {
		t.Fatalf("accumulated turns out of order or wrong: %+v", profile.Recent)
	}
}

// TestWSNewSessionGetsOpeningGreeting checks the opening-greeting feature
// (pipeline.StartConversation) end to end: a brand-new session gets an
// assistant message on the reserved turn-0 sentinel before the learner ever
// says anything, and — since it's never persisted per-turn (see
// persistEvent's turn==0 skip) — an abandoned room still leaves no session
// row behind, exactly like TestWSListSessionsOnlyShowsSessionsWithMessages
// expects for a silent connection.
func TestWSNewSessionGetsOpeningGreeting(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	c, _ := dial(t, srv, "greet-user", "")
	ready := readEvent(t, c)

	done := readUntilTurn(t, c, protocol.EvAssistantDone, 0)
	if done.Text == "" {
		t.Fatalf("expected a non-empty opening greeting, got %+v", done)
	}
	c.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond) // let the deferred save (a no-op here) run
	if _, _, err := st.SessionDetail(context.Background(), "greet-user", ready.Session); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("greeting-only session should leave no session row, err = %v", err)
	}
}

// TestWSResumedSessionSkipsOpeningGreeting checks a resumed room (?session=)
// never gets a second greeting — only a bare connection (no ?session=) does.
func TestWSResumedSessionSkipsOpeningGreeting(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	cookie := "resume-user"

	c1, _ := dial(t, srv, cookie, "")
	ready1 := readEvent(t, c1)
	sendText(t, c1, "hello")
	readUntilTurn(t, c1, protocol.EvAssistantDone, 1)
	c1.Close(websocket.StatusNormalClosure, "")

	deadline := time.Now().Add(10 * time.Second) // CI runners can be much slower than local
	for time.Now().Before(deadline) {
		if p, _ := st.Load(context.Background(), cookie, ready1.Session); len(p.Recent) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	c2, _ := dial(t, srv, cookie, ready1.Session)
	readEvent(t, c2) // ready
	for {
		ev, ok := tryReadEvent(t, c2, 300*time.Millisecond)
		if !ok {
			return // nothing else ever arrived — no second greeting, as expected
		}
		if ev.Turn == 0 {
			t.Fatalf("resumed session should not get a second opening greeting, got %+v", ev)
		}
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
		readUntilTurn(t, c, protocol.EvAssistantDone, 1) // turn 1: the real reply, not the opening greeting's turn 0
		c.Close(websocket.StatusNormalClosure, "")
	}

	// 3, not 1: each of these is a brand-new session, so its opening greeting
	// (see pipeline.StartConversation) lands in history ahead of the real
	// user/assistant pair, at index 0.
	deadline := time.Now().Add(10 * time.Second) // CI runners can be much slower than local
	for time.Now().Before(deadline) {
		a, _ := st.Load(context.Background(), "user-a", sessions["user-a"])
		b, _ := st.Load(context.Background(), "user-b", sessions["user-b"])
		if len(a.Recent) >= 3 && len(b.Recent) >= 3 {
			if a.Recent[1].Content != "I am user A" || b.Recent[1].Content != "I am user B" {
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
	if turns[0].Role != "user" || turns[0].Text != "Hello Buddy" || turns[0].Source != protocol.SourceText {
		t.Fatalf("turn[0] = %+v, want user/\"Hello Buddy\"/source=text", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text == "" || turns[1].Source != "" {
		t.Fatalf("turn[1] = %+v, want a non-empty assistant reply with no source", turns[1])
	}
}

// TestWSBinaryFramePersistsVoiceSource verifies a spoken (binary-frame)
// utterance is persisted with Source == protocol.SourceVoice, distinguishing
// it from TestWSFinalAndAssistantTurnsArePersisted's typed path — this is
// what lets the frontend show which input method produced each message.
func TestWSBinaryFramePersistsVoiceSource(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	c, _ := dial(t, srv, "voice-user", "")
	ready := readEvent(t, c)

	if err := c.Write(context.Background(), websocket.MessageBinary, []byte("fake pcm bytes")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	readUntil(t, c, protocol.EvFinal)

	var turns []store.Turn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, ts, err := st.SessionDetail(context.Background(), "voice-user", ready.Session)
		if err == nil && len(ts) >= 1 {
			turns = ts
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(turns) == 0 {
		t.Fatalf("no turns persisted in time")
	}
	if turns[0].Role != "user" || turns[0].Source != protocol.SourceVoice {
		t.Fatalf("turn[0] = %+v, want user/source=voice", turns[0])
	}
}

// TestPersistEventSavesTranslationsByRole checks the persistEvent cases
// added for EvUserTranslation/EvAssistantTranslation: each must reach
// SaveTranslation with the role that disambiguates it from the turn's other
// role, since persistEvent runs its saves off the hot path in goroutines
// where a mixed-up role would silently overwrite the wrong row.
func TestPersistEventSavesTranslationsByRole(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-1", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", "sess-1", 1, "assistant", "Nice!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}

	persistEvent(st, "alex", "sess-1", protocol.ServerEvent{Type: protocol.EvUserTranslation, Turn: 1, Text: "그는 학교에 간다"})
	persistEvent(st, "alex", "sess-1", protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: 1, Text: "좋아요!"})

	var turns []store.Turn
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, ts, err := st.SessionDetail(ctx, "alex", "sess-1")
		if err == nil && len(ts) == 2 && ts[0].Translation != "" && ts[1].Translation != "" {
			turns = ts
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(turns) != 2 {
		t.Fatalf("translations did not persist in time: %+v", turns)
	}
	if turns[0].Role != "user" || turns[0].Translation != "그는 학교에 간다" {
		t.Fatalf("user turn = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Translation != "좋아요!" {
		t.Fatalf("assistant turn = %+v", turns[1])
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

// TestWSBinaryFrameSavesRecording covers the archival side-effect (see
// internal/recording): a spoken utterance must be
// handed to the recording store independently of — and even if — STT/LLM
// fail, since recording.Store.Save doesn't touch the conversation pipeline
// at all.
func TestWSBinaryFrameSavesRecording(t *testing.T) {
	rec := &fakeRecordingStore{}
	srv := newTestServerWithRecordings(t, newTestStore(t), rec)
	c, _ := dial(t, srv, "voice-user", "")
	ready := readEvent(t, c) // ready

	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := c.Write(context.Background(), websocket.MessageBinary, pcm); err != nil {
		t.Fatalf("Write(binary) error = %v", err)
	}
	readUntil(t, c, protocol.EvFinal) // wait for the pipeline to process the utterance

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if saved := rec.all(); len(saved) == 1 {
			if saved[0].userID != "voice-user" {
				t.Fatalf("saved userID = %q, want voice-user", saved[0].userID)
			}
			// sessionID must be threaded through so a later session delete can
			// cascade to this recording (see recording.Store.DeleteBySession).
			if saved[0].sessionID != ready.Session {
				t.Fatalf("saved sessionID = %q, want %q (the minted WS session)", saved[0].sessionID, ready.Session)
			}
			if string(saved[0].pcm) != string(pcm) {
				t.Fatalf("saved pcm = %v, want %v", saved[0].pcm, pcm)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a recording save, got %+v", rec.all())
}

// TestWSNilRecordingStoreDisablesArchival documents that a nil recordings
// store (the default when config.Config's S3Bucket is unset) is safe: no
// panic, the conversation pipeline still runs normally.
func TestWSNilRecordingStoreDisablesArchival(t *testing.T) {
	srv := newTestServerWithRecordings(t, newTestStore(t), nil)
	c, _ := dial(t, srv, "", "")
	readEvent(t, c) // ready

	if err := c.Write(context.Background(), websocket.MessageBinary, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write(binary) error = %v", err)
	}
	readUntil(t, c, protocol.EvFinal) // would hang/fail if the nil store panicked
}
