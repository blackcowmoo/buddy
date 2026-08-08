package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

func TestMessageAudioHandlerServesCachedAudioWithoutGenerating(t *testing.T) {
	st := &fakeSessionStore{detailTurns: []store.Turn{
		{Turn: 3, Role: "user", Text: "hi"},
		{Turn: 3, Role: "assistant", Text: "hello there"},
	}}
	speaker := &fakeAudioSpeaker{}
	cache := &fakeAudioCache{byKey: map[string][]byte{transport.MessageAudioKey("s1", 3, "assistant"): []byte("cached-mp3-bytes")}}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: speaker, Cache: cache})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/3/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "cached-mp3-bytes" {
		t.Fatalf("body = %q, want the cached bytes served as-is", rec.Body.String())
	}
	if speaker.calls != 0 {
		t.Fatalf("Speak/Stream called %d times, want 0 — a cache hit must never regenerate", speaker.calls)
	}
}

// TestMessageAudioHandlerStreamsAndCachesOnCacheMiss guards the whole point
// of the streaming path: the response body is the generated audio (proving
// it was relayed through, not silently dropped) and the same bytes end up
// cached for the next request — the tee, not a buffer-then-send.
func TestMessageAudioHandlerStreamsAndCachesOnCacheMiss(t *testing.T) {
	st := &fakeSessionStore{detailTurns: []store.Turn{
		{Turn: 5, Role: "assistant", Text: "let's practice more"},
	}}
	speaker := &fakeAudioSpeaker{}
	cache := &fakeAudioCache{}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: speaker, Cache: cache})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/5/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "5")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	want := "audio-for:let's practice more"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
	if speaker.calls != 1 {
		t.Fatalf("Speak/Stream called %d times, want exactly 1", speaker.calls)
	}
	key := transport.MessageAudioKey("s1", 5, "assistant")
	if string(cache.byKey[key]) != want {
		t.Fatalf("cached audio for %q = %q, want the same bytes that were streamed to the client (%q)", key, cache.byKey[key], want)
	}
}

func TestMessageAudioHandlerDistinguishesUserAndAssistantTurnsWithTheSameNumber(t *testing.T) {
	// Regression guard: turn numbers are shared between the user's line and
	// the assistant's reply to it (see store.Turn) — the handler must find
	// the row matching the requested role, not whichever comes first in the
	// slice, and must not mix up the two roles' separately cached audio.
	st := &fakeSessionStore{detailTurns: []store.Turn{
		{Turn: 1, Role: "assistant", Text: "assistant text"},
		{Turn: 1, Role: "user", Text: "user text"},
	}}
	speaker := &fakeAudioSpeaker{}
	cache := &fakeAudioCache{}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: speaker, Cache: cache})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/1/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "audio-for:assistant text" {
		t.Fatalf("body = %q, want the assistant turn's text synthesized, not the user's", rec.Body.String())
	}
}

// TestMessageAudioHandlerPlaysTheLearnersOwnLineForPronunciationPractice
// guards that role=user works too — StudyControl.tsx offers 🔊 on both the
// learner's own line and the assistant's reply, not just the latter.
func TestMessageAudioHandlerPlaysTheLearnersOwnLineForPronunciationPractice(t *testing.T) {
	st := &fakeSessionStore{detailTurns: []store.Turn{
		{Turn: 1, Role: "assistant", Text: "assistant text"},
		{Turn: 1, Role: "user", Text: "user text"},
	}}
	speaker := &fakeAudioSpeaker{}
	cache := &fakeAudioCache{}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: speaker, Cache: cache})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/1/audio?role=user", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "audio-for:user text" {
		t.Fatalf("body = %q, want the user turn's own text synthesized", rec.Body.String())
	}
}

func TestMessageAudioHandlerNotFoundForMissingTurn(t *testing.T) {
	st := &fakeSessionStore{detailTurns: []store.Turn{{Turn: 1, Role: "assistant", Text: "hi"}}}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/99/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "99")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestMessageAudioHandlerNotFoundForNonNumericTurn(t *testing.T) {
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, &fakeSessionStore{}, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/not-a-number/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "not-a-number")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestMessageAudioHandlerNotFoundForMissingSession(t *testing.T) {
	st := &fakeSessionStore{detailErr: store.ErrNotFound}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/sessions/missing/messages/1/audio?role=assistant", nil)
	req.SetPathValue("id", "missing")
	req.SetPathValue("turn", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestMessageAudioHandlerServiceUnavailableWhenTTSNotConfigured(t *testing.T) {
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, &fakeSessionStore{}, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/1/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusServiceUnavailable)
}

func TestMessageAudioHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := messageAudioHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/1/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	assertUnauthorized(t, h, req)
}

func TestMessageAudioHandlerGenerationFailureIsServerError(t *testing.T) {
	st := &fakeSessionStore{detailTurns: []store.Turn{{Turn: 1, Role: "assistant", Text: "hi"}}}
	h := messageAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{
		Client: &fakeAudioSpeaker{failWith: errors.New("tts unreachable")},
		Cache:  &fakeAudioCache{},
	})

	req := httptest.NewRequest("GET", "/api/sessions/s1/messages/1/audio?role=assistant", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}
