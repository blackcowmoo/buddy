package wordreview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"buddy/server/internal/mysqlerr"
	"buddy/server/internal/workguard"
)

func migrateMeaningVersions(ctx context.Context, db *sql.DB) error {
	if err := addWordColumns("meaning_target_version INT NOT NULL DEFAULT 0")(ctx, db); err != nil {
		return err
	}
	// Legacy workers advanced meaning_version on completion, before confirmation.
	// Keep that as the result version, crediting only explicit learner choices.
	// The target guard also makes a partially applied migration safe to replay.
	_, err := db.ExecContext(ctx, `UPDATE `+table+` SET meaning_target_version=meaning_version,
		meaning_version=CASE WHEN meaning_status='confirmed' THEN meaning_version ELSE 0 END
		WHERE meaning_target_version=0 AND meaning_version>0`)
	return err
}

func (s *MySQLStore) StartMeaningCleanup(ctx context.Context, userID string) error {
	// The target version prevents every poll from redoing an unconfirmed result
	// or retrying a failure. A newer version invalidates older worker snapshots
	// and browser choices through the revision, including formerly confirmed rows.
	_, err := s.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status='pending', meaning_error='',
		meaning_target_version=?, meaning_revision=meaning_revision+1
		WHERE user_id=? AND status=? AND meaning_version<? AND meaning_target_version<?`,
		CurrentMeaningVersion, userID, StatusVerified, CurrentMeaningVersion, CurrentMeaningVersion)
	return err
}

func (s *MySQLStore) PendingMeanings(ctx context.Context, userID string) ([]Word, error) {
	// Workers must see the intent just written by StartMeaningCleanup even
	// when the read replica has not caught up yet.
	rows, err := s.rw.QueryContext(ctx, `SELECT `+wordColumns+` FROM `+table+`
		WHERE user_id=? AND status=? AND meaning_status='pending' AND meaning_version<? AND meaning_target_version=? ORDER BY id`,
		userID, StatusVerified, CurrentMeaningVersion, CurrentMeaningVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var words []Word
	for rows.Next() {
		word, err := scanWord(rows, userID)
		if err != nil {
			return nil, err
		}
		words = append(words, word)
	}
	return words, rows.Err()
}

func (s *MySQLStore) SaveMeaning(ctx context.Context, before Word, meaning string) error {
	if before.MeaningTargetVersion != CurrentMeaningVersion {
		return nil
	}
	// A compare-and-swap prevents a delayed worker from overwriting a later
	// edit. Only gloss metadata changes; even a review completed during the
	// model call keeps its schedule, counts, and existing same-sense question.
	res, err := workguard.Executor(ctx, s.rw).ExecContext(ctx, `UPDATE `+table+`
		SET previous_meaning=COALESCE(previous_meaning, meaning), meaning=?, meaning_status='done', meaning_error=''
		WHERE id=? AND user_id=? AND status=? AND meaning_status='pending' AND meaning_version<?
		AND meaning_revision=? AND meaning_target_version=?
		AND BINARY meaning=BINARY ? AND BINARY word=BINARY ? AND BINARY example=BINARY ?`,
		meaning, before.ID, before.UserID, StatusVerified, CurrentMeaningVersion, before.MeaningRevision, before.MeaningTargetVersion, before.Meaning, before.Word, before.Example)
	if mysqlerr.Is(err, 1062) {
		// Do not merge or delete duplicate study cards: either may have progress
		// the learner wants to keep. Record a recoverable conflict instead.
		return s.FailMeaning(ctx, before, "같은 단어와 뜻의 항목이 있어 기존 뜻을 유지했어요.")
	}
	if err != nil {
		return fmt.Errorf("word meaning: save: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return s.FailMeaning(ctx, before, "정리 중 단어가 변경되어 기존 뜻을 유지했어요.")
	}
	return nil
}

func (s *MySQLStore) FailMeaning(ctx context.Context, before Word, reason string) error {
	if before.MeaningTargetVersion != CurrentMeaningVersion {
		return nil
	}
	_, err := workguard.Executor(ctx, s.rw).ExecContext(ctx, `UPDATE `+table+` SET meaning_status='failed', meaning_error=?
		WHERE id=? AND user_id=? AND meaning_status='pending' AND meaning_version<? AND meaning_revision=? AND meaning_target_version=?`,
		reason, before.ID, before.UserID, CurrentMeaningVersion, before.MeaningRevision, before.MeaningTargetVersion)
	return err
}

func (s *MySQLStore) SelectMeaning(ctx context.Context, userID, id string, choice MeaningChoice, revision int) (Word, error) {
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
	if w.Status != StatusVerified || w.MeaningRevision != revision {
		return Word{}, ErrMeaningConflict
	}
	switch choice {
	case MeaningChoiceCleaned:
		if w.MeaningStatus == MeaningConfirmed {
			return w, nil
		}
		if w.MeaningStatus != MeaningDone || w.MeaningTargetVersion == 0 || w.MeaningTargetVersion < w.MeaningVersion {
			return Word{}, ErrMeaningConflict
		}
		w.MeaningStatus = MeaningConfirmed
		w.MeaningVersion = w.MeaningTargetVersion
		w.ResearchStatus, w.ResearchResults = ResearchConfirmed, nil
	case MeaningChoiceOriginal:
		if w.MeaningStatus != MeaningDone && w.MeaningStatus != MeaningFailed {
			return Word{}, ErrMeaningConflict
		}
		if w.MeaningTargetVersion > CurrentMeaningVersion {
			return Word{}, ErrMeaningConflict
		}
		if w.PreviousMeaning != "" {
			w.Meaning = w.PreviousMeaning
		}
		w.MeaningStatus, w.MeaningTargetVersion = MeaningPending, CurrentMeaningVersion
		w.MeaningRevision++
	default:
		return Word{}, ErrMeaningConflict
	}
	w.PreviousMeaning, w.MeaningError = "", ""
	query := `UPDATE ` + table + ` SET meaning=?, previous_meaning=NULL, meaning_status=?, meaning_version=?, meaning_target_version=?, meaning_revision=?, meaning_error=''`
	args := []any{w.Meaning, w.MeaningStatus, w.MeaningVersion, w.MeaningTargetVersion, w.MeaningRevision}
	if choice == MeaningChoiceCleaned {
		query += `, research_status=?, research_results=NULL`
		args = append(args, ResearchConfirmed)
	}
	_, err = tx.ExecContext(ctx, query+` WHERE id=? AND user_id=?`, append(args, id, userID)...)
	if mysqlerr.Is(err, 1062) {
		// Restoring an old gloss can collide with a card added since cleanup.
		// Keep both cards and their progress intact for the learner to resolve.
		return Word{}, ErrMeaningConflict
	}
	if err != nil {
		return Word{}, fmt.Errorf("word meaning: select: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Word{}, err
	}
	return w, nil
}
