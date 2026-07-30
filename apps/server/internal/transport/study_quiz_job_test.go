package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

// TestRunStudyQuizSkipsLLMWhenNoIssuesFlagged mirrors
// TestRunStudySummarySkipsLLMWhenNoIssuesFlagged: no LLM call, an empty quiz
// persisted via CompleteStudyQuiz as JobStatusDone (not JobStatusFailed —
// unlike runStudySummary, an empty result here isn't ambiguous with an LLM
// hiccup, since sessionQuizHandler already represented "nothing to quiz" as
// an empty questions array before this job existed).
func TestRunStudyQuizSkipsLLMWhenNoIssuesFlagged(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLM{&calls, "should not be called"}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-quiz-1", 1, "user", "I like pizza.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-quiz-1", 1, protocol.Correction{Original: "I like pizza.", Corrected: "I like pizza."}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-quiz-1"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudyQuizInline(context.Background(), pipe, st, "alex", "sess-quiz-1"); err != nil {
		t.Fatalf("RunStudyQuizInline() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call when no issues were flagged, got %d calls", calls)
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-quiz-1")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != store.JobStatusDone || len(meta.Quiz) != 0 {
		t.Fatalf("meta = %+v, want JobStatusDone with an empty quiz", meta)
	}
}

// TestRunStudyQuizCollectsIssuesAndPersistsQuestions guards the primary
// flow: every issue from every user turn's correction reaches
// GenerateStudyQuiz, and the resulting questions are persisted as done.
func TestRunStudyQuizCollectsIssuesAndPersistsQuestions(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"questions":[{"prompt":"He ___ to school.","answer":"goes","translation":"그는 학교에 가요.","explanation":"subject-verb agreement","explanationTranslation":"주어-동사 일치"}]}`}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-quiz-2", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(context.Background(), "alex", "sess-quiz-2", 1, "assistant", "Nice!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-quiz-2", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-quiz-2"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudyQuizInline(context.Background(), pipe, st, "alex", "sess-quiz-2"); err != nil {
		t.Fatalf("RunStudyQuizInline() error = %v", err)
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-quiz-2")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != store.JobStatusDone {
		t.Fatalf("QuizStatus = %q, want JobStatusDone", meta.QuizStatus)
	}
	if len(meta.Quiz) != 1 || meta.Quiz[0].Answer != "goes" {
		t.Fatalf("Quiz = %+v", meta.Quiz)
	}
}

// TestRunStudyQuizGenerateErrorMarksFailed guards the error path: a failed
// LLM call must not persist any quiz, must record JobStatusFailed, and must
// propagate the error so asyncjob's reaper retries the job from scratch.
func TestRunStudyQuizGenerateErrorMarksFailed(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: failingAnalysisLLM{}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-quiz-3", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-quiz-3", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-quiz-3"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudyQuizInline(context.Background(), pipe, st, "alex", "sess-quiz-3"); err == nil {
		t.Fatalf("RunStudyQuizInline() error = nil, want the LLM error propagated")
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-quiz-3")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != store.JobStatusFailed {
		t.Fatalf("QuizStatus = %q, want JobStatusFailed", meta.QuizStatus)
	}
	if len(meta.Quiz) != 0 {
		t.Fatalf("Quiz = %+v, want still empty after a failed attempt", meta.Quiz)
	}
}

func TestStudyQuizJobHandlerBadPayload(t *testing.T) {
	handler := StudyQuizJobHandler(&pipeline.Pipeline{}, newFakeStore())
	job := asyncjob.Job{Kind: asyncjob.KindStudyQuiz, Payload: json.RawMessage(`not json`)}
	if err := handler(context.Background(), job); err == nil {
		t.Fatalf("handler(bad payload) error = nil, want an unmarshal error")
	}
}

// TestEnqueueStudyQuizJobRunsInBackgroundAndPersists mirrors
// TestEnqueueStudySummaryJobRunsInBackgroundAndPersists against real Redis.
func TestEnqueueStudyQuizJobRunsInBackgroundAndPersists(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"questions":[{"prompt":"He ___ to school.","answer":"goes","translation":"그는 학교에 가요.","explanation":"subject-verb agreement","explanationTranslation":"주어-동사 일치"}]}`}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-quiz-enqueue", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-quiz-enqueue", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-quiz-enqueue"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := EnqueueStudyQuizJob(context.Background(), queue, pipe, st, "alex", "sess-quiz-enqueue"); err != nil {
		t.Fatalf("EnqueueStudyQuizJob() error = %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-quiz-enqueue")
		return err == nil && meta.QuizStatus == store.JobStatusDone
	})
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-quiz-enqueue")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(meta.Quiz) != 1 || meta.Quiz[0].Answer != "goes" {
		t.Fatalf("Quiz = %+v", meta.Quiz)
	}
}
