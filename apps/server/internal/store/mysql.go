package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"

	"buddy/server/internal/llm"
)

// table is the single table this store owns. It carries a buddy_ prefix
// because the database is shared with other services — an unprefixed
// "profiles" would risk colliding with another service's table.
const table = "buddy_profiles"

// MySQLConfig describes how to reach MySQL. RWHost is the primary: all writes
// and the schema bootstrap go there. ROHost is an optional read replica that
// Load reads from to take load off the primary; leave it empty (or equal to
// RWHost) to serve reads from the primary too — the strongly-consistent
// default. When ROHost names a real replica, Load may observe slightly stale
// data during replication lag, which is acceptable here: a Profile is re-saved
// as the session continues, so a stale read self-heals within a turn or two.
type MySQLConfig struct {
	RWHost   string
	ROHost   string
	Port     int
	User     string
	Password string
	Database string
}

// MySQLStore is the default Store. MySQL (not an embedded file database) is
// what lets the app run as multiple replicas in Kubernetes: it handles
// concurrent writers natively, whereas a single-writer file on an RWO volume
// could be mounted by only one pod.
type MySQLStore struct {
	rw *sql.DB
	ro *sql.DB // == rw when no distinct read replica is configured
}

// NewMySQL connects to the primary (and the read replica, if one is
// configured) and ensures the buddy_profiles table exists on the primary.
func NewMySQL(cfg MySQLConfig) (*MySQLStore, error) {
	rw, err := openPool(cfg, cfg.RWHost)
	if err != nil {
		return nil, err
	}

	// Reuse the primary pool for reads unless a distinct replica is named, so
	// the common single-endpoint case doesn't open two pools to one host.
	ro := rw
	if cfg.ROHost != "" && cfg.ROHost != cfg.RWHost {
		ro, err = openPool(cfg, cfg.ROHost)
		if err != nil {
			rw.Close()
			return nil, err
		}
	}

	// VARCHAR(255) (not TEXT) for the key: MySQL can't index a TEXT column
	// without a prefix length, and utf8mb4 keeps the key well under InnoDB's
	// index-length limit. utf8mb4 throughout so learner text and native-language
	// feedback (CJK, emoji) round-trip losslessly.
	const schema = `CREATE TABLE IF NOT EXISTS ` + table + ` (
		user_id    VARCHAR(255) NOT NULL,
		summary    TEXT         NOT NULL,
		recent     TEXT         NOT NULL,
		updated_at BIGINT       NOT NULL,
		PRIMARY KEY (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.Exec(schema); err != nil {
		rw.Close()
		if ro != rw {
			ro.Close()
		}
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

// openPool opens and verifies a connection pool to one MySQL host.
func openPool(cfg MySQLConfig, host string) (*sql.DB, error) {
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	c.User = cfg.User
	c.Passwd = cfg.Password
	c.DBName = cfg.Database
	c.Collation = "utf8mb4_unicode_ci"

	db, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", host, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", host, err)
	}
	// Modest cap so one replica can't exhaust MySQL's max_connections in a
	// multi-replica deployment; MySQL itself handles the concurrency fine.
	db.SetMaxOpenConns(10)
	// Recycle connections so a load-balanced RW/RO endpoint that failed over
	// doesn't leave us pinned to a now-dead backend.
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

func (s *MySQLStore) Load(ctx context.Context, userID string) (Profile, error) {
	var summary, recentJSON string
	err := s.ro.QueryRowContext(ctx,
		`SELECT summary, recent FROM `+table+` WHERE user_id = ?`, userID,
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

func (s *MySQLStore) Save(ctx context.Context, userID string, p Profile) error {
	recentJSON, err := json.Marshal(p.Recent)
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}
	// VALUES(col) in the update clause is deprecated in MySQL 8.0.20+, but
	// unlike the newer row-alias syntax it works across every MySQL/MariaDB
	// version this might run against — worth a deprecation notice for the
	// portability, since writes go to the primary regardless of its version.
	_, err = s.rw.ExecContext(ctx, `
		INSERT INTO `+table+` (user_id, summary, recent, updated_at)
		VALUES (?, ?, ?, UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE
			summary = VALUES(summary),
			recent = VALUES(recent),
			updated_at = VALUES(updated_at)
	`, userID, p.Summary, string(recentJSON))
	if err != nil {
		return fmt.Errorf("store: save: %w", err)
	}
	return nil
}

func (s *MySQLStore) Close() error {
	err := s.rw.Close()
	if s.ro != s.rw {
		if e := s.ro.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
