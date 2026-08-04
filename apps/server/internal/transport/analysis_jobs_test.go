package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
	"buddy/server/internal/store"
)

// ---- correction ---------------------------------------------------------

func TestNewCorrectHookReturnsNilWithoutQueue(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	if hook := NewCorrectHook(pipe, newFakeStore(), nil, nil, nil, nil, nil); hook != nil {
		t.Fatalf("NewCorrectHook(nil queue) = %v, want nil (so correct() falls back to the direct in-process path)", hook)
	}
}

func TestCorrectHookFastPathPersistsAndCallsOnResult(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-correct", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{
			complete: `{"corrected":"He goes to school.","translation":"그는 학교에 간다","issues":[{"type":"grammar","span":"he go","suggestion":"he goes","explanation":"third person singular","explanationTranslation":"3인칭 단수"}]}`,
		}}},
		FeedbackLang: "ko",
	}
	hook := NewCorrectHook(pipe, st, nil, nil, nil, nil, queue)

	var gotCorrected, gotTranslation string
	var gotIssues []protocol.Issue
	var called bool
	hook(context.Background(), "alex", "sess-correct", 1, "he go school", "",
		func(corrected string, issues []protocol.Issue, translation string) {
			called = true
			gotCorrected, gotIssues, gotTranslation = corrected, issues, translation
		},
		func() { t.Fatalf("onFailure called unexpectedly") },
	)

	if !called {
		t.Fatalf("onResult was never called")
	}
	if gotCorrected != "He goes to school." {
		t.Fatalf("corrected = %q, want %q", gotCorrected, "He goes to school.")
	}
	if len(gotIssues) != 1 {
		t.Fatalf("issues = %+v, want exactly 1", gotIssues)
	}
	if gotTranslation != "그는 학교에 간다" {
		t.Fatalf("translation = %q", gotTranslation)
	}

	_, turns, err := st.SessionDetail(context.Background(), "alex", "sess-correct")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	var found bool
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "user" {
			found = true
			if tn.Correction == nil || tn.Correction.Corrected != "He goes to school." {
				t.Fatalf("persisted correction = %+v, want the analyzed result", tn.Correction)
			}
			if tn.Translation != "그는 학교에 간다" {
				t.Fatalf("persisted translation = %q", tn.Translation)
			}
		}
	}
	if !found {
		t.Fatalf("user turn 1 not found: %+v", turns)
	}
}

// TestCorrectHookFastPathCapturesVocabularyWord guards the queued path's
// wiring of captureCorrectionWords (see word_capture.go): a vocabulary issue
// coming out of the queued CorrectionJobHandler must land in the wordreview
// store, not just in the fast-path onResult callback.
func TestCorrectHookFastPathCapturesVocabularyWord(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-correct-capture", 1, "user", "I was very angry", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{
			complete: `{"corrected":"I was furious.","translation":"","issues":[{"type":"vocabulary","span":"very angry","suggestion":"furious","explanationTranslation":"몹시 화난"}]}`,
		}}},
		FeedbackLang: "ko",
	}
	words := newFakeWordReviewStore()
	hook := NewCorrectHook(pipe, st, words, nil, nil, nil, queue)

	hook(context.Background(), "alex", "sess-correct-capture", 1, "I was very angry", "",
		func(corrected string, issues []protocol.Issue, translation string) {},
		func() { t.Fatalf("onFailure called unexpectedly") },
	)

	list, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Word != "furious" {
		t.Fatalf("captured words = %+v, want exactly one 'furious'", list)
	}
}

// TestCorrectHookFastPathCallsOnFailureAndMarksJobFailed is the fix for the
// bug this file's other correction test doesn't cover: when analyze() itself
// errors (every candidate LLM call failing), the live connection must still
// hear about it via onFailure — not just have its grammar spinner hang
// forever — and the job's durable status must land as JobStatusFailed, not
// stay stuck on JobStatusPending indistinguishably from "still checking".
func TestCorrectHookFastPathCallsOnFailureAndMarksJobFailed(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-correct-fail", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis:     []pipeline.Candidate{{Model: "m", LLM: failingAnalysisLLM{}}},
		FeedbackLang: "ko",
	}
	hook := NewCorrectHook(pipe, st, nil, nil, nil, nil, queue)

	var onResultCalled, onFailureCalled bool
	hook(context.Background(), "alex", "sess-correct-fail", 1, "he go school", "",
		func(corrected string, issues []protocol.Issue, translation string) { onResultCalled = true },
		func() { onFailureCalled = true },
	)

	if onResultCalled {
		t.Fatalf("onResult was called, want only onFailure on an analyze() error")
	}
	if !onFailureCalled {
		t.Fatalf("onFailure was never called")
	}

	status, err := st.JobStatus(context.Background(), "alex", "sess-correct-fail", 1, "correction")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != store.JobStatusFailed {
		t.Fatalf("JobStatus() = %q, want %q", status, store.JobStatusFailed)
	}

	_, turns, err := st.SessionDetail(context.Background(), "alex", "sess-correct-fail")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "user" && tn.Correction != nil {
			t.Fatalf("persisted correction = %+v, want nil after a failed analysis", tn.Correction)
		}
	}
}

// TestCorrectionJobHandlerAutoFinalizesInstantSession guards
// maybeFinalizeInstantSession (see session_finalize.go): once a room marked
// instant/"오늘의 한 문장" gets its one real exchange's correction back, the
// session must finalize itself (EndSession) without any client ever calling
// the manual "종료" endpoint — the whole point being that this doesn't
// depend on the learner's browser tab/connection still being around (unlike
// apps/web/src/App.tsx's own client-side auto-end effect).
func TestCorrectionJobHandlerAutoFinalizesInstantSession(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-instant-auto", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.MarkInstant(ctx, "alex", "sess-instant-auto"); err != nil {
		t.Fatalf("MarkInstant() error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{
			complete: `{"corrected":"He goes to school.","translation":"","issues":[]}`,
		}}},
		FeedbackLang: "ko",
	}
	handler := CorrectionJobHandler(pipe, st, nil, nil, nil, nil, nil)

	payload := correctionJobPayload{UserID: "alex", SessionID: "sess-instant-auto", Turn: 1, Text: "he go school"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := handler(ctx, asyncjob.Job{ID: "job-1", Kind: asyncjob.KindCorrection, Payload: raw}); err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	meta, _, err := st.SessionDetail(ctx, "alex", "sess-instant-auto")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if !meta.Ended {
		t.Fatalf("meta.Ended = false, want the instant session to auto-finalize once its correction lands")
	}
}

// TestCorrectionJobHandlerLeavesNormalSessionOpen is the contrast case: a
// non-instant room's correction landing must never auto-end it — only the
// learner's own "종료" confirmation (httpserver.sessionEndHandler) may do
// that for a regular conversation.
func TestCorrectionJobHandlerLeavesNormalSessionOpen(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-normal-auto", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{
			complete: `{"corrected":"He goes to school.","translation":"","issues":[]}`,
		}}},
		FeedbackLang: "ko",
	}
	handler := CorrectionJobHandler(pipe, st, nil, nil, nil, nil, nil)

	payload := correctionJobPayload{UserID: "alex", SessionID: "sess-normal-auto", Turn: 1, Text: "he go school"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := handler(ctx, asyncjob.Job{ID: "job-1", Kind: asyncjob.KindCorrection, Payload: raw}); err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	meta, _, err := st.SessionDetail(ctx, "alex", "sess-normal-auto")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Ended {
		t.Fatalf("meta.Ended = true, want a normal (non-instant) session to stay open after its correction")
	}
}

// ---- live translation -----------------------------------------------------

func TestNewTranslateHookReturnsNilWithoutQueue(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	if hook := NewTranslateHook(pipe, newFakeStore(), nil); hook != nil {
		t.Fatalf("NewTranslateHook(nil queue) = %v, want nil (so translateAssistant() falls back to the direct in-process path)", hook)
	}
}

func TestTranslateHookFastPathPersistsAndCallsOnResult(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-translate", 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(context.Background(), "alex", "sess-translate", 1, "assistant", "Nice to meet you!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis:     []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: "만나서 반가워요!"}}},
		FeedbackLang: "ko",
	}
	hook := NewTranslateHook(pipe, st, queue)

	var got string
	var called bool
	hook(context.Background(), "alex", "sess-translate", 1, "Nice to meet you!", func(translation string) {
		called = true
		got = translation
	})

	if !called || got != "만나서 반가워요!" {
		t.Fatalf("onResult called=%v got=%q, want 만나서 반가워요!", called, got)
	}

	_, turns, err := st.SessionDetail(context.Background(), "alex", "sess-translate")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "assistant" && tn.Translation != "만나서 반가워요!" {
			t.Fatalf("persisted translation = %q", tn.Translation)
		}
	}
}

// ---- title ---------------------------------------------------------------

func TestTitleJobHandlerPersistsGeneratedTitle(t *testing.T) {
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-title", 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{LLM: fixedCompleteLLM{reply: "Hiking Trip Plans"}, ChatModel: "m"}
	handler := TitleJobHandler(pipe, st)

	payload := titleJobPayload{UserID: "alex", SessionID: "sess-title", Turn: 1, Transcript: []llm.Message{
		{Role: llm.RoleUser, Content: "I went hiking"},
		{Role: llm.RoleAssistant, Content: "Nice!"},
	}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := handler(context.Background(), asyncjob.Job{ID: "job-1", Kind: asyncjob.KindTitle, Payload: raw}); err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-title")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Hiking Trip Plans" {
		t.Fatalf("title = %q, want the generated title", meta.Title)
	}
}

func TestHandlerGenerateTitleUsesQueueWhenConfigured(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-handler-title", 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{LLM: fixedCompleteLLM{reply: "Queued Title"}, ChatModel: "m"}
	pipe.TitleHook = NewTitleHook(pipe, st, queue)
	h := NewHandler(pipe, nil, st, nil, nil, nil, nil)

	sess := session.New("sys")
	sess.AppendUser("first message")
	h.generateTitle("alex", "sess-handler-title", sess, 1, "assistant reply")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-handler-title")
		if err == nil && meta.Title == "Queued Title" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("title was never set to the generated value via the queue path")
}

// ---- test doubles ---------------------------------------------------------

// fakeAnalysisLLM is a minimal llm.Client double for the Analysis ensemble:
// Complete always returns the same canned response, ChatStream is unused.
type fakeAnalysisLLM struct{ complete string }

func (f fakeAnalysisLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (f fakeAnalysisLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete, nil
}

// failingAnalysisLLM is a Complete that always errors, for exercising the
// analyze()-fails path (see TestCorrectHookFastPathCallsOnFailureAndMarksJobFailed).
type failingAnalysisLLM struct{}

func (f failingAnalysisLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (f failingAnalysisLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return "", errFakeLLMUnavailable
}

// fixedCompleteLLM is a minimal llm.Client double for GenerateTitle (which
// calls Complete, not ChatStream).
type fixedCompleteLLM struct{ reply string }

func (f fixedCompleteLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (f fixedCompleteLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.reply, nil
}
