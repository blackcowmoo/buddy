package wordreview

import (
	"context"
	"fmt"

	"buddy/server/internal/mysqlerr"
	"buddy/server/internal/workguard"
)

func (s *MySQLStore) StartMeaningCleanup(ctx context.Context, userID string) error {
	_, err := s.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status='pending', meaning_error=''
		WHERE user_id=? AND status=? AND meaning_version<? AND meaning_status<>'pending'`, userID, StatusVerified, CurrentMeaningVersion)
	return err
}

func (s *MySQLStore) PendingMeanings(ctx context.Context, userID string) ([]Word, error) {
	// Workers must see the intent just written by StartMeaningCleanup even
	// when the read replica has not caught up yet.
	rows, err := s.rw.QueryContext(ctx, `SELECT `+wordColumns+` FROM `+table+`
		WHERE user_id=? AND status=? AND meaning_status='pending' AND meaning_version<? ORDER BY id`,
		userID, StatusVerified, CurrentMeaningVersion)
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
	// A compare-and-swap prevents a delayed worker from overwriting a later
	// edit. Only gloss metadata changes; even a review completed during the
	// model call keeps its schedule, counts, and existing same-sense question.
	res, err := workguard.Executor(ctx, s.rw).ExecContext(ctx, `UPDATE `+table+`
		SET previous_meaning=meaning, meaning=?, meaning_version=?, meaning_status='done', meaning_error=''
		WHERE id=? AND user_id=? AND status=? AND meaning_status='pending' AND meaning_version<?
		AND BINARY meaning=BINARY ? AND BINARY word=BINARY ? AND BINARY example=BINARY ?`,
		meaning, CurrentMeaningVersion, before.ID, before.UserID, StatusVerified, CurrentMeaningVersion, before.Meaning, before.Word, before.Example)
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
	_, err := workguard.Executor(ctx, s.rw).ExecContext(ctx, `UPDATE `+table+` SET meaning_status='failed', meaning_error=?
		WHERE id=? AND user_id=? AND meaning_status='pending' AND meaning_version<?`, reason, before.ID, before.UserID, CurrentMeaningVersion)
	return err
}
