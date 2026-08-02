package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReserveAssistantTurn writes a placeholder assistant-turn row plus a
// pending reply-job row, in one transaction, before the reply job is even
// enqueued. Both inserts are no-ops on a second call for the same
// (userID, sessionID, turn) — `ON DUPLICATE KEY UPDATE user_id = user_id`
// touches nothing — so a race between two callers (e.g. a reconnect racing
// the original connection's inline claim) can never clobber an
// already-completed row's text or job status.
func (s *MySQLStore) ReserveAssistantTurn(ctx context.Context, userID, sessionID string, turn int) error {
	return s.withTx(ctx, "reserve assistant turn", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO `+turnsTable+` (user_id, session_id, turn, role, text, refined, source, created_at)
			VALUES (?, ?, ?, 'assistant', '', 0, '', UNIX_TIMESTAMP())
			ON DUPLICATE KEY UPDATE user_id = user_id
		`, userID, sessionID, turn); err != nil {
			return fmt.Errorf("store: reserve assistant turn: placeholder: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO `+jobsTable+` (user_id, session_id, turn, kind, status, created_at, updated_at)
			VALUES (?, ?, ?, 'reply', ?, UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
			ON DUPLICATE KEY UPDATE user_id = user_id
		`, userID, sessionID, turn, JobStatusPending); err != nil {
			return fmt.Errorf("store: reserve assistant turn: job: %w", err)
		}
		return nil
	})
}

// CompleteAssistantTurn writes the finished reply text and marks its job
// done, atomically — so a poller (or another replica reloading Profile
// after this job ran elsewhere) never observes a "done" status with the
// old empty placeholder text, or vice versa.
func (s *MySQLStore) CompleteAssistantTurn(ctx context.Context, userID, sessionID string, turn int, text string) error {
	return s.withTx(ctx, "complete assistant turn", func(tx *sql.Tx) error {
		// Turn 0 is the opening greeting (see pipeline.StartConversation): its
		// text is only known now, at completion, not at ReserveAssistantTurn
		// time — so the session-row-creation side effect SaveTurn's turn==0
		// branch normally provides (a greeting-only room already visible to
		// ListSessions/SessionDetail) happens here instead.
		if turn == 0 {
			if err := ensureSessionRow(ctx, tx, userID, sessionID, text); err != nil {
				return fmt.Errorf("store: complete assistant turn: ensure session: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE `+turnsTable+` SET text = ? WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'assistant'
		`, text, userID, sessionID, turn); err != nil {
			return fmt.Errorf("store: complete assistant turn: text: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE `+jobsTable+` SET status = ?, updated_at = UNIX_TIMESTAMP()
			WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = 'reply'
		`, JobStatusDone, userID, sessionID, turn); err != nil {
			return fmt.Errorf("store: complete assistant turn: job: %w", err)
		}
		return nil
	})
}

func (s *MySQLStore) FailJob(ctx context.Context, userID, sessionID string, turn int, kind, errMsg string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+jobsTable+` SET status = ?, error = ?, updated_at = UNIX_TIMESTAMP()
		WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = ?
	`, JobStatusFailed, errMsg, userID, sessionID, turn, kind); err != nil {
		return fmt.Errorf("store: fail job: %w", err)
	}
	return nil
}

// JobStatus reads from s.rw (the primary), not s.ro: callers use this to
// decide whether to redo expensive work (see pipeline.ReplyJobHandler's
// idempotency guard), so a stale "not done yet" read from a lagging replica
// would cause a duplicate LLM call — the same reasoning as LastTurn.
func (s *MySQLStore) JobStatus(ctx context.Context, userID, sessionID string, turn int, kind string) (string, error) {
	return scanStringOrEmpty(ctx, s.rw, "job status", `
		SELECT status FROM `+jobsTable+` WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = ?
	`, userID, sessionID, turn, kind)
}

// AssistantTurnText reads one turn's assistant text — see the Store
// interface doc for why this exists alongside SessionDetail. Against s.rw,
// not s.ro, for the same reason as JobStatus: its caller is polling for a
// write that just happened, which a read replica may not have yet.
func (s *MySQLStore) AssistantTurnText(ctx context.Context, userID, sessionID string, turn int) (string, error) {
	return scanStringOrEmpty(ctx, s.rw, "assistant turn text", `
		SELECT text FROM `+turnsTable+` WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'assistant'
	`, userID, sessionID, turn)
}

// scanStringOrEmpty runs a single-row, single-column query and returns "" (no
// error) when the row doesn't exist — the shared shape behind JobStatus,
// AssistantTurnText, GetInterlocutorStyle, and GetLearnerProfile, which
// differ only in which db handle, query, and error label they use.
func scanStringOrEmpty(ctx context.Context, db *sql.DB, errLabel, query string, args ...any) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, query, args...).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: %s: %w", errLabel, err)
	}
	return v, nil
}
