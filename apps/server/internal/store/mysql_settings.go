package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *MySQLStore) GetInterlocutorStyle(ctx context.Context, userID string) (string, error) {
	return scanStringOrEmpty(ctx, s.ro, "get interlocutor style",
		`SELECT interlocutor_style FROM `+settingsTable+` WHERE user_id = ?`, userID)
}

func (s *MySQLStore) SaveInterlocutorStyle(ctx context.Context, userID, style string) error {
	// learner_profile has no column-level default (it postdates
	// interlocutor_style — see the schema above, and SaveLearnerProfile's
	// matching comment for the reverse case), so a first-ever write to this
	// user's settings row must supply it explicitly or MySQL's strict mode
	// rejects the insert; ON DUPLICATE KEY leaves an existing value alone
	// since it's absent from the UPDATE clause.
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+settingsTable+` (user_id, interlocutor_style, learner_profile, updated_at)
		VALUES (?, ?, '', UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE interlocutor_style = VALUES(interlocutor_style), updated_at = VALUES(updated_at)
	`, userID, style); err != nil {
		return fmt.Errorf("store: save interlocutor style: %w", err)
	}
	return nil
}

func (s *MySQLStore) GetLearnerProfile(ctx context.Context, userID string) (string, error) {
	return scanStringOrEmpty(ctx, s.ro, "get learner profile",
		`SELECT learner_profile FROM `+settingsTable+` WHERE user_id = ?`, userID)
}

func (s *MySQLStore) SaveLearnerProfile(ctx context.Context, userID, profile string) error {
	// interlocutor_style has no column-level default (predates
	// learner_profile — see the schema above), so a first-ever write to this
	// user's settings row must supply it explicitly or MySQL's strict mode
	// rejects the insert; ON DUPLICATE KEY leaves an existing value alone
	// since it's absent from the UPDATE clause.
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+settingsTable+` (user_id, interlocutor_style, learner_profile, updated_at)
		VALUES (?, '', ?, UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE learner_profile = VALUES(learner_profile), updated_at = VALUES(updated_at)
	`, userID, profile); err != nil {
		return fmt.Errorf("store: save learner profile: %w", err)
	}
	return nil
}

// GetWordAutoAddStatus reads from s.rw, not s.ro, for the same reason as
// JobStatus: httpserver's poll endpoint is checking on a write
// (StartWordAutoAdd/CompleteWordAutoAdd/FailWordAutoAdd) that may have just
// happened, and a stale read from a lagging replica would show "pending"
// long after the job actually finished.
func (s *MySQLStore) GetWordAutoAddStatus(ctx context.Context, userID string) (string, int, error) {
	var status string
	var count int
	err := s.rw.QueryRowContext(ctx, `
		SELECT word_auto_add_status, word_auto_add_count FROM `+settingsTable+` WHERE user_id = ?
	`, userID).Scan(&status, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: get word auto-add status: %w", err)
	}
	return status, count, nil
}

// StartWordAutoAdd, like SaveLearnerProfile above, must supply every
// NOT NULL TEXT column explicitly on a first-ever insert for this user's
// settings row, since MySQL's strict mode rejects a bare literal default on
// TEXT; ON DUPLICATE KEY leaves interlocutor_style/learner_profile alone.
func (s *MySQLStore) StartWordAutoAdd(ctx context.Context, userID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+settingsTable+` (user_id, interlocutor_style, learner_profile, word_auto_add_status, word_auto_add_count, updated_at)
		VALUES (?, '', '', ?, 0, UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE word_auto_add_status = VALUES(word_auto_add_status), word_auto_add_count = VALUES(word_auto_add_count), updated_at = VALUES(updated_at)
	`, userID, JobStatusPending); err != nil {
		return fmt.Errorf("store: start word auto-add: %w", err)
	}
	return nil
}

func (s *MySQLStore) CompleteWordAutoAdd(ctx context.Context, userID string, addedCount int) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+settingsTable+` SET word_auto_add_status = ?, word_auto_add_count = ?, updated_at = UNIX_TIMESTAMP() WHERE user_id = ?
	`, JobStatusDone, addedCount, userID); err != nil {
		return fmt.Errorf("store: complete word auto-add: %w", err)
	}
	return nil
}

func (s *MySQLStore) FailWordAutoAdd(ctx context.Context, userID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+settingsTable+` SET word_auto_add_status = ?, updated_at = UNIX_TIMESTAMP() WHERE user_id = ?
	`, JobStatusFailed, userID); err != nil {
		return fmt.Errorf("store: fail word auto-add: %w", err)
	}
	return nil
}
