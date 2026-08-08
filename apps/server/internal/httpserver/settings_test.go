package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsGetReturnsSavedStyle(t *testing.T) {
	st := &fakeSessionStore{styles: map[string]string{"alex": "ask interview-style questions"}}
	h := settingsGetHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var body struct {
		InterlocutorStyle string `json:"interlocutorStyle"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.InterlocutorStyle != "ask interview-style questions" {
		t.Fatalf("interlocutorStyle = %q, want the saved value", body.InterlocutorStyle)
	}
}

func TestSettingsGetReturnsEmptyStringWhenNeverSet(t *testing.T) {
	st := &fakeSessionStore{}
	h := settingsGetHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var body struct {
		InterlocutorStyle string `json:"interlocutorStyle"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.InterlocutorStyle != "" {
		t.Fatalf("interlocutorStyle = %q, want \"\"", body.InterlocutorStyle)
	}
}

func TestSettingsGetReturnsLearnerProfile(t *testing.T) {
	st := &fakeSessionStore{learnerProfiles: map[string]string{"alex": "struggles with articles; loves cooking"}}
	h := settingsGetHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var body struct {
		LearnerProfile string `json:"learnerProfile"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LearnerProfile != "struggles with articles; loves cooking" {
		t.Fatalf("learnerProfile = %q, want the saved value", body.LearnerProfile)
	}
}

func TestSettingsGetInternalErrorOnLearnerProfileFailure(t *testing.T) {
	st := &fakeSessionStore{learnerProfileErr: errors.New("mysql unreachable")}
	h := settingsGetHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}

func TestSettingsGetUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := settingsGetHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})

	req := httptest.NewRequest("GET", "/api/settings", nil)
	assertUnauthorized(t, h, req)
}

func TestSettingsGetInternalErrorOnStoreFailure(t *testing.T) {
	st := &fakeSessionStore{styleErr: errors.New("mysql unreachable")}
	h := settingsGetHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}

func TestSettingsSaveStoresTrimmedStyle(t *testing.T) {
	st := &fakeSessionStore{}
	h := settingsSaveHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"interlocutorStyle":"  sound professional  "}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if st.styles["alex"] != "sound professional" {
		t.Fatalf("saved style = %q, want trimmed \"sound professional\"", st.styles["alex"])
	}
}

func TestSettingsSaveUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := settingsSaveHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})

	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"interlocutorStyle":"x"}`))
	assertUnauthorized(t, h, req)
}

func TestSettingsSaveBadRequestOnMalformedJSON(t *testing.T) {
	h := settingsSaveHandler(fakeIdentifier{id: "alex", ok: true}, &fakeSessionStore{})

	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusBadRequest)
}

// TestSettingsSaveRejectsOverlongStyle guards the cap that keeps a learner
// from ballooning every future chat session's system prompt (and LLM cost)
// with an arbitrarily long paste — see maxInterlocutorStyleLen.
func TestSettingsSaveRejectsOverlongStyle(t *testing.T) {
	st := &fakeSessionStore{}
	h := settingsSaveHandler(fakeIdentifier{id: "alex", ok: true}, st)

	tooLong := strings.Repeat("x", maxInterlocutorStyleLen+1)
	body, err := json.Marshal(map[string]string{"interlocutorStyle": tooLong})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("PUT", "/api/settings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusBadRequest)
	if _, ok := st.styles["alex"]; ok {
		t.Fatalf("overlong style should not have been saved")
	}
}

// TestSettingsSaveCountsRunesNotBytes verifies the length cap is measured in
// characters, not UTF-8 bytes — otherwise a Korean-language style preference
// (3 bytes/char) would hit the cap roughly 3x sooner than an English one.
func TestSettingsSaveCountsRunesNotBytes(t *testing.T) {
	st := &fakeSessionStore{}
	h := settingsSaveHandler(fakeIdentifier{id: "alex", ok: true}, st)

	style := strings.Repeat("가", maxInterlocutorStyleLen) // exactly at the cap, in runes
	body, err := json.Marshal(map[string]string{"interlocutorStyle": style})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("PUT", "/api/settings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if st.styles["alex"] != style {
		t.Fatalf("saved style = %q, want the exact input", st.styles["alex"])
	}
}

func TestSettingsSaveInternalErrorOnStoreFailure(t *testing.T) {
	st := &fakeSessionStore{styleErr: errors.New("mysql unreachable")}
	h := settingsSaveHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"interlocutorStyle":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}
