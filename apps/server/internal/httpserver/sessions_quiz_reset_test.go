package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

func postQuizResetRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/sessions/s1/quiz/reset", nil)
	req.SetPathValue("id", "s1")
	return req
}

// doneQuizFixture builds an ended session whose quiz already finished
// generating with real content — the exact state sessionQuizResetHandler is
// meant to regenerate, unlike sessionRestudyHandler's "done but empty" only
// case.
func doneQuizFixture() ([]store.Turn, store.SessionMeta) {
	turns := []store.Turn{{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{Issues: []protocol.Issue{{Type: "grammar"}}}}}
	meta := store.SessionMeta{
		ID:         "s1",
		Ended:      true,
		QuizStatus: store.JobStatusDone,
		Quiz:       []protocol.QuizQuestion{{Prompt: "He ___ to school.", Answer: "goes"}},
	}
	return turns, meta
}

// TestSessionQuizResetHandlerNoQueueEventuallyRegeneratesQuiz guards the core
// flow (no-Redis fallback): RestartStudyQuiz must be called (clearing the old
// quiz back to pending) and, from there, the same background regeneration
// sessionEndHandler already uses — landing as JobStatusDone with a fresh set
// of questions.
func TestSessionQuizResetHandlerNoQueueEventuallyRegeneratesQuiz(t *testing.T) {
	turns, meta := doneQuizFixture()
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"questions":[{"prompt":"He ___ to school.","answer":"goes","answerMeaning":"가다","translation":"그는 학교에 가요.","explanation":"subject-verb agreement","explanationTranslation":"주어-동사 일치"}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if st.snapshotRestartQuizCalls() != 1 {
		t.Fatalf("RestartStudyQuiz calls = %d, want 1", st.snapshotRestartQuizCalls())
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteQuizCalls() == 1 })
}

// TestSessionQuizResetHandlerWithQueueEnqueuesDurableJob mirrors
// TestSessionRestudyHandlerWithQueueEnqueuesDurableJob: the regeneration must
// go through asyncjob.KindStudyQuiz (context.Background()-scoped), not run
// inline tied to this request.
func TestSessionQuizResetHandlerWithQueueEnqueuesDurableJob(t *testing.T) {
	rdb := requireRedis(t)
	queue := asyncjob.NewQueue(rdb)
	turns, meta := doneQuizFixture()
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"questions":[{"prompt":"He ___ to school.","answer":"goes","answerMeaning":"가다","translation":"그는 학교에 가요.","explanation":"subject-verb agreement","explanationTranslation":"주어-동사 일치"}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, queue)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteQuizCalls() == 1 })
}

// TestSessionQuizResetHandlerAllowsRegeneratingNonEmptyQuiz documents the key
// difference from sessionRestudyHandler: an already-populated quiz is a
// valid target for reset, not just a stuck-empty one — the whole point of
// "퀴즈 다시 만들기" is to replace real questions with a fresh set.
func TestSessionQuizResetHandlerAllowsRegeneratingNonEmptyQuiz(t *testing.T) {
	turns, meta := doneQuizFixture()
	if len(meta.Quiz) == 0 {
		t.Fatalf("fixture must start with a non-empty quiz")
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if st.snapshotRestartQuizCalls() != 1 {
		t.Fatalf("RestartStudyQuiz calls = %d, want 1", st.snapshotRestartQuizCalls())
	}
}

// TestSessionQuizResetHandlerAcceptsLegacyEmptyStatus guards a session ended
// before quiz pre-generation existed, which reads QuizStatus as "" rather
// than JobStatusDone (see store.SessionMeta.QuizStatus's doc comment).
func TestSessionQuizResetHandlerAcceptsLegacyEmptyStatus(t *testing.T) {
	turns, meta := doneQuizFixture()
	meta.QuizStatus = ""
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"questions":[{"prompt":"He ___ to school.","answer":"goes","answerMeaning":"가다","translation":"그는 학교에 가요.","explanation":"subject-verb agreement","explanationTranslation":"주어-동사 일치"}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if st.snapshotRestartQuizCalls() != 1 {
		t.Fatalf("RestartStudyQuiz calls = %d, want 1", st.snapshotRestartQuizCalls())
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteQuizCalls() == 1 })
}

// TestSessionQuizResetHandlerRejectsWhenStillPending guards against
// double-triggering a regeneration that's already in flight.
func TestSessionQuizResetHandlerRejectsWhenStillPending(t *testing.T) {
	turns, meta := doneQuizFixture()
	meta.QuizStatus = store.JobStatusPending
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if st.snapshotRestartQuizCalls() != 0 {
		t.Fatalf("RestartStudyQuiz calls = %d, want 0", st.snapshotRestartQuizCalls())
	}
}

// TestSessionQuizResetHandlerRejectsWhenNotEnded guards against forcing a
// quiz on a room that hasn't even been ended yet.
func TestSessionQuizResetHandlerRejectsWhenNotEnded(t *testing.T) {
	turns, meta := doneQuizFixture()
	meta.Ended = false
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestSessionQuizResetHandlerNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{detailErr: store.ErrNotFound}
	h := sessionQuizResetHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSessionQuizResetHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionQuizResetHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizResetRequest(t))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
