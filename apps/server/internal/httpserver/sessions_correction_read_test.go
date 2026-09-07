package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionCorrectionReadScopesAcknowledgementToAuthenticatedUserRoomAndTurn(t *testing.T) {
	st := &fakeSessionStore{}
	h := sessionCorrectionReadHandler(fakeIdentifier{id: "alex", ok: true}, st)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/corrections/7/read", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "7")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNoContent)
	if len(st.markCorrectionReadCalls) != 1 {
		t.Fatalf("MarkCorrectionRead calls = %+v, want exactly one", st.markCorrectionReadCalls)
	}
	got := st.markCorrectionReadCalls[0]
	if got.userID != "alex" || got.sessionID != "s1" || got.turn != 7 {
		t.Fatalf("MarkCorrectionRead call = %+v, want alex/s1#7", got)
	}
}

func TestSessionCorrectionReadRejectsInvalidTurn(t *testing.T) {
	for _, turn := range []string{"0", "-1", "not-a-number"} {
		t.Run(turn, func(t *testing.T) {
			st := &fakeSessionStore{}
			h := sessionCorrectionReadHandler(fakeIdentifier{id: "alex", ok: true}, st)
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.SetPathValue("id", "s1")
			req.SetPathValue("turn", turn)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			requireStatus(t, rec, http.StatusBadRequest)
			if len(st.markCorrectionReadCalls) != 0 {
				t.Fatalf("MarkCorrectionRead called for invalid turn %q", turn)
			}
		})
	}
}

func TestSessionCorrectionReadRequiresAuthentication(t *testing.T) {
	h := sessionCorrectionReadHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	assertUnauthorized(t, h, req)
}

func TestSessionCorrectionReadReportsStoreFailure(t *testing.T) {
	st := &fakeSessionStore{markCorrectionReadErr: errors.New("db unavailable")}
	h := sessionCorrectionReadHandler(fakeIdentifier{id: "alex", ok: true}, st)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("id", "s1")
	req.SetPathValue("turn", "1")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}
