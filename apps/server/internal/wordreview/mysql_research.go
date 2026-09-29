package wordreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"buddy/server/internal/mysqlerr"
	"buddy/server/internal/workguard"
)

func (s *MySQLStore) StartResearch(ctx context.Context, userID, id string) (Word, error) {
	if _, err := s.rw.ExecContext(ctx, `UPDATE `+table+` SET research_status=?, research_results=NULL,
		research_revision=research_revision+1 WHERE id=? AND user_id=? AND research_status<>?`, ResearchPending, id, userID, ResearchPending); err != nil {
		return Word{}, fmt.Errorf("wordreview: research start: %w", err)
	}
	// The worker and client must receive the revision just written, even when
	// the read replica has not caught up with the pending intent yet.
	w, err := scanWord(s.rw.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id=? AND user_id=?`, id, userID), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Word{}, nil
	}
	return w, err
}

func (s *MySQLStore) FinishResearch(ctx context.Context, before Word, results []ResearchSuggestion) (Word, error) {
	status := ResearchDone
	if len(results) == 0 {
		status = ResearchFailed
	}
	b, err := json.Marshal(results)
	if err != nil {
		return Word{}, err
	}
	// Bind JSON as text for MySQL's utf8mb4 JSON columns. The snapshot guards
	// prevent late workers from overwriting a new search or a learner's choice.
	db := workguard.Executor(ctx, s.rw)
	if _, err = db.ExecContext(ctx, `UPDATE `+table+` SET research_status=?, research_results=?
		WHERE id=? AND user_id=? AND research_status=? AND research_revision=?
		AND BINARY word=BINARY ? AND BINARY meaning=BINARY ? AND BINARY example=BINARY ?`,
		status, string(b), before.ID, before.UserID, ResearchPending, before.ResearchRevision, before.Word, before.Meaning, before.Example); err != nil {
		return Word{}, fmt.Errorf("wordreview: research finish: %w", err)
	}
	w, err := scanWord(db.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id=? AND user_id=?`, before.ID, before.UserID), before.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return Word{}, nil
	}
	return w, err
}

// SelectResearch replaces the candidate in place: Save's duplicate lookup plus
// a browser-side Delete could return the old rejected example and then delete
// that very same row. A transaction preserves identity and all review progress.
func (s *MySQLStore) SelectResearch(ctx context.Context, userID, id string, revision int, choice ResearchSuggestion) (Word, error) {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return Word{}, err
	}
	defer tx.Rollback()
	w, err := scanWord(tx.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id=? AND user_id=? FOR UPDATE`, id, userID), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Word{}, nil
	}
	if err != nil {
		return Word{}, err
	}
	if w.Status == StatusPending || MeaningNeedsReview(w) || w.ResearchStatus != ResearchDone || w.ResearchRevision != revision {
		return Word{}, ErrResearchConflict
	}
	matched := false
	for _, result := range w.ResearchResults {
		if result.Verified && result.Word == choice.Word && result.Meaning == choice.Meaning && result.Example == choice.Example {
			matched = true
			break
		}
	}
	if !matched {
		return Word{}, ErrResearchConflict
	}
	w.Word, w.Meaning, w.Example = choice.Word, choice.Meaning, choice.Example
	w.Status, w.VerifyReason = StatusVerified, ""
	w.ResearchStatus, w.ResearchResults = ResearchNone, nil
	w.ReviewQuestion = Question{}
	w.MeaningVersion, w.MeaningTargetVersion = CurrentMeaningVersion, CurrentMeaningVersion
	w.MeaningStatus, w.MeaningError, w.PreviousMeaning = MeaningConfirmed, "", ""
	w.MeaningRevision++
	_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET word=?, meaning=?, example=?, status=?, verify_reason='',
		research_status='', research_results=NULL, review_question_version=0, review_prompt='', review_answer='', review_answers=NULL,
		meaning_version=?, meaning_target_version=?, meaning_status=?, meaning_error='', previous_meaning=NULL, meaning_revision=?
		WHERE id=? AND user_id=?`, w.Word, w.Meaning, w.Example, w.Status, w.MeaningVersion, w.MeaningTargetVersion, w.MeaningStatus, w.MeaningRevision, id, userID)
	if mysqlerr.Is(err, 1062) {
		return Word{}, ErrResearchConflict
	}
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: research select: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Word{}, err
	}
	return w, nil
}
