package store

import (
	"context"
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
