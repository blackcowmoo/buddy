package writing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"buddy/server/internal/migration"
	"buddy/server/internal/protocol"
	"github.com/google/uuid"
)

const promptsTable = "buddy_writing_prompts"

type MySQLStore struct{ rw, ro *sql.DB }

func NewMySQL(ctx context.Context, rw, ro *sql.DB) (*MySQLStore, error) {
	if _, err := rw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+promptsTable+` (
		id VARCHAR(64) NOT NULL, user_id VARCHAR(255) NOT NULL,
		korean TEXT NOT NULL, status VARCHAR(16) NOT NULL,
		created_at BIGINT NOT NULL, PRIMARY KEY (id),
		KEY idx_user_created (user_id, created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		return nil, fmt.Errorf("writing: schema: %w", err)
	}
	// Answer persistence arrived after the first deployments, so the columns
	// are added through guarded ALTERs; the existence check keeps this
	// idempotent regardless of whether the shared history already ran them.
	steps := []migration.Step{
		{Version: 1, Name: "answer_check_columns", Up: func(ctx context.Context, db *sql.DB) error {
			for _, column := range []struct{ name, definition string }{
				{"answer", "TEXT NULL"},
				{"check_status", "VARCHAR(16) NOT NULL DEFAULT ''"},
				{"corrected", "TEXT NULL"},
				{"issues_json", "JSON NULL"},
			} {
				if err := addColumn(ctx, db, promptsTable, column.name, column.definition); err != nil {
					return err
				}
			}
			return nil
		}},
	}
	if err := migration.ApplyLegacy(ctx, rw, "writing", steps); err != nil {
		return nil, fmt.Errorf("writing: migrate: %w", err)
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

func addColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?)`, table, column).Scan(&exists)
	if err != nil || exists {
		return err
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

// promptColumns is shared by every read so the scan order stays in one place.
const promptColumns = `id, user_id, korean, status, created_at, answer, check_status, corrected, issues_json`

func scanPrompt(s interface{ Scan(...any) error }) (Prompt, error) {
	var p Prompt
	var created int64
	var answer, corrected sql.NullString
	var issuesRaw []byte
	if err := s.Scan(&p.ID, &p.UserID, &p.Korean, &p.Status, &created, &answer, &p.CheckStatus, &corrected, &issuesRaw); err != nil {
		return Prompt{}, err
	}
	p.CreatedAt = time.Unix(created, 0)
	p.Answer = answer.String
	p.Corrected = corrected.String
	if len(issuesRaw) > 0 {
		if err := json.Unmarshal(issuesRaw, &p.Issues); err != nil {
			return Prompt{}, fmt.Errorf("writing: decode issues: %w", err)
		}
	}
	return p, nil
}

func (s *MySQLStore) Create(ctx context.Context, userID string) (Prompt, error) {
	p := Prompt{ID: uuid.NewString(), UserID: userID, Status: StatusPending, CreatedAt: time.Now()}
	if _, err := s.rw.ExecContext(ctx, `INSERT INTO `+promptsTable+` (id,user_id,korean,status,created_at) VALUES (?,?, '', ?, ?)`, p.ID, userID, p.Status, p.CreatedAt.Unix()); err != nil {
		return Prompt{}, fmt.Errorf("writing: create: %w", err)
	}
	return p, nil
}

func (s *MySQLStore) List(ctx context.Context, userID string) ([]Prompt, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+promptColumns+` FROM `+promptsTable+` WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("writing: list: %w", err)
	}
	defer rows.Close()
	var out []Prompt
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MySQLStore) Get(ctx context.Context, userID, id string) (Prompt, error) {
	p, err := scanPrompt(s.ro.QueryRowContext(ctx, `SELECT `+promptColumns+` FROM `+promptsTable+` WHERE user_id=? AND id=?`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Prompt{}, nil
	}
	if err != nil {
		return Prompt{}, fmt.Errorf("writing: get: %w", err)
	}
	return p, nil
}

func (s *MySQLStore) Delete(ctx context.Context, userID, id string) error {
	if _, err := s.rw.ExecContext(ctx, `DELETE FROM `+promptsTable+` WHERE user_id=? AND id=?`, userID, id); err != nil {
		return fmt.Errorf("writing: delete: %w", err)
	}
	return nil
}

func (s *MySQLStore) Complete(ctx context.Context, id, korean string) error {
	_, err := s.rw.ExecContext(ctx, `UPDATE `+promptsTable+` SET korean=?, status=? WHERE id=? AND status IN (?, ?)`, korean, StatusDone, id, StatusPending, StatusFailed)
	if err != nil {
		return fmt.Errorf("writing: complete: %w", err)
	}
	return nil
}

func (s *MySQLStore) Fail(ctx context.Context, id string) error {
	_, err := s.rw.ExecContext(ctx, `UPDATE `+promptsTable+` SET status=? WHERE id=? AND status=?`, StatusFailed, id, StatusPending)
	if err != nil {
		return fmt.Errorf("writing: fail: %w", err)
	}
	return nil
}

func (s *MySQLStore) StartCheck(ctx context.Context, userID, id, answer string) (bool, error) {
	res, err := s.rw.ExecContext(ctx, `UPDATE `+promptsTable+`
		SET answer=?, check_status=?, corrected=NULL, issues_json=NULL
		WHERE user_id=? AND id=?`, answer, CheckPending, userID, id)
	if err != nil {
		return false, fmt.Errorf("writing: start check: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("writing: start check: %w", err)
	}
	return n > 0, nil
}

func (s *MySQLStore) CompleteCheck(ctx context.Context, id, answer, corrected string, issues []protocol.Issue) (bool, error) {
	payload, err := json.Marshal(issues)
	if err != nil {
		return false, fmt.Errorf("writing: encode issues: %w", err)
	}
	res, err := s.rw.ExecContext(ctx, `UPDATE `+promptsTable+`
		SET check_status=?, corrected=?, issues_json=?
		WHERE id=? AND answer=? AND check_status=?`, CheckDone, corrected, string(payload), id, answer, CheckPending)
	if err != nil {
		return false, fmt.Errorf("writing: complete check: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("writing: complete check: %w", err)
	}
	return n > 0, nil
}

func (s *MySQLStore) FailCheck(ctx context.Context, id, answer string) (bool, error) {
	res, err := s.rw.ExecContext(ctx, `UPDATE `+promptsTable+`
		SET check_status=?
		WHERE id=? AND answer=? AND check_status=?`, CheckFailed, id, answer, CheckPending)
	if err != nil {
		return false, fmt.Errorf("writing: fail check: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("writing: fail check: %w", err)
	}
	return n > 0, nil
}

func (s *MySQLStore) Recheck(ctx context.Context, userID, id string) (bool, error) {
	// The answer guard means a re-check can never start without a stored
	// answer; the check_status guard means a second request while one is
	// in flight claims nothing, so exactly one dispatch owns the re-check.
	res, err := s.rw.ExecContext(ctx, `UPDATE `+promptsTable+`
		SET check_status=?, corrected=NULL, issues_json=NULL
		WHERE user_id=? AND id=? AND answer IS NOT NULL AND answer<>'' AND check_status<>?`,
		CheckPending, userID, id, CheckPending)
	if err != nil {
		return false, fmt.Errorf("writing: recheck: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("writing: recheck: %w", err)
	}
	return n > 0, nil
}

func (s *MySQLStore) Close() error { return nil }
