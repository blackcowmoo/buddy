package httpserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"buddy/server/internal/writing"
)

type fakeWritingStore struct{ prompts []writing.Prompt }

func (f *fakeWritingStore) Create(context.Context, string) (writing.Prompt, error) {
	return writing.Prompt{}, nil
}
func (f *fakeWritingStore) List(context.Context, string) ([]writing.Prompt, error) {
	return f.prompts, nil
}
func (f *fakeWritingStore) Get(_ context.Context, userID, id string) (writing.Prompt, error) {
	for _, p := range f.prompts {
		if p.UserID == userID && p.ID == id {
			return p, nil
		}
	}
	return writing.Prompt{}, nil
}
func (f *fakeWritingStore) Delete(_ context.Context, userID, id string) error {
	for i, p := range f.prompts {
		if p.UserID == userID && p.ID == id {
			f.prompts = append(f.prompts[:i], f.prompts[i+1:]...)
			return nil
		}
	}
	return nil
}
func (f *fakeWritingStore) Complete(context.Context, string, string) error { return nil }
func (f *fakeWritingStore) Fail(context.Context, string) error             { return nil }
func (f *fakeWritingStore) Close() error                                   { return nil }

func TestWritingListHandlerReturnsPersistedPrompts(t *testing.T) {
	created := time.Unix(1700000000, 0)
	st := &fakeWritingStore{prompts: []writing.Prompt{{ID: "p1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone, CreatedAt: created}}}
	h := writingListHandler(fakeIdentifier{id: "alex", ok: true}, st)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/writing", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `[{"id":"p1","korean":"오늘은 비가 와요.","status":"done","createdAt":1700000000}]`+"\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestWritingInstanceHandlerDoesNotExposeAnotherUsersPrompt(t *testing.T) {
	st := &fakeWritingStore{prompts: []writing.Prompt{{ID: "p1", UserID: "other", Korean: "비밀", Status: writing.StatusDone}}}
	h := writingInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/writing/p1", nil)
	req.SetPathValue("id", "p1")
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWritingDeleteHandlerOnlyDeletesCallersPrompt(t *testing.T) {
	st := &fakeWritingStore{prompts: []writing.Prompt{
		{ID: "mine", UserID: "alex"}, {ID: "other", UserID: "other"},
	}}
	h := writingDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st)
	req := httptest.NewRequest("DELETE", "/api/writing/mine", nil)
	req.SetPathValue("id", "mine")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 || len(st.prompts) != 1 || st.prompts[0].ID != "other" {
		t.Fatalf("status=%d prompts=%+v, want caller's prompt deleted only", rec.Code, st.prompts)
	}
}
