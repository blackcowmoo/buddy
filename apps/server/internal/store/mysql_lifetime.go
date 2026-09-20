package store

import (
	"context"
	"database/sql"
	"sort"

	"buddy/server/internal/workguard"
)

const lifetimesTable = "buddy_session_lifetimes"

// WorkExists distinguishes a new lazy-created room from a deleted room. Read
// the primary: a stale replica must never authorize more work after deletion.
func (s *MySQLStore) WorkExists(ctx context.Context, userID, sessionID string) (bool, error) {
	var deleted bool
	err := s.rw.QueryRowContext(ctx, `SELECT deleted FROM `+lifetimesTable+` WHERE user_id=? AND session_id=?`, userID, sessionID).Scan(&deleted)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return !deleted, err
}

// All writes capable of creating a room/turn/job lock the same identity row
// as deletion. Checking before an upsert alone would leave a check/write race.
func lockSessionLifetime(ctx context.Context, tx *sql.Tx, userID, sessionID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+lifetimesTable+` (user_id, session_id) VALUES (?, ?) ON DUPLICATE KEY UPDATE session_id=VALUES(session_id)`, userID, sessionID); err != nil {
		return false, err
	}
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT deleted FROM `+lifetimesTable+` WHERE user_id=? AND session_id=? FOR UPDATE`, userID, sessionID).Scan(&deleted)
	return !deleted, err
}

func (s *MySQLStore) withLiveSession(ctx context.Context, userID, sessionID, label string, work func(*sql.Tx) error) error {
	return s.withTx(ctx, label, func(tx *sql.Tx) error {
		alive, err := lockSessionLifetime(ctx, tx, userID, sessionID)
		if err != nil {
			return err
		}
		if !alive {
			return workguard.ErrDeleted
		}
		return work(tx)
	})
}

// WorkCommit fences derived writes to other tables against session deletion.
func (s *MySQLStore) WorkCommit(ctx context.Context, userID, sessionID string, work func(context.Context) error) error {
	if tx := workguard.Tx(ctx, s.rw); tx != nil {
		alive, err := lockSessionLifetime(ctx, tx, userID, sessionID)
		if err != nil {
			return err
		}
		if !alive {
			return workguard.ErrDeleted
		}
		return work(ctx)
	}
	return s.withLiveSession(ctx, userID, sessionID, "session effect", func(tx *sql.Tx) error {
		return work(workguard.WithTx(ctx, s.rw, tx))
	})
}

func (s *MySQLStore) PublishLearnerProfile(ctx context.Context, userID string, sessionIDs []string, revision int64, profile string) error {
	ids := append([]string(nil), sessionIDs...)
	sort.Strings(ids)
	return s.withTx(ctx, "profile snapshot", func(tx *sql.Tx) error {
		for _, id := range ids {
			alive, err := lockSessionLifetime(ctx, tx, userID, id)
			if err != nil {
				return err
			}
			if !alive {
				return workguard.ErrDeleted
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+settingsTable+` (user_id, interlocutor_style, learner_profile, updated_at) VALUES (?, '', '', UNIX_TIMESTAMP()) ON DUPLICATE KEY UPDATE user_id=VALUES(user_id)`, userID); err != nil {
			return err
		}
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT learner_profile_revision FROM `+settingsTable+` WHERE user_id=? FOR UPDATE`, userID).Scan(&current); err != nil {
			return err
		}
		if current != revision {
			return ErrProfileChanged
		}
		return saveLearnerProfile(ctx, tx, userID, profile)
	})
}

func (s *MySQLStore) GetLearnerProfileSnapshot(ctx context.Context, userID string) (string, int64, error) {
	var profile string
	var revision int64
	err := s.rw.QueryRowContext(ctx, `SELECT learner_profile, learner_profile_revision FROM `+settingsTable+` WHERE user_id=?`, userID).Scan(&profile, &revision)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return profile, revision, err
}
