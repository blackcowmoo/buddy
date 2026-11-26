package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/writing"
)

// fakeWritingStore mirrors the real store's check-state transitions so the
// handlers are exercised against the same pending/done/failed fences the
// MySQL implementation enforces.
type fakeWritingStore struct {
	mu      sync.Mutex
	prompts []writing.Prompt
}

func (f *fakeWritingStore) Create(context.Context, string) (writing.Prompt, error) {
	return writing.Prompt{}, nil
}
func (f *fakeWritingStore) List(context.Context, string) ([]writing.Prompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]writing.Prompt(nil), f.prompts...), nil
}
func (f *fakeWritingStore) findLocked(userID, id string) *writing.Prompt {
	for i := range f.prompts {
		if f.prompts[i].UserID == userID && f.prompts[i].ID == id {
			return &f.prompts[i]
		}
	}
	return nil
}
func (f *fakeWritingStore) Get(_ context.Context, userID, id string) (writing.Prompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.findLocked(userID, id); p != nil {
		return *p, nil
	}
	return writing.Prompt{}, nil
}
func (f *fakeWritingStore) Delete(_ context.Context, userID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
func (f *fakeWritingStore) StartCheck(_ context.Context, userID, id, answer string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.findLocked(userID, id)
	if p == nil {
		return false, nil
	}
	p.Answer, p.CheckStatus, p.Corrected, p.Issues = answer, writing.CheckPending, "", nil
	return true, nil
}
func (f *fakeWritingStore) CompleteCheck(_ context.Context, id, answer, corrected string, issues []protocol.Issue) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prompts {
		if f.prompts[i].ID == id && f.prompts[i].Answer == answer && f.prompts[i].CheckStatus == writing.CheckPending {
			f.prompts[i].CheckStatus, f.prompts[i].Corrected, f.prompts[i].Issues = writing.CheckDone, corrected, issues
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeWritingStore) FailCheck(_ context.Context, id, answer string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prompts {
		if f.prompts[i].ID == id && f.prompts[i].Answer == answer && f.prompts[i].CheckStatus == writing.CheckPending {
			f.prompts[i].CheckStatus = writing.CheckFailed
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeWritingStore) Recheck(_ context.Context, userID, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.findLocked(userID, id)
	if p == nil || p.Answer == "" || p.CheckStatus == writing.CheckPending {
		return false, nil
	}
	p.CheckStatus, p.Corrected, p.Issues = writing.CheckPending, "", nil
	return true, nil
}
func (f *fakeWritingStore) Close() error { return nil }

func (f *fakeWritingStore) snapshot(id string) (writing.Prompt, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prompts {
		if p.ID == id {
			return p, true
		}
	}
	return writing.Prompt{}, false
}

// correctionLLM answers every stage with one fixed correction payload; with
// no Analysis/Judge candidates the cascade terminates on the Chat draft.
type correctionLLM struct {
	mu     sync.Mutex
	calls  int
	text   string
	err    error
	gate   chan struct{} // when non-nil, each call waits for a token
}

func (f *correctionLLM) ChatStream(context.Context, string, []llm.Message, func(string)) (string, error) {
	return "", nil
}
func (f *correctionLLM) Complete(context.Context, string, []llm.Message, bool) (string, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.text, f.err
}
func (f *correctionLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const doneCorrection = `{"corrected":"오늘은 비가 와요.","translation":"It rains today.","issues":[{"type":"grammar","span":"와","suggestion":"와요","explanation":"polite ending","explanationTranslation":"해요체"}]}`

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

func TestWritingListHandlerIncludesStoredAnswerAndCheckResult(t *testing.T) {
	st := &fakeWritingStore{prompts: []writing.Prompt{{
		ID: "p1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone,
		Answer: "오늘 비가 와", CheckStatus: writing.CheckDone, Corrected: "오늘은 비가 와요.",
		Issues: []protocol.Issue{{Type: "grammar", Span: "와", Suggestion: "와요", Explanation: "polite ending", ExplanationTranslation: "해요체"}},
	}}}
	rec := httptest.NewRecorder()
	writingListHandler(fakeIdentifier{id: "alex", ok: true}, st)(rec, httptest.NewRequest("GET", "/api/writing", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	for field, want := range map[string]string{"answer": "오늘 비가 와", "checkStatus": "done", "corrected": "오늘은 비가 와요."} {
		if items[0][field] != want {
			t.Fatalf("body[%q] = %v, want %q (body = %s)", field, items[0][field], want, rec.Body.String())
		}
	}
	issues, ok := items[0]["issues"].([]any)
	if !ok || len(issues) != 1 || issues[0].(map[string]any)["suggestion"] != "와요" {
		t.Fatalf("issues = %v", items[0]["issues"])
	}
}

func TestWritingInstanceHandlerDoesNotExposeAnotherUsersPrompt(t *testing.T) {
	st := &fakeWritingStore{prompts: []writing.Prompt{{ID: "p1", UserID: "other", Korean: "비밀", Status: writing.StatusDone}}}
	h := writingInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/writing/p1", nil)
	req.SetPathValue("id", "p1")
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWritingInstanceHandlerRestoresStoredAnswerAndResult(t *testing.T) {
	model := &correctionLLM{text: doneCorrection}
	st := &fakeWritingStore{prompts: []writing.Prompt{{
		ID: "p1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone,
		Answer: "오늘 비가 와", CheckStatus: writing.CheckDone, Corrected: "오늘은 비가 와요.",
	}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/writing/p1", nil)
	req.SetPathValue("id", "p1")
	writingInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, nil)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body writingPromptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Answer != "오늘 비가 와" || body.CheckStatus != writing.CheckDone || body.Corrected != "오늘은 비가 와요." {
		t.Fatalf("restored fields = %+v", body)
	}
	if model.callCount() != 0 {
		t.Fatalf("a finished check re-invoked the model %d times", model.callCount())
	}
}

// TestWritingInstanceHandlerRedispatchesLostCheck covers a submitted answer
// whose dispatch never ran (e.g. the worker died before claiming it): every
// poll re-dispatches, but the prompt-keyed dedupe collapses the polls into
// exactly one grading run, and the result lands on the stored answer.
func TestWritingInstanceHandlerRedispatchesLostCheck(t *testing.T) {
	gate := make(chan struct{})
	model := &correctionLLM{text: doneCorrection, gate: gate}
	st := &fakeWritingStore{prompts: []writing.Prompt{{
		ID: "redispatch-1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone,
		Answer: "오늘 비가 와", CheckStatus: writing.CheckPending,
	}}}
	handler := writingInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, nil)
	send := func() writingPromptResponse {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/writing/redispatch-1", nil)
		req.SetPathValue("id", "redispatch-1")
		handler(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var body writingPromptResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	// Two polls while the first (inline) run is blocked in the model: both
	// report pending, and the dedupe keeps them a single generation.
	for range 2 {
		if body := send(); body.CheckStatus != writing.CheckPending {
			t.Fatalf("poll during run: %+v", body)
		}
	}
	close(gate)
	waitForCondition(t, 5*time.Second, func() bool {
		p, ok := st.snapshot("redispatch-1")
		return ok && p.CheckStatus == writing.CheckDone
	})
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one deduped grading run", got)
	}
	if body := send(); body.CheckStatus != writing.CheckDone || body.Corrected != "오늘은 비가 와요." {
		t.Fatalf("final poll: %+v", body)
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

// TestWritingCheckHandlerPersistsAnswerAndDefersGrading is the core fix: the
// submit response carries no grade, the answer is stored, and the grade
// arrives asynchronously onto the stored row.
func TestWritingCheckHandlerPersistsAnswerAndDefersGrading(t *testing.T) {
	gate := make(chan struct{})
	model := &correctionLLM{text: doneCorrection, gate: gate}
	st := &fakeWritingStore{prompts: []writing.Prompt{{
		ID: "check-1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone,
	}}}
	rec := httptest.NewRecorder()
	writingCheckHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, nil)(
		rec, httptest.NewRequest("POST", "/api/writing/check", strings.NewReader(`{"promptId":"check-1","answer":"오늘 비가 와"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body writingPromptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CheckStatus != writing.CheckPending || body.Answer != "오늘 비가 와" || body.Corrected != "" {
		t.Fatalf("submit response = %+v, want pending with no grade", body)
	}
	// The answer is durably stored before the model is even consulted, so
	// navigating away now cannot lose it.
	if p, ok := st.snapshot("check-1"); !ok || p.Answer != "오늘 비가 와" || p.CheckStatus != writing.CheckPending {
		t.Fatalf("stored prompt before grading = %+v ok=%v", p, ok)
	}
	close(gate)
	waitForCondition(t, 5*time.Second, func() bool {
		p, ok := st.snapshot("check-1")
		return ok && p.CheckStatus == writing.CheckDone
	})
	p, _ := st.snapshot("check-1")
	if p.Corrected != "오늘은 비가 와요." || len(p.Issues) != 1 || p.Issues[0].Suggestion != "와요" {
		t.Fatalf("graded prompt = %+v", p)
	}
}

func TestWritingCheckHandlerRejectsUnknownOrUnreadyPrompt(t *testing.T) {
	model := &correctionLLM{text: doneCorrection}
	st := &fakeWritingStore{prompts: []writing.Prompt{
		{ID: "pending", UserID: "alex", Status: writing.StatusPending},
	}}
	for _, tc := range []struct{ name, body string; want int }{
		{"no prompt id", `{"answer":"x"}`, 400},
		{"unknown prompt", `{"promptId":"ghost","answer":"x"}`, 404},
		{"prompt not generated yet", `{"promptId":"pending","answer":"x"}`, 409},
		{"empty answer", `{"promptId":"pending","answer":"  "}`, 400},
	} {
		rec := httptest.NewRecorder()
		writingCheckHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, nil)(
			rec, httptest.NewRequest("POST", "/api/writing/check", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Fatalf("%s: status = %d body = %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	if model.callCount() != 0 {
		t.Fatalf("rejected submissions consulted the model %d times", model.callCount())
	}
}

func TestWritingCheckHandlerGradesDeletedPromptNowhere(t *testing.T) {
	// Deleting between the read and the durable start must not leave a
	// pending check behind, and the response is 404.
	st := &raceDeleteWritingStore{fakeWritingStore: &fakeWritingStore{prompts: []writing.Prompt{
		{ID: "gone", UserID: "alex", Korean: "문장", Status: writing.StatusDone},
	}}}
	model := &correctionLLM{text: doneCorrection}
	rec := httptest.NewRecorder()
	writingCheckHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, nil)(
		rec, httptest.NewRequest("POST", "/api/writing/check", strings.NewReader(`{"promptId":"gone","answer":"답"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if model.callCount() != 0 {
		t.Fatalf("deleted prompt consulted the model %d times", model.callCount())
	}
}

// raceDeleteWritingStore deletes the prompt right after the handler's Get,
// simulating a deletion that wins the race against the check start.
type raceDeleteWritingStore struct {
	*fakeWritingStore
	once sync.Once
}

func (r *raceDeleteWritingStore) Get(ctx context.Context, userID, id string) (writing.Prompt, error) {
	p, err := r.fakeWritingStore.Get(ctx, userID, id)
	r.once.Do(func() { _ = r.Delete(context.Background(), userID, id) })
	return p, err
}

func TestWritingRecheckHandlerRestartsCheckWithoutRetyping(t *testing.T) {
	gate := make(chan struct{})
	model := &correctionLLM{text: doneCorrection, gate: gate}
	st := &fakeWritingStore{prompts: []writing.Prompt{{
		ID: "recheck-1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone,
		Answer: "오늘 비가 와", CheckStatus: writing.CheckFailed, Corrected: "stale",
	}}}
	recheck := func() writingPromptResponse {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/writing/recheck-1/recheck", nil)
		req.SetPathValue("id", "recheck-1")
		writingRecheckHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, nil)(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var body writingPromptResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := recheck(); body.CheckStatus != writing.CheckPending || body.Corrected != "" {
		t.Fatalf("recheck response = %+v, want pending with the stale result cleared", body)
	}
	// A second click while the recheck runs claims nothing and pays nothing.
	if body := recheck(); body.CheckStatus != writing.CheckPending {
		t.Fatalf("second recheck = %+v", body)
	}
	close(gate)
	waitForCondition(t, 5*time.Second, func() bool {
		p, ok := st.snapshot("recheck-1")
		return ok && p.CheckStatus == writing.CheckDone
	})
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want exactly the one recheck run", got)
	}
	p, _ := st.snapshot("recheck-1")
	if p.Answer != "오늘 비가 와" || p.Corrected != "오늘은 비가 와요." {
		t.Fatalf("rechecked prompt = %+v", p)
	}
}

func TestWritingRecheckHandlerWithoutStoredAnswerIsANoOp(t *testing.T) {
	st := &fakeWritingStore{prompts: []writing.Prompt{{ID: "fresh", UserID: "alex", Korean: "문장", Status: writing.StatusDone}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/writing/fresh/recheck", nil)
	req.SetPathValue("id", "fresh")
	writingRecheckHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body writingPromptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CheckStatus != "" {
		t.Fatalf("recheck without an answer started a check: %+v", body)
	}
}
