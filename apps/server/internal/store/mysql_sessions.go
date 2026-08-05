package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"buddy/server/internal/llm"
)

func (s *MySQLStore) Load(ctx context.Context, userID, sessionID string) (Profile, error) {
	var summary, recentJSON string
	err := s.ro.QueryRowContext(ctx,
		`SELECT summary, recent FROM `+sessionsTable+` WHERE user_id = ? AND id = ?`, userID, sessionID,
	).Scan(&summary, &recentJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, nil
	}
	if err != nil {
		return Profile{}, fmt.Errorf("store: load: %w", err)
	}
	var recent []llm.Message
	if err := json.Unmarshal([]byte(recentJSON), &recent); err != nil {
		return Profile{}, fmt.Errorf("store: decode: %w", err)
	}
	return Profile{Summary: summary, Recent: recent}, nil
}

func (s *MySQLStore) Save(ctx context.Context, userID, sessionID string, p Profile) error {
	recentJSON, err := json.Marshal(p.Recent)
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}
	// A plain UPDATE, not an upsert: a session row only exists once its
	// first turn has been saved (see SaveTurn), so a connection that never
	// sent a message leaves nothing behind for ListSessions to show.
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET summary = ?, recent = ?, updated_at = UNIX_TIMESTAMP()
		WHERE user_id = ? AND id = ?
	`, p.Summary, string(recentJSON), userID, sessionID); err != nil {
		return fmt.Errorf("store: save: %w", err)
	}
	return nil
}

// maxTitleLen bounds the title derived from a session's opening message —
// long enough to be recognizable in a chat-room list, short enough to fit
// one line.
const maxTitleLen = 60

func truncateTitle(text string) string {
	text = strings.TrimSpace(text)
	r := []rune(text)
	if len(r) <= maxTitleLen {
		return text
	}
	return string(r[:maxTitleLen]) + "…"
}

// execer is satisfied by both *sql.DB and *sql.Tx, so ensureSessionRow can
// run either as a standalone statement or as part of a caller's transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureSessionRow creates the session row on first sight (either the
// opening greeting or the learner's own first message — see SaveTurn and
// CompleteAssistantTurn) and otherwise refreshes updated_at, only ever
// overwriting title while title_generated is still 0. That guard means a
// session that already has a real (possibly LLM-generated) title — e.g.
// after a "reset" that cleared buddy_turns but left buddy_sessions alone —
// keeps it instead of being clobbered back to a raw-text placeholder.
// SaveGeneratedTitle itself no longer relies on this flag to guard its own
// writes (see its doc comment) — title_generated now only feeds this check.
func ensureSessionRow(ctx context.Context, exec execer, userID, sessionID, text string) error {
	_, err := exec.ExecContext(ctx, `
		INSERT INTO `+sessionsTable+` (user_id, id, title, summary, recent, created_at, updated_at, study_summary, quiz)
		VALUES (?, ?, ?, '', '[]', UNIX_TIMESTAMP(), UNIX_TIMESTAMP(), '', '')
		ON DUPLICATE KEY UPDATE
			title = IF(title_generated = 0, VALUES(title), title),
			updated_at = VALUES(updated_at)
	`, userID, sessionID, truncateTitle(text))
	return err
}

// SaveGeneratedTitle overwrites a session's title with an LLM-generated one.
// Unlike SaveTurn's truncated-first-message placeholder (which is guarded by
// title_generated so a real title, once set, is never clobbered back to a
// placeholder — see ensureSessionRow), this call always takes effect: it's
// invoked not just once per session but periodically as the conversation
// continues (see internal/transport's TitleRegenerateEveryNTurns), and each
// call is meant to actually update the title to match the current topic.
// title_generated is still set to 1 on every call — that flag's only
// remaining job is telling ensureSessionRow a real title is in place, not
// gating this method's own writes.
//
// This must be an upsert, not a plain UPDATE: Handler.generateTitle (see
// ws.go) is fired off `go`, independently and unsynchronized, from the same
// turn-1 event that triggers SaveTurn's own session-row-creating write —
// there is no ordering guarantee between the two beyond "both eventually
// run". A plain UPDATE would silently affect 0 rows if this call reached
// the database first (no row to match yet), permanently losing the
// generated title with nothing to retry it. Creating the row here too, with
// title_generated already 1, makes the outcome correct regardless of which
// of the two writes lands first: if SaveTurn's own upsert (ensureSessionRow)
// runs after this one, its `IF(title_generated = 0, ...)` guard already
// knows to leave this title alone.
func (s *MySQLStore) SaveGeneratedTitle(ctx context.Context, userID, sessionID, title string) error {
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+sessionsTable+` (user_id, id, title, summary, recent, created_at, updated_at, title_generated, study_summary, quiz)
		VALUES (?, ?, ?, '', '[]', UNIX_TIMESTAMP(), UNIX_TIMESTAMP(), 1, '', '')
		ON DUPLICATE KEY UPDATE
			title = VALUES(title),
			title_generated = 1,
			updated_at = VALUES(updated_at)
	`, userID, sessionID, truncateTitle(title)); err != nil {
		return fmt.Errorf("store: save generated title: %w", err)
	}
	return nil
}

// EndSession is a plain UPDATE, not an upsert: by the time a learner can
// confirm "end this conversation", the session row already exists (it
// carries at least one saved turn — see SaveTurn), so unlike
// SaveGeneratedTitle there's no race with a row-creating write to guard
// against. study_summary/quiz themselves are untouched here — they're still
// whatever they were (normally empty) until CompleteStudySummary/
// CompleteStudyQuiz fill them in — only study_summary_status and
// quiz_status flip to JobStatusPending, immediately, so freezing the room
// never waits on either background job's LLM call. Both jobs are enqueued
// right after this by httpserver.sessionEndHandler, in parallel — see
// asyncjob.KindStudySummary/asyncjob.KindStudyQuiz.
func (s *MySQLStore) EndSession(ctx context.Context, userID, sessionID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET ended = 1, study_summary_status = ?, quiz_status = ?, updated_at = UNIX_TIMESTAMP()
		WHERE user_id = ? AND id = ?
	`, JobStatusPending, JobStatusPending, userID, sessionID); err != nil {
		return fmt.Errorf("store: end session: %w", err)
	}
	return nil
}

// SessionEnded is a single-column read of the same `ended` flag EndSession
// sets — see the Store interface doc for why callers on a hot path (the WS
// read loop) want this instead of SessionDetail's full transcript fetch. A
// missing/foreign-user row reads the same as a fresh, un-ended session
// (false, nil), matching Load's zero-value-not-error contract.
func (s *MySQLStore) SessionEnded(ctx context.Context, userID, sessionID string) (bool, error) {
	var ended int
	err := s.ro.QueryRowContext(ctx, `
		SELECT ended FROM `+sessionsTable+` WHERE user_id = ? AND id = ?
	`, userID, sessionID).Scan(&ended)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: session ended: %w", err)
	}
	return ended != 0, nil
}

// ListSessions returns userID's normal chat rooms — instant/"오늘의 한 문장"
// rooms (see MarkInstant) are deliberately excluded so they never clutter
// the main room list; ListInstantSessions is their own separate list.
func (s *MySQLStore) ListSessions(ctx context.Context, userID string) ([]SessionMeta, error) {
	return s.listSessions(ctx, userID, false)
}

// ListInstantSessions returns userID's instant/"오늘의 한 문장" rooms (see
// MarkInstant) — the mirror image of ListSessions' exclusion, for that
// feature's own dedicated list page. Most recently active first, same as
// ListSessions, since instant rooms are meant to be reviewed and pruned
// (deleted, to keep a bad exchange out of future study material) rather than
// browsed chronologically.
func (s *MySQLStore) ListInstantSessions(ctx context.Context, userID string) ([]SessionMeta, error) {
	return s.listSessions(ctx, userID, true)
}

func (s *MySQLStore) listSessions(ctx context.Context, userID string, instant bool) ([]SessionMeta, error) {
	instantFlag := 0
	if instant {
		instantFlag = 1
	}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, title, created_at, updated_at, ended, study_summary, study_summary_status, quiz_status, quiz_completed FROM `+sessionsTable+`
		WHERE user_id = ? AND instant = ? ORDER BY updated_at DESC
	`, userID, instantFlag)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()

	out := []SessionMeta{}
	for rows.Next() {
		var m SessionMeta
		var ended, quizCompleted int
		var studySummaryJSON string
		if err := rows.Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt, &ended, &studySummaryJSON, &m.StudySummaryStatus, &m.QuizStatus, &quizCompleted); err != nil {
			return nil, fmt.Errorf("store: list sessions: %w", err)
		}
		m.Ended = ended != 0
		m.StudySummary = decodeStudySummary(studySummaryJSON)
		m.QuizCompleted = quizCompleted != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListSessionsWithStudySummary returns every one of userID's ended sessions
// that actually folded a study summary into the learner profile (see
// UpdateLearnerProfile) — i.e. ended AND study_summary is non-empty — in the
// order they were originally ended (updated_at ASC; EndSession is the only
// write that ever touches updated_at on an already-ended row, so this is a
// reliable end-order even though it's not a dedicated "ended_at" column).
// Used by transport.runProfileRegenerate to rebuild the profile from scratch
// after a session that had contributed to it gets deleted — replaying every
// remaining contributor in the same order it was originally folded in.
// Includes both normal and instant/"오늘의 한 문장" rooms: both fold into the
// same profile via the exact same EndSession -> study-summary-job path (see
// MarkInstant's doc comment — instant mode changes nothing about the
// pipeline itself, only how the room is displayed), so both must be replayed.
func (s *MySQLStore) ListSessionsWithStudySummary(ctx context.Context, userID string) ([]SessionMeta, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, title, created_at, updated_at, study_summary FROM `+sessionsTable+`
		WHERE user_id = ? AND ended = 1 AND study_summary <> '' ORDER BY updated_at ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions with study summary: %w", err)
	}
	defer rows.Close()

	out := []SessionMeta{}
	for rows.Next() {
		var m SessionMeta
		var studySummaryJSON string
		if err := rows.Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt, &studySummaryJSON); err != nil {
			return nil, fmt.Errorf("store: list sessions with study summary: %w", err)
		}
		m.Ended = true
		m.StudySummary = decodeStudySummary(studySummaryJSON)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkInstant flags a room as an instant/"오늘의 한 문장" conversation — called
// once, right after the server mints a brand-new session's ID (see
// httpserver.sessionMarkInstantHandler), so ListSessions excludes it and
// ListInstantSessions picks it up from then on. An upsert, not a plain
// UPDATE, for the same reason SaveGeneratedTitle is one: this races
// SaveTurn's own row-creating upsert (ensureSessionRow) for the same brand-
// new session, with no ordering guarantee between the two beyond "both
// eventually run" — whichever lands first creates the row, the other just
// updates the one column it owns, so the outcome is correct either way.
func (s *MySQLStore) MarkInstant(ctx context.Context, userID, sessionID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+sessionsTable+` (user_id, id, title, summary, recent, created_at, updated_at, study_summary, quiz, instant)
		VALUES (?, ?, '', '', '[]', UNIX_TIMESTAMP(), UNIX_TIMESTAMP(), '', '', 1)
		ON DUPLICATE KEY UPDATE instant = 1
	`, userID, sessionID); err != nil {
		return fmt.Errorf("store: mark instant: %w", err)
	}
	return nil
}

// SessionDetail loads the session's metadata and its full transcript. The
// two are independent reads against s.ro (the metadata row isn't needed to
// look up turns, only to confirm the session exists), so they run
// concurrently rather than paying two sequential round trips to what may be
// a network-hop-away replica.
func (s *MySQLStore) SessionDetail(ctx context.Context, userID, sessionID string) (SessionMeta, []Turn, error) {
	meta, turns, _, err := s.sessionDetail(ctx, userID, sessionID, func() ([]Turn, bool, error) {
		turns, err := s.sessionTurns(ctx, userID, sessionID)
		return turns, false, err
	})
	return meta, turns, err
}

// SessionDetailPage loads the session's metadata and one page of its
// transcript — see the Store interface doc. Same concurrency reasoning as
// SessionDetail: the metadata read and the page read are independent.
func (s *MySQLStore) SessionDetailPage(ctx context.Context, userID, sessionID string, beforeTurn, limit int) (SessionMeta, []Turn, bool, error) {
	return s.sessionDetail(ctx, userID, sessionID, func() ([]Turn, bool, error) {
		return s.sessionTurnsPage(ctx, userID, sessionID, beforeTurn, limit)
	})
}

// sessionDetail is the shared body of SessionDetail/SessionDetailPage: it
// reads the session's metadata row concurrently with fetchTurns (whichever
// transcript slice that caller wants — all of it, or one page), then applies
// the error handling both need, including mapping a missing metadata row to
// ErrNotFound. The turns half is the only part that differs between the two,
// so it's the only part passed in.
func (s *MySQLStore) sessionDetail(ctx context.Context, userID, sessionID string, fetchTurns func() ([]Turn, bool, error)) (SessionMeta, []Turn, bool, error) {
	meta := SessionMeta{ID: sessionID}
	var metaErr, turnsErr error
	var turns []Turn
	var hasMore bool
	var ended, quizCompleted, instant int
	var studySummaryJSON, quizJSON string

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		metaErr = s.ro.QueryRowContext(ctx, `
			SELECT title, created_at, updated_at, ended, study_summary, study_summary_status, quiz, quiz_status, quiz_completed, instant FROM `+sessionsTable+` WHERE user_id = ? AND id = ?
		`, userID, sessionID).Scan(&meta.Title, &meta.CreatedAt, &meta.UpdatedAt, &ended, &studySummaryJSON, &meta.StudySummaryStatus, &quizJSON, &meta.QuizStatus, &quizCompleted, &instant)
	}()
	go func() {
		defer wg.Done()
		turns, hasMore, turnsErr = fetchTurns()
	}()
	wg.Wait()

	if errors.Is(metaErr, sql.ErrNoRows) {
		return SessionMeta{}, nil, false, ErrNotFound
	}
	if metaErr != nil {
		return SessionMeta{}, nil, false, fmt.Errorf("store: session detail: %w", metaErr)
	}
	if turnsErr != nil {
		return SessionMeta{}, nil, false, fmt.Errorf("store: session detail: %w", turnsErr)
	}
	meta.Ended = ended != 0
	meta.StudySummary = decodeStudySummary(studySummaryJSON)
	meta.Quiz = decodeQuiz(quizJSON)
	meta.QuizCompleted = quizCompleted != 0
	meta.Instant = instant != 0
	return meta, turns, hasMore, nil
}

// DeleteSession removes a session and its transcript in one transaction, so
// a crash or error partway through never leaves an orphaned buddy_turns row
// pointing at a session that no longer exists in buddy_sessions.
func (s *MySQLStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	return s.withTx(ctx, "delete session", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM `+turnsTable+` WHERE user_id = ? AND session_id = ?
		`, userID, sessionID); err != nil {
			return fmt.Errorf("store: delete session: turns: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM `+sessionsTable+` WHERE user_id = ? AND id = ?
		`, userID, sessionID); err != nil {
			return fmt.Errorf("store: delete session: session: %w", err)
		}
		return nil
	})
}
