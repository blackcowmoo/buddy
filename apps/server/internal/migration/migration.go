// Package migration owns the application's database schema migrations.
package migration

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationsTable = "buddy_migrate_schema_migrations"

// Step and ApplyLegacy are retained only for component-specific data repairs
// that have not yet been folded into the shared SQL history. New schema
// changes must be added as numbered golang-migrate files instead.
type Step struct {
	Version int
	Name    string
	Up      func(context.Context, *sql.DB) error
}

func ApplyLegacy(ctx context.Context, db *sql.DB, component string, steps []Step) error {
	const table = "buddy_schema_migrations"
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		component VARCHAR(128) NOT NULL, version INT NOT NULL, name VARCHAR(255) NOT NULL,
		applied_at BIGINT NOT NULL, PRIMARY KEY (component, version)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		return fmt.Errorf("create legacy migration table: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open legacy migration connection: %w", err)
	}
	defer conn.Close()
	var locked int
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, "buddy:legacy-migrations:"+component).Scan(&locked); err != nil {
		return fmt.Errorf("acquire legacy migration lock: %w", err)
	}
	if locked != 1 {
		return fmt.Errorf("acquire legacy migration lock: timeout")
	}
	defer conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, "buddy:legacy-migrations:"+component) //nolint:errcheck
	for _, step := range steps {
		var exists int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE component = ? AND version = ?`, component, step.Version).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check legacy migration %d (%s): %w", step.Version, step.Name, err)
		}
		if step.Up == nil {
			return fmt.Errorf("legacy migration %d (%s) has no action", step.Version, step.Name)
		}
		if err := step.Up(ctx, db); err != nil {
			return fmt.Errorf("legacy migration %d (%s): %w", step.Version, step.Name, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO `+table+` (component, version, name, applied_at) VALUES (?, ?, ?, UNIX_TIMESTAMP())`, component, step.Version, step.Name); err != nil {
			return fmt.Errorf("record legacy migration %d (%s): %w", step.Version, step.Name, err)
		}
	}
	return nil
}

// Apply runs all pending migrations against the primary database. The MySQL
// driver takes an advisory lock and records a version plus dirty state, so a
// failed migration is visible and cannot be silently retried.
func Apply(ctx context.Context, db *sql.DB, databaseName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("migration files: %w", err)
	}
	source, err := iofs.New(entries, ".")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}
	driver, err := mysql.WithInstance(db, &mysql.Config{DatabaseName: databaseName, MigrationsTable: migrationsTable})
	if err != nil {
		return fmt.Errorf("migration database: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "mysql", driver)
	if err != nil {
		return fmt.Errorf("migration runner: %w", err)
	}
	// Do not call m.Close here. NewWithInstance borrows db, and the MySQL
	// driver's Close method closes that *sql.DB as well as its migration
	// connection. The caller still owns db and must use it for the rest of
	// startup (and for the lifetime of the application).
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		version, dirty, versionErr := m.Version()
		if versionErr == nil && dirty {
			return fmt.Errorf("migration failed at version %d (dirty): %w", version, err)
		}
		return fmt.Errorf("migration failed: %w", err)
	}
	return nil
}
