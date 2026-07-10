package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // pure-Go driver: no CGo

	"buddy/server/internal/llm"
)

// PostgresStore is the default Store. Unlike an embedded file-based database,
// Postgres handles concurrent writers natively — this is what lets the app
// run as multiple replicas in Kubernetes (a SQLite file on a PV can't:
// single-writer, and PVs are typically RWO so only one pod could mount it).
type PostgresStore struct {
	db *sql.DB
}

// NewPostgres connects to dsn (e.g. "postgres://user:pass@host:5432/buddy")
// and ensures the schema exists.
func NewPostgres(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	// Modest cap so one replica can't exhaust Postgres's max_connections in a
	// multi-replica deployment; Postgres itself handles the concurrency fine.
	db.SetMaxOpenConns(10)

	const schema = `CREATE TABLE IF NOT EXISTS profiles (
		user_id    TEXT PRIMARY KEY,
		summary    TEXT NOT NULL DEFAULT '',
		recent     TEXT NOT NULL DEFAULT '[]',
		updated_at BIGINT NOT NULL DEFAULT extract(epoch from now())::bigint
	)`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) Load(ctx context.Context, userID string) (Profile, error) {
	var summary, recentJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT summary, recent FROM profiles WHERE user_id = $1`, userID,
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

func (s *PostgresStore) Save(ctx context.Context, userID string, p Profile) error {
	recentJSON, err := json.Marshal(p.Recent)
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO profiles (user_id, summary, recent, updated_at)
		VALUES ($1, $2, $3, extract(epoch from now())::bigint)
		ON CONFLICT (user_id) DO UPDATE SET
			summary = excluded.summary,
			recent = excluded.recent,
			updated_at = excluded.updated_at
	`, userID, p.Summary, string(recentJSON))
	if err != nil {
		return fmt.Errorf("store: save: %w", err)
	}
	return nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }
