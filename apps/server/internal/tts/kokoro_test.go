package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewKokoroNormalizesBaseURLAndDefaultsVoiceAndVolume(t *testing.T) {
	k := NewKokoro("http://kokoro:8880", "", 0, "")
	if k.BaseURL != "http://kokoro:8880/v1" {
		t.Errorf("BaseURL = %q, want http://kokoro:8880/v1", k.BaseURL)
	}
	if k.Voice != "af_heart" {
		t.Errorf("Voice = %q, want af_heart default", k.Voice)
	}
	if k.VolumeMultiplier != 1.0 {
		t.Errorf("VolumeMultiplier = %v, want 1.0 default for a zero/unset value", k.VolumeMultiplier)
	}

	k2 := NewKokoro("http://kokoro:8880/v1/", "af_bella", 1.5, "")
	if k2.BaseURL != "http://kokoro:8880/v1" {
		t.Errorf("BaseURL = %q, want http://kokoro:8880/v1 (no double /v1, no trailing slash)", k2.BaseURL)
	}
	if k2.Voice != "af_bella" {
		t.Errorf("Voice = %q, want af_bella (explicit voice not overridden)", k2.Voice)
	}
	if k2.VolumeMultiplier != 1.5 {
		t.Errorf("VolumeMultiplier = %v, want 1.5 (explicit value not overridden)", k2.VolumeMultiplier)
	}
}

func TestVersionChangesWithVoiceOrVolume(t *testing.T) {
	base := NewKokoro("http://kokoro:8880", "af_heart", 1.5, "")
	sameSettings := NewKokoro("http://kokoro:8880", "af_heart", 1.5, "")
	if base.Version() != sameSettings.Version() {
		t.Errorf("Version() = %q vs %q, want identical settings to fingerprint the same", base.Version(), sameSettings.Version())
	}

	differentVolume := NewKokoro("http://kokoro:8880", "af_heart", 2.0, "")
	if base.Version() == differentVolume.Version() {
		t.Errorf("Version() unchanged (%q) after a volume change — cached audio would never be regenerated at the new volume", base.Version())
	}

	differentVoice := NewKokoro("http://kokoro:8880", "af_bella", 1.5, "")
	if base.Version() == differentVoice.Version() {
		t.Errorf("Version() unchanged (%q) after a voice change", base.Version())
	}
}

func TestSpeakPostsExpectedRequestAndReturnsBody(t *testing.T) {
	const wantAudio = "fake-mp3-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("path = %q, want /v1/audio/speech", r.URL.Path)
		}
		var body speechReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Input != "hello world" || body.Voice != "af_heart" || body.ResponseFormat != "mp3" {
			t.Errorf("body = %+v, want input=hello world voice=af_heart format=mp3", body)
		}
		if body.VolumeMultiplier != 1.75 {
			t.Errorf("VolumeMultiplier = %v, want 1.75", body.VolumeMultiplier)
		}
		if body.Stream {
			t.Errorf("Speak() should request stream=false, got stream=true")
		}
		io.WriteString(w, wantAudio)
	}))
	defer srv.Close()

	k := NewKokoro(srv.URL, "af_heart", 1.75, "")
	got, err := k.Speak(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Speak() error = %v", err)
	}
	if string(got) != wantAudio {
		t.Fatalf("Speak() = %q, want %q", got, wantAudio)
	}
}

func TestStreamRequestsStreamTrueAndReturnsUnbufferedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body speechReq
		json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Errorf("Stream() should request stream=true, got stream=false")
		}
		io.WriteString(w, "streamed-bytes")
	}))
	defer srv.Close()

	k := NewKokoro(srv.URL, "af_heart", 0, "")
	rc, err := k.Stream(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(got) != "streamed-bytes" {
		t.Fatalf("stream body = %q, want %q", got, "streamed-bytes")
	}
}

func TestSpeakSendsBearerTokenWhenAPIKeySet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	k := NewKokoro(srv.URL, "af_heart", 0, "secret-key")
	if _, err := k.Speak(context.Background(), "hi"); err != nil {
		t.Fatalf("Speak() error = %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-key")
	}
}

func TestSpeakNonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "model overloaded")
	}))
	defer srv.Close()

	k := NewKokoro(srv.URL, "af_heart", 0, "")
	_, err := k.Speak(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("err = %v, want it to mention status 500 and the response body", err)
	}
}

func TestStreamNonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	k := NewKokoro(srv.URL, "af_heart", 0, "")
	_, err := k.Stream(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want it to mention status 503", err)
	}
}

func TestSpeakUnreachableServerReturnsError(t *testing.T) {
	k := NewKokoro("http://127.0.0.1:1", "af_heart", 0, "") // port 0/1 refuses immediately
	_, err := k.Speak(context.Background(), "hi")
	if err == nil {
		t.Fatal("Speak() error = nil, want an error for an unreachable server")
	}
}
