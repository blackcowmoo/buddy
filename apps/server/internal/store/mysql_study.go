package store

import (
	"context"
	"encoding/json"
	"fmt"

	"buddy/server/internal/protocol"
)

// CompleteStudySummary persists an asyncjob.KindStudySummary job's finished
// wrap-up and marks it done — see the Store interface doc comment. Encoded
// as JSON into the same study_summary TEXT column the pre-bilingual feature
// stored plain native-language text in (see the legacy-reset migration in
// NewMySQL) — an empty summary (no issues flagged) still stores as ”, not
// "[]", so it round-trips through decodeStudySummary the same way a
// never-completed one does.
func (s *MySQLStore) CompleteStudySummary(ctx context.Context, userID, sessionID string, summary []protocol.StudySummarySentence) error {
	encoded, err := encodeJSONSlice(summary)
	if err != nil {
		return fmt.Errorf("store: encode study summary: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET study_summary = ?, study_summary_status = ?
		WHERE user_id = ? AND id = ?
	`, encoded, JobStatusDone, userID, sessionID); err != nil {
		return fmt.Errorf("store: complete study summary: %w", err)
	}
	return nil
}

// encodeJSONSlice marshals items to a JSON array, except an empty/nil slice
// encodes as "" rather than "[]" — matching the legacy plain-text columns
// (study_summary, quiz) this backs, so a never-completed row and a
// completed-but-empty row are indistinguishable, both round-tripping to nil
// through decodeStudySummary/decodeQuiz.
func encodeJSONSlice[T any](items []T) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeStudySummary parses the study_summary column's JSON-encoded
// []protocol.StudySummarySentence — "" (never completed, or no issues were
// flagged) decodes to nil. A row left over from before this bilingual format
// existed would fail to parse as JSON (it's plain native-language prose); the
// legacy-reset migration in NewMySQL clears those back to "" on startup, but
// this still falls back to nil defensively rather than surfacing a decode
// error to the caller, since a missing wrap-up on old data is harmless.
func decodeStudySummary(raw string) []protocol.StudySummarySentence {
	if raw == "" {
		return nil
	}
	var sentences []protocol.StudySummarySentence
	if err := json.Unmarshal([]byte(raw), &sentences); err != nil {
		return nil
	}
	return sentences
}

// FailStudySummary records that an asyncjob.KindStudySummary job's LLM call
// errored — see the Store interface doc comment.
func (s *MySQLStore) FailStudySummary(ctx context.Context, userID, sessionID string) error {
	return s.failSessionJob(ctx, userID, sessionID, "study_summary_status", "fail study summary")
}

// RestartStudySummary resets a wrap-up back to JobStatusPending with an
// empty study_summary — see the Store interface doc comment. Unlike
// EndSession (which only ever moves a session pending the first time), this
// is meant to be called on an already-terminal row, so it clears
// study_summary explicitly rather than relying on it already being empty.
func (s *MySQLStore) RestartStudySummary(ctx context.Context, userID, sessionID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET study_summary = '', study_summary_status = ?
		WHERE user_id = ? AND id = ?
	`, JobStatusPending, userID, sessionID); err != nil {
		return fmt.Errorf("store: restart study summary: %w", err)
	}
	return nil
}

// CompleteStudyQuiz persists an asyncjob.KindStudyQuiz job's finished quiz
// and marks it done — mirrors CompleteStudySummary; an empty quiz (no issues
// flagged) still stores as ”, not "[]", so it round-trips through
// decodeQuiz the same way a never-completed one does.
func (s *MySQLStore) CompleteStudyQuiz(ctx context.Context, userID, sessionID string, questions []protocol.QuizQuestion) error {
	encoded, err := encodeJSONSlice(questions)
	if err != nil {
		return fmt.Errorf("store: encode quiz: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET quiz = ?, quiz_status = ?
		WHERE user_id = ? AND id = ?
	`, encoded, JobStatusDone, userID, sessionID); err != nil {
		return fmt.Errorf("store: complete study quiz: %w", err)
	}
	return nil
}

// decodeQuiz parses the quiz column's JSON-encoded []protocol.QuizQuestion —
// mirrors decodeStudySummary. "" (never completed, or no issues were
// flagged) decodes to nil.
func decodeQuiz(raw string) []protocol.QuizQuestion {
	if raw == "" {
		return nil
	}
	var questions []protocol.QuizQuestion
	if err := json.Unmarshal([]byte(raw), &questions); err != nil {
		return nil
	}
	return questions
}

// FailStudyQuiz records that an asyncjob.KindStudyQuiz job's LLM call
// errored — mirrors FailStudySummary.
func (s *MySQLStore) FailStudyQuiz(ctx context.Context, userID, sessionID string) error {
	return s.failSessionJob(ctx, userID, sessionID, "quiz_status", "fail study quiz")
}

// failSessionJob marks a session-scoped async job (study summary or quiz) as
// failed by setting its status column — shared by FailStudySummary and
// FailStudyQuiz, which differ only in which column they update.
func (s *MySQLStore) failSessionJob(ctx context.Context, userID, sessionID, statusColumn, errLabel string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET `+statusColumn+` = ?
		WHERE user_id = ? AND id = ?
	`, JobStatusFailed, userID, sessionID); err != nil {
		return fmt.Errorf("store: %s: %w", errLabel, err)
	}
	return nil
}

// MarkQuizCompleted sets quiz_completed — see SessionMeta.QuizCompleted's
// doc comment. A plain UPDATE, not conditional on quiz_status/quiz content:
// callers (sessionQuizCompleteHandler) are what decide when this is the
// right call to make (either every question answered correctly, or the "내가
// 읽었음" acknowledgment for a quiz with nothing to answer).
func (s *MySQLStore) MarkQuizCompleted(ctx context.Context, userID, sessionID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET quiz_completed = 1
		WHERE user_id = ? AND id = ?
	`, userID, sessionID); err != nil {
		return fmt.Errorf("store: mark quiz completed: %w", err)
	}
	return nil
}

// RestartStudyQuiz resets a quiz back to JobStatusPending with an empty quiz
// and quiz_completed cleared — see the Store interface doc comment. Unlike
// RestartStudySummary, this is meant to be called on an already-terminal row
// regardless of whether it already carries real questions, so it clears
// quiz explicitly rather than relying on it already being empty.
func (s *MySQLStore) RestartStudyQuiz(ctx context.Context, userID, sessionID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET quiz = '', quiz_status = ?, quiz_completed = 0
		WHERE user_id = ? AND id = ?
	`, JobStatusPending, userID, sessionID); err != nil {
		return fmt.Errorf("store: restart study quiz: %w", err)
	}
	return nil
}
