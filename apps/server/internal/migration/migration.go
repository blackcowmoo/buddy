// Package migration runs ordered, one-time MySQL schema/data migrations.
package migration

import (
	"context"
	"database/sql"
	"fmt"
)

const table = "buddy_schema_migrations"

type Step struct {
	Version int
	Name    string
	Up      func(context.Context, *sql.DB) error
}

// Apply applies each unapplied step in order. A MySQL advisory lock prevents
// two application replicas from running the same migration concurrently.
// The step is recorded only after Up succeeds, so an interrupted/failed step
// is retried on the next startup. DDL is deliberately not wrapped in a
// transaction: MySQL implicitly commits most ALTER TABLE statements.
func Apply(ctx context.Context, db *sql.DB, component string, steps []Step) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+table+` (
			component VARCHAR(128) NOT NULL,
			version INT NOT NULL,
			name VARCHAR(255) NOT NULL,
			applied_at BIGINT NOT NULL,
			PRIMARY KEY (component, version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
	`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer conn.Close()

	var locked int
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, "buddy:migrations:"+component).Scan(&locked); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if locked != 1 {
		return fmt.Errorf("acquire migration lock: timeout")
	}
	defer conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, "buddy:migrations:"+component) //nolint:errcheck

	for _, step := range steps {
		var exists int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE component = ? AND version = ?`, component, step.Version).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %d (%s): %w", step.Version, step.Name, err)
		}
		if step.Up == nil {
			return fmt.Errorf("migration %d (%s) has no action", step.Version, step.Name)
		}
		if err := step.Up(ctx, db); err != nil {
			return fmt.Errorf("migration %d (%s): %w", step.Version, step.Name, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO `+table+` (component, version, name, applied_at) VALUES (?, ?, ?, UNIX_TIMESTAMP())`, component, step.Version, step.Name); err != nil {
			return fmt.Errorf("record migration %d (%s): %w", step.Version, step.Name, err)
		}
	}
	return nil
}
