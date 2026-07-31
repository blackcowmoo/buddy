package wordreview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// table carries a buddy_ prefix for the same reason as internal/store's and
// internal/recording's tables: the database is shared with other services.
const table = "buddy_word_reviews"

// MySQLStore is the default Store. rw/ro are shared with internal/store's
// MySQLStore (see its DB() accessor) rather than a second connection pool to
// the same instance — this package has no storage needs of its own beyond
// plain rows, unlike internal/recording's S3 archive.
type MySQLStore struct {
	rw, ro *sql.DB
}

// NewMySQL ensures buddy_word_reviews exists and returns a Store backed by
// it.
func NewMySQL(ctx context.Context, rw, ro *sql.DB) (*MySQLStore, error) {
	// user_id+word+meaning is UNIQUE (not just user_id+word) so the same word
	// with a different meaning — "bank" the riverbank vs. "bank" the
	// financial one — tracks, verifies, and schedules as two independent
	// rows, while Save's INSERT IGNORE still can't create a duplicate row for
	// the exact same (word, meaning) pair. idx_user_due covers the Due/
	// DueCount query's WHERE (status, next_review_at) scoped to one user, the
	// hot path this package exists for. No "retired"/mastered column: words
	// are never removed from rotation, just reviewed less and less often
	// (see wordreview.go's stageIntervals doc for why).
	const schema = `CREATE TABLE IF NOT EXISTS ` + table + ` (
		id               VARCHAR(64)  NOT NULL,
		user_id          VARCHAR(255) NOT NULL,
		word             VARCHAR(255) NOT NULL,
		meaning          TEXT         NOT NULL,
		example          TEXT         NOT NULL,
		stage            INT          NOT NULL DEFAULT 0,
		review_count     INT          NOT NULL DEFAULT 0,
		correct_streak   INT          NOT NULL DEFAULT 0,
		next_review_at   BIGINT       NOT NULL,
		last_reviewed_at BIGINT       NOT NULL DEFAULT 0,
		status           VARCHAR(16)  NOT NULL DEFAULT 'pending',
		verify_reason    TEXT         NOT NULL,
		created_at       BIGINT       NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY idx_user_word_meaning (user_id, word(191), meaning(191)),
		KEY idx_user_due (user_id, status, next_review_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("wordreview: schema: %w", err)
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

// scanner lets scanWord read from either *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanWord(row scanner, userID string) (Word, error) {
	var w Word
	var nextReviewAt, lastReviewedAt, createdAt int64
	if err := row.Scan(
		&w.ID, &w.Word, &w.Meaning, &w.Example,
		&w.Stage, &w.ReviewCount, &w.CorrectStreak,
		&nextReviewAt, &lastReviewedAt, &w.Status, &w.VerifyReason, &createdAt,
	); err != nil {
		return Word{}, err
	}
	w.UserID = userID
	w.NextReviewAt = time.Unix(nextReviewAt, 0)
	if lastReviewedAt > 0 {
		w.LastReviewedAt = time.Unix(lastReviewedAt, 0)
	}
	w.CreatedAt = time.Unix(createdAt, 0)
	return w, nil
}

const wordColumns = `id, word, meaning, example, stage, review_count, correct_streak, next_review_at, last_reviewed_at, status, verify_reason, created_at`

func (s *MySQLStore) Save(ctx context.Context, userID, word, meaning, example string) (Word, error) {
	now := time.Now()
	// INSERT IGNORE: the UNIQUE KEY on (user_id, word, meaning) makes this a
	// no-op if the learner already chose to study this exact word+meaning —
	// re-selecting it from a later search must not reset progress already
	// made, so a duplicate silently keeps the existing row instead of
	// erroring. NextReviewAt is set here but doesn't matter until
	// MarkVerified resets it — a pending word is excluded from Due/DueCount
	// regardless (see their WHERE clauses).
	_, err := s.rw.ExecContext(ctx, `
		INSERT IGNORE INTO `+table+` (id, user_id, word, meaning, example, stage, review_count, correct_streak, next_review_at, last_reviewed_at, status, verify_reason, created_at)
		VALUES (?, ?, ?, ?, ?, 0, 0, 0, ?, 0, ?, '', ?)
	`, uuid.New().String(), userID, word, meaning, example, now.Add(intervalForStage(0)).Unix(), StatusPending, now.Unix())
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: save: insert: %w", err)
	}
	w, err := scanWord(s.rw.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE user_id = ? AND word = ? AND meaning = ?`, userID, word, meaning), userID)
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: save: lookup: %w", err)
	}
	return w, nil
}

func (s *MySQLStore) Get(ctx context.Context, userID, id string) (Word, error) {
	w, err := scanWord(s.ro.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id = ? AND user_id = ?`, id, userID), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Word{}, nil
	}
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: get: %w", err)
	}
	return w, nil
}

func (s *MySQLStore) List(ctx context.Context, userID string) ([]Word, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("wordreview: list: %w", err)
	}
	defer rows.Close()
	var out []Word
	for rows.Next() {
		w, err := scanWord(rows, userID)
		if err != nil {
			return nil, fmt.Errorf("wordreview: list: scan: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *MySQLStore) DueCount(ctx context.Context, userID string, now time.Time) (int, error) {
	var n int
	err := s.ro.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM `+table+` WHERE user_id = ? AND status = ? AND next_review_at <= ?
	`, userID, StatusVerified, now.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("wordreview: due count: %w", err)
	}
	return n, nil
}

// MarkVerified reads/writes via rw (not ro) so a word saved moments ago is
// never missed because of replica lag, same reasoning as Review below.
func (s *MySQLStore) MarkVerified(ctx context.Context, userID, id string, now time.Time) (Word, error) {
	nextReviewAt := now.Add(intervalForStage(0))
	res, err := s.rw.ExecContext(ctx, `
		UPDATE `+table+` SET status = ?, next_review_at = ? WHERE id = ? AND user_id = ?
	`, StatusVerified, nextReviewAt.Unix(), id, userID)
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: mark verified: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Word{}, nil
	}
	w, err := scanWord(s.rw.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id = ? AND user_id = ?`, id, userID), userID)
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: mark verified: lookup: %w", err)
	}
	return w, nil
}

func (s *MySQLStore) MarkRejected(ctx context.Context, userID, id string, reason string) (Word, error) {
	res, err := s.rw.ExecContext(ctx, `
		UPDATE `+table+` SET status = ?, verify_reason = ? WHERE id = ? AND user_id = ?
	`, StatusRejected, reason, id, userID)
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: mark rejected: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Word{}, nil
	}
	w, err := scanWord(s.rw.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id = ? AND user_id = ?`, id, userID), userID)
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: mark rejected: lookup: %w", err)
	}
	return w, nil
}

// Review reads via rw (not ro) so a word saved or reviewed moments ago is
// never missed because of replica lag, same reasoning as
// internal/recording's Delete.
func (s *MySQLStore) Review(ctx context.Context, userID, id string, correct bool, now time.Time) (Word, error) {
	w, err := scanWord(s.rw.QueryRowContext(ctx, `SELECT `+wordColumns+` FROM `+table+` WHERE id = ? AND user_id = ?`, id, userID), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Word{}, nil
	}
	if err != nil {
		return Word{}, fmt.Errorf("wordreview: review: lookup: %w", err)
	}

	newStage, nextReviewAt := nextSchedule(w.Stage, correct, now)
	streak := 0
	if correct {
		streak = w.CorrectStreak + 1
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+table+` SET
			stage = ?, review_count = review_count + 1, correct_streak = ?,
			next_review_at = ?, last_reviewed_at = ?
		WHERE id = ? AND user_id = ?
	`, newStage, streak, nextReviewAt.Unix(), now.Unix(), id, userID); err != nil {
		return Word{}, fmt.Errorf("wordreview: review: update: %w", err)
	}

	w.Stage = newStage
	w.ReviewCount++
	w.CorrectStreak = streak
	w.NextReviewAt = nextReviewAt
	w.LastReviewedAt = now
	return w, nil
}

func (s *MySQLStore) Delete(ctx context.Context, userID, id string) error {
	if _, err := s.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return fmt.Errorf("wordreview: delete: %w", err)
	}
	return nil
}

// Close is a no-op: the rw/ro pools are owned by internal/store's
// MySQLStore, which closes them.
func (s *MySQLStore) Close() error { return nil }
