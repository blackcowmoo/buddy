package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"buddy/server/internal/protocol"
)

func (s *MySQLStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error {
	// The session row is created lazily by whichever turn lands first for a
	// room — either the opening greeting (turn 0, assistant) or the
	// learner's own turn 1 — so a room the learner opened shows up in
	// ListSessions/SessionDetail (and can be deleted from there) even if
	// they never answered the greeting, instead of leaving an invisible
	// orphan sitting in buddy_turns with no row in buddy_sessions to find it
	// by.
	if turn == 0 || (turn == 1 && role == "user") {
		if err := ensureSessionRow(ctx, s.rw, userID, sessionID, text); err != nil {
			return fmt.Errorf("store: ensure session: %w", err)
		}
	} else if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET updated_at = UNIX_TIMESTAMP() WHERE user_id = ? AND id = ?
	`, userID, sessionID); err != nil {
		// Every later turn also needs to bump updated_at — otherwise
		// ListSessions' ORDER BY updated_at DESC only reflects activity from
		// the room's first turn until the connection's 30s save ticker (or
		// its on-disconnect save, see transport.Handler) happens to fire,
		// which is what made the room list look unsorted (or need a second
		// back-navigation to catch up) right after a multi-turn chat.
		return fmt.Errorf("store: touch session: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+turnsTable+` (user_id, session_id, turn, role, text, refined, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE text = VALUES(text), refined = VALUES(refined), source = VALUES(source)
	`, userID, sessionID, turn, role, text, refined, source); err != nil {
		return fmt.Errorf("store: save turn: %w", err)
	}
	return nil
}

// LastTurn reads from s.rw (the primary), not s.ro: this value directly
// guards against corrupting the transcript in SaveTurn's caller (see
// internal/session.Session.Seed), so a stale, too-low read from a lagging
// replica would reopen the exact bug it exists to prevent — unlike Load/Save,
// which already tolerate replica lag by design (see MySQLConfig's comment).
func (s *MySQLStore) LastTurn(ctx context.Context, userID, sessionID string) (int, error) {
	var last int
	err := s.rw.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(turn), 0) FROM `+turnsTable+` WHERE user_id = ? AND session_id = ?
	`, userID, sessionID).Scan(&last)
	if err != nil {
		return 0, fmt.Errorf("store: last turn: %w", err)
	}
	return last, nil
}

// SaveCorrection writes the terminal Judge correction and marks its job
// "done" in one transaction — same atomicity reasoning as
// CompleteAssistantTurn, so a
// poller never sees a "done" correction-job status before the correction it
// belongs to is readable. The job update is a plain UPDATE (not an upsert),
// so it's a harmless no-op when no job was ever reserved for this turn (e.g.
// a caller that predates ReserveCorrectionJob, or a test double).
func (s *MySQLStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	return s.SaveCorrectionFinal(ctx, userID, sessionID, turn, c, false)
}

func (s *MySQLStore) SaveCorrectionFinal(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction, unread bool) error {
	corrJSON, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: encode correction: %w", err)
	}
	return s.withTx(ctx, "save correction", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE `+turnsTable+` SET
				correction_unread = CASE
					WHEN correction_stage = 'judge' AND BINARY correction = BINARY ? THEN correction_unread
					WHEN ? THEN 1
					ELSE 0
				END,
				correction = ?, correction_stage = 'judge'
			WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'user'
		`, string(corrJSON), unread, string(corrJSON), userID, sessionID, turn); err != nil {
			return fmt.Errorf("store: save correction: text: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE `+jobsTable+` SET status = ?, updated_at = UNIX_TIMESTAMP()
			WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = 'correction'
		`, JobStatusDone, userID, sessionID, turn); err != nil {
			return fmt.Errorf("store: save correction: job: %w", err)
		}
		return nil
	})
}

// SaveCorrectionPreview persists the first Chat-stage result without
// completing the durable correction job. The stage predicate prevents a
// delayed preview goroutine from overwriting a Judge result that completed
// first; this matters in the no-Redis path where both event writes are
// intentionally detached from the WebSocket request.
func (s *MySQLStore) SaveCorrectionPreview(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	corrJSON, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: encode correction preview: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+turnsTable+` SET correction = ?, correction_stage = 'chat', correction_unread = 0
		WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'user'
			AND correction_stage <> 'judge'
	`, string(corrJSON), userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: save correction preview: %w", err)
	}
	return nil
}

func (s *MySQLStore) MarkCorrectionRead(ctx context.Context, userID, sessionID string, turn int) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+turnsTable+` SET correction_unread = 0
		WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'user'
			AND correction_stage = 'judge'
	`, userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: mark correction read: %w", err)
	}
	return nil
}

// ReserveCorrectionJob writes a "pending" correction-job row for (userID,
// sessionID, turn) — see ReserveAssistantTurn's doc comment for the shared
// reasoning; unlike that method, there's no placeholder turn to insert here
// since the user turn under correction has already been saved by the time
// correction ever runs.
func (s *MySQLStore) ReserveCorrectionJob(ctx context.Context, userID, sessionID string, turn int) error {
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+jobsTable+` (user_id, session_id, turn, kind, status, created_at, updated_at)
		VALUES (?, ?, ?, 'correction', ?, UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE user_id = user_id
	`, userID, sessionID, turn, JobStatusPending); err != nil {
		return fmt.Errorf("store: reserve correction job: %w", err)
	}
	return nil
}

func (s *MySQLStore) SaveTranslation(ctx context.Context, userID, sessionID string, turn int, role, translation string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+turnsTable+` SET translation = ?
		WHERE user_id = ? AND session_id = ? AND turn = ? AND role = ?
	`, translation, userID, sessionID, turn, role); err != nil {
		return fmt.Errorf("store: save translation: %w", err)
	}
	return nil
}

// sessionTurns loads every turn in (userID, sessionID), ordered so that
// role = 'assistant' sorts after 'user' within a turn (false < true). Two
// LEFT JOINs against buddy_jobs — one for kind='reply' (assistant turns),
// one for kind='correction' (user turns) — surface each turn's job status,
// so the frontend can tell "still generating"/"still checking"
// (ReplyStatus/CorrectionStatus == JobStatusPending, no result yet) apart
// from "no job was ever tracked for this turn" (older turns saved before
// that job kind's tracking existed) — both look identical in buddy_turns
// alone.
// turnColumns is the column list sessionTurns and sessionTurnsPage both
// select, aliased to a `t` for the buddy_turns row and `rj`/`cj` for the
// reply/correction job-status LEFT JOINs turnJobJoins adds.
const turnColumns = `t.turn, t.role, t.text, t.refined, t.source, t.correction, t.translation, t.meta, t.created_at, rj.status, cj.status, t.correction_stage, t.correction_unread`

// turnJobJoins is the pair of job-status LEFT JOINs shared by sessionTurns
// and sessionTurnsPage — one for kind='reply' (assistant turns), one for
// kind='correction' (user turns) — kept as one fragment so the two queries'
// join conditions can't silently drift apart (e.g. when a column is added,
// see mysqlerr.ApplyAdditive's callers in this file). Assumes the query's
// FROM clause aliases the turns row as `t`.
const turnJobJoins = `
	LEFT JOIN ` + jobsTable + ` rj
		ON rj.user_id = t.user_id AND rj.session_id = t.session_id AND rj.turn = t.turn
		AND rj.kind = 'reply' AND t.role = 'assistant'
	LEFT JOIN ` + jobsTable + ` cj
		ON cj.user_id = t.user_id AND cj.session_id = t.session_id AND cj.turn = t.turn
		AND cj.kind = 'correction' AND t.role = 'user'
`

func (s *MySQLStore) sessionTurns(ctx context.Context, userID, sessionID string) ([]Turn, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+turnColumns+`
		FROM `+turnsTable+` t
		`+turnJobJoins+`
		WHERE t.user_id = ? AND t.session_id = ? ORDER BY t.turn ASC, t.role = 'assistant' ASC
	`, userID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: session detail: %w", err)
	}
	defer rows.Close()

	turns := []Turn{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("store: session detail: %w", err)
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session detail: %w", err)
	}
	return turns, nil
}

// sessionTurnsPage loads at most `limit` distinct turns older than
// beforeTurn (or the most recent `limit` turns overall when beforeTurn <=
// 0), same join and ordering as sessionTurns. The inner derived table picks
// the page's turn numbers (fetching one extra distinct turn beyond `limit`
// to detect whether older turns remain, without a second round trip to the
// database), and the outer join reuses it against the same turnsTable/
// jobsTable columns sessionTurns selects — one query total. The extra
// turn's rows (if any) are trimmed in Go once scanned, since a turn can
// contribute 1 or 2 rows (user and/or assistant) and only the row count,
// not the turn count, is known until they're read.
func (s *MySQLStore) sessionTurnsPage(ctx context.Context, userID, sessionID string, beforeTurn, limit int) ([]Turn, bool, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+turnColumns+`
		FROM (
			SELECT DISTINCT turn FROM `+turnsTable+`
			WHERE user_id = ? AND session_id = ? AND (? <= 0 OR turn < ?)
			ORDER BY turn DESC LIMIT ?
		) page
		JOIN `+turnsTable+` t ON t.turn = page.turn AND t.user_id = ? AND t.session_id = ?
		`+turnJobJoins+`
		ORDER BY t.turn ASC, t.role = 'assistant' ASC
	`, userID, sessionID, beforeTurn, beforeTurn, limit+1, userID, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("store: session detail: page: %w", err)
	}
	defer rows.Close()

	turns := []Turn{}
	distinctTurns := []int{} // ascending, first-seen order — matches ORDER BY t.turn ASC
	seen := map[int]bool{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, false, fmt.Errorf("store: session detail: page: %w", err)
		}
		if !seen[t.Turn] {
			seen[t.Turn] = true
			distinctTurns = append(distinctTurns, t.Turn)
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: session detail: page: %w", err)
	}

	hasMore := false
	if len(distinctTurns) > limit {
		hasMore = true
		oldest := distinctTurns[0] // ASC order, so the extra probed turn sorts first
		kept := turns[:0]
		for _, t := range turns {
			if t.Turn == oldest {
				continue
			}
			kept = append(kept, t)
		}
		turns = kept
	}
	return turns, hasMore, nil
}

// scanTurn scans one row of sessionTurns'/sessionTurnsPage's shared
// SELECT column list.
func scanTurn(rows *sql.Rows) (Turn, error) {
	var t Turn
	var refined, correctionUnread int
	var correctionJSON, translation, metaJSON, replyStatus, correctionStatus sql.NullString
	if err := rows.Scan(&t.Turn, &t.Role, &t.Text, &refined, &t.Source, &correctionJSON, &translation, &metaJSON, &t.CreatedAt, &replyStatus, &correctionStatus, &t.CorrectionStage, &correctionUnread); err != nil {
		return Turn{}, err
	}
	t.Refined = refined != 0
	if correctionJSON.Valid {
		var c protocol.Correction
		if err := json.Unmarshal([]byte(correctionJSON.String), &c); err == nil {
			t.Correction = &c
		}
	}
	t.Translation = translation.String
	if metaJSON.Valid {
		t.Meta = json.RawMessage(metaJSON.String)
	}
	t.ReplyStatus = replyStatus.String
	t.CorrectionStatus = correctionStatus.String
	t.CorrectionUnread = correctionUnread != 0
	return t, nil
}
