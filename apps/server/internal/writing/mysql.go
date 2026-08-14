package writing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const promptsTable = "buddy_writing_prompts"

type MySQLStore struct{ rw, ro *sql.DB }

func NewMySQL(ctx context.Context, rw, ro *sql.DB) (*MySQLStore, error) {
	_, err := rw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+promptsTable+` (
		id VARCHAR(64) NOT NULL, user_id VARCHAR(255) NOT NULL,
		korean TEXT NOT NULL, status VARCHAR(16) NOT NULL,
		created_at BIGINT NOT NULL, PRIMARY KEY (id),
		KEY idx_user_created (user_id, created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)
	if err != nil {
		return nil, fmt.Errorf("writing: schema: %w", err)
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

const promptColumns = `id, user_id, korean, status, created_at`

func scanPrompt(s interface{ Scan(...any) error }) (Prompt, error) {
	var p Prompt
	var created int64
	if err := s.Scan(&p.ID, &p.UserID, &p.Korean, &p.Status, &created); err != nil {
		return Prompt{}, err
	}
	p.CreatedAt = time.Unix(created, 0)
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
	if err == sql.ErrNoRows {
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

func (s *MySQLStore) Close() error { return nil }
