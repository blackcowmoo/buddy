package store

import (
	"context"
	"reflect"
	"testing"

	"buddy/server/internal/protocol"
)

// TestMySQLCompleteStudySummarySavesTextAndMarksDone guards the background
// job's terminal write: once the asyncjob.KindStudySummary job finishes, its
// text lands in StudySummary and StudySummaryStatus flips to JobStatusDone —
// the state a reopened room stops polling on.
func TestMySQLCompleteStudySummarySavesTextAndMarksDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "complete-summary-user"
	sessionID := "sess-complete"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	wantSummary := []protocol.StudySummarySentence{{English: "Focus on third-person -s.", Translation: "3인칭 단수 -s에 집중하세요."}}
	if err := st.CompleteStudySummary(ctx, userID, sessionID, wantSummary); err != nil {
		t.Fatalf("CompleteStudySummary() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != JobStatusDone {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusDone", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 1 || meta.StudySummary[0] != wantSummary[0] {
		t.Fatalf("StudySummary = %+v, want %+v", meta.StudySummary, wantSummary)
	}
}

// TestMySQLFailStudySummaryMarksFailedWithoutTouchingText guards the error
// path: a failed generation attempt must record JobStatusFailed for a
// poller to show, without inventing any StudySummary text.
func TestMySQLFailStudySummaryMarksFailedWithoutTouchingText(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "fail-summary-user"
	sessionID := "sess-fail"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.FailStudySummary(ctx, userID, sessionID); err != nil {
		t.Fatalf("FailStudySummary() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != JobStatusFailed {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusFailed", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 0 {
		t.Fatalf("StudySummary = %+v, want still empty after a failed attempt", meta.StudySummary)
	}
}

// TestMySQLRestartStudySummaryResetsDoneToPendingAndClearsSummary guards the
// manual "다시 확인하기" recovery path (see httpserver.sessionRestudyHandler):
// a wrap-up already landed as JobStatusDone — including one carrying real
// content — must go back to JobStatusPending with StudySummary cleared, so
// asyncjob.KindStudySummary regenerates it from scratch instead of the
// handler's guard (checked before this is ever called) being the only thing
// standing between a learner and silently wiping a good wrap-up.
func TestMySQLRestartStudySummaryResetsDoneToPendingAndClearsSummary(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "restart-summary-user"
	sessionID := "sess-restart"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.CompleteStudySummary(ctx, userID, sessionID, nil); err != nil {
		t.Fatalf("CompleteStudySummary() error = %v", err)
	}

	if err := st.RestartStudySummary(ctx, userID, sessionID); err != nil {
		t.Fatalf("RestartStudySummary() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != JobStatusPending || len(meta.StudySummary) != 0 {
		t.Fatalf("meta = %+v, want JobStatusPending with an empty summary", meta)
	}
}

// TestMySQLRestartStudySummaryMissingSessionIsNoop mirrors EndSession's "no
// row to match" behavior — RestartStudySummary never creates a session row
// on its own.
func TestMySQLRestartStudySummaryMissingSessionIsNoop(t *testing.T) {
	st := requireStore(t)
	if err := st.RestartStudySummary(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("RestartStudySummary() error = %v, want nil (silent no-op)", err)
	}
}

// TestMySQLCompleteStudyQuizSavesQuestionsAndMarksDone mirrors
// TestMySQLCompleteStudySummarySavesTextAndMarksDone: once the
// asyncjob.KindStudyQuiz job finishes, its questions land in Quiz and
// QuizStatus flips to JobStatusDone.
func TestMySQLCompleteStudyQuizSavesQuestionsAndMarksDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "complete-quiz-user"
	sessionID := "sess-complete-quiz"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	wantQuiz := []protocol.QuizQuestion{{Prompt: "He ___ to school.", Answer: "goes", Translation: "그는 학교에 가요.", Explanation: "subject-verb agreement", ExplanationTranslation: "주어-동사 일치"}}
	if err := st.CompleteStudyQuiz(ctx, userID, sessionID, wantQuiz); err != nil {
		t.Fatalf("CompleteStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusDone {
		t.Fatalf("QuizStatus = %q, want JobStatusDone", meta.QuizStatus)
	}
	if len(meta.Quiz) != 1 || !reflect.DeepEqual(meta.Quiz[0], wantQuiz[0]) {
		t.Fatalf("Quiz = %+v, want %+v", meta.Quiz, wantQuiz)
	}
}

// TestMySQLCompleteStudyQuizWithNoQuestionsStillMarksDone documents that an
// ended session with no flagged issues (runStudyQuiz skips the LLM call
// entirely — see its doc comment) still reaches a terminal QuizStatus, with
// Quiz staying empty rather than the JSON literal "[]" — same round-trip
// contract as CompleteStudySummary's empty-summary case.
func TestMySQLCompleteStudyQuizWithNoQuestionsStillMarksDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "complete-quiz-empty-user"
	sessionID := "sess-complete-quiz-empty"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.CompleteStudyQuiz(ctx, userID, sessionID, nil); err != nil {
		t.Fatalf("CompleteStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusDone {
		t.Fatalf("QuizStatus = %q, want JobStatusDone", meta.QuizStatus)
	}
	if len(meta.Quiz) != 0 {
		t.Fatalf("Quiz = %+v, want empty", meta.Quiz)
	}
}

// TestMySQLFailStudyQuizMarksFailedWithoutTouchingQuestions mirrors
// TestMySQLFailStudySummaryMarksFailedWithoutTouchingText.
func TestMySQLFailStudyQuizMarksFailedWithoutTouchingQuestions(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "fail-quiz-user"
	sessionID := "sess-fail-quiz"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.FailStudyQuiz(ctx, userID, sessionID); err != nil {
		t.Fatalf("FailStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusFailed {
		t.Fatalf("QuizStatus = %q, want JobStatusFailed", meta.QuizStatus)
	}
	if len(meta.Quiz) != 0 {
		t.Fatalf("Quiz = %+v, want still empty after a failed attempt", meta.Quiz)
	}
}

// TestMySQLMarkQuizCompletedSetsFlag guards the room-list checkmark: once
// set, QuizCompleted reads back true both from SessionDetail and
// ListSessions (the room list's own source, see sessionsListHandler).
func TestMySQLMarkQuizCompletedSetsFlag(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "mark-quiz-completed-user"
	sessionID := "sess-mark-quiz-completed"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizCompleted {
		t.Fatalf("QuizCompleted = true before MarkQuizCompleted, want false")
	}

	if err := st.MarkQuizCompleted(ctx, userID, sessionID); err != nil {
		t.Fatalf("MarkQuizCompleted() error = %v", err)
	}

	meta, _, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if !meta.QuizCompleted {
		t.Fatalf("QuizCompleted = false after MarkQuizCompleted, want true")
	}

	sessions, err := st.ListSessions(ctx, userID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || !sessions[0].QuizCompleted {
		t.Fatalf("ListSessions() = %+v, want QuizCompleted true", sessions)
	}
}

// TestMySQLMarkQuizCompletedMissingSessionIsNoop mirrors EndSession's "no
// row to match" behavior.
func TestMySQLMarkQuizCompletedMissingSessionIsNoop(t *testing.T) {
	st := requireStore(t)
	if err := st.MarkQuizCompleted(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("MarkQuizCompleted() error = %v, want nil (silent no-op)", err)
	}
}

// TestMySQLRestartStudyQuizResetsDoneToPendingClearsQuizAndCompleted guards
// the "퀴즈 다시 만들기" recovery path (see httpserver.sessionQuizResetHandler):
// unlike RestartStudySummary, this must reset a quiz that already carries
// real content, and must also clear QuizCompleted so a checkmark earned on
// the old questions doesn't silently carry over to ones the learner hasn't
// answered yet.
func TestMySQLRestartStudyQuizResetsDoneToPendingClearsQuizAndCompleted(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "restart-quiz-user"
	sessionID := "sess-restart-quiz"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	oldQuiz := []protocol.QuizQuestion{{Prompt: "He ___ to school.", Answer: "goes"}}
	if err := st.CompleteStudyQuiz(ctx, userID, sessionID, oldQuiz); err != nil {
		t.Fatalf("CompleteStudyQuiz() error = %v", err)
	}
	if err := st.MarkQuizCompleted(ctx, userID, sessionID); err != nil {
		t.Fatalf("MarkQuizCompleted() error = %v", err)
	}

	if err := st.RestartStudyQuiz(ctx, userID, sessionID); err != nil {
		t.Fatalf("RestartStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusPending || len(meta.Quiz) != 0 || meta.QuizCompleted {
		t.Fatalf("meta = %+v, want JobStatusPending with an empty quiz and QuizCompleted false", meta)
	}
}

// TestMySQLRestartStudyQuizMissingSessionIsNoop mirrors EndSession's "no row
// to match" behavior.
func TestMySQLRestartStudyQuizMissingSessionIsNoop(t *testing.T) {
	st := requireStore(t)
	if err := st.RestartStudyQuiz(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("RestartStudyQuiz() error = %v, want nil (silent no-op)", err)
	}
}
