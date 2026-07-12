package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

// Table names carry a buddy_ prefix because the database is shared with
// other services — unprefixed names would risk colliding with another
// service's tables.
const (
	sessionsTable = "buddy_sessions"
	turnsTable    = "buddy_turns"
)

// MySQLConfig describes how to reach MySQL. RWHost is the primary: all writes
// and the schema bootstrap go there. ROHost is an optional read replica that
// Load reads from to take load off the primary; leave it empty (or equal to
// RWHost) to serve reads from the primary too — the strongly-consistent
// default. When ROHost names a real replica, reads may observe slightly
// stale data during replication lag, which is acceptable here: a session is
// re-saved as the conversation continues, so a stale read self-heals within
// a turn or two.
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
// configured) and ensures buddy_sessions/buddy_turns exist on the primary.
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

	// (user_id, id)/(user_id, session_id, turn, role) composite primary keys
	// — not a bare id/session_id — so every row is structurally scoped to
	// its owner: even a guessed or leaked session ID can only ever resolve
	// to rows under the requesting user's own user_id, never someone else's.
	// VARCHAR (not TEXT) for keys: MySQL can't index TEXT without a prefix
	// length. utf8mb4 throughout so learner text and native-language
	// feedback (CJK, emoji) round-trip losslessly.
	schema := []string{
		`CREATE TABLE IF NOT EXISTS ` + sessionsTable + ` (
			user_id    VARCHAR(255) NOT NULL,
			id         VARCHAR(64)  NOT NULL,
			title      VARCHAR(255) NOT NULL DEFAULT '',
			summary    TEXT         NOT NULL,
			recent     TEXT         NOT NULL,
			created_at BIGINT       NOT NULL,
			updated_at BIGINT       NOT NULL,
			PRIMARY KEY (user_id, id),
			INDEX idx_user_updated (user_id, updated_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS ` + turnsTable + ` (
			user_id    VARCHAR(255) NOT NULL,
			session_id VARCHAR(64)  NOT NULL,
			turn       INT          NOT NULL,
			role       VARCHAR(16)  NOT NULL,
			text       TEXT         NOT NULL,
			refined    TINYINT(1)   NOT NULL DEFAULT 0,
			correction TEXT         NULL,
			meta       TEXT         NULL,
			created_at BIGINT       NOT NULL,
			PRIMARY KEY (user_id, session_id, turn, role)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, stmt := range schema {
		if _, err := rw.Exec(stmt); err != nil {
			rw.Close()
			if ro != rw {
				ro.Close()
			}
			return nil, fmt.Errorf("store: schema: %w", err)
		}
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

// HostPort returns the "host:port" to dial for host, which may be a bare
// hostname (fallbackPort is appended) or may already include its own
// ":port" — some secret stores inject the endpoint pre-joined. Unconditionally
// appending fallbackPort via net.JoinHostPort in that case would double up
// the port and, worse, wrap the whole "host:port" string in brackets as a
// literal DNS name (net.JoinHostPort brackets any host containing ":"),
// producing lookups like "mysql-prod.internal:3306: no such host" instead of
// resolving the intended hostname. Shared by every caller that dials a host
// from config (MySQL here, Redis in cmd/server) since both are exposed to the
// same class of secret-store incident.
func HostPort(host string, fallbackPort int) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(fallbackPort))
}

// openPool opens and verifies a connection pool to one MySQL host.
func openPool(cfg MySQLConfig, host string) (*sql.DB, error) {
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = HostPort(host, cfg.Port)
	c.User = cfg.User
	c.Passwd = cfg.Password
	c.DBName = cfg.Database
	c.Collation = "utf8mb4_unicode_ci"
	// Interpolate params client-side so Load/Save's simple queries skip the
	// server-side prepare round trip that database/sql otherwise does on
	// every call — this runs on every WS save tick, not just at startup.
	c.InterpolateParams = true

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

func (s *MySQLStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool) error {
	// The session's row (and its title, derived from the opening message) is
	// created lazily by whichever turn arrives first — always turn 1 from
	// the user, since a session only starts once someone speaks or types.
	// ON DUPLICATE KEY UPDATE only touches updated_at, so a session that
	// already exists (e.g. after a "reset" that cleared buddy_turns but left
	// buddy_sessions alone) keeps its original title.
	if turn == 1 && role == "user" {
		if _, err := s.rw.ExecContext(ctx, `
			INSERT INTO `+sessionsTable+` (user_id, id, title, summary, recent, created_at, updated_at)
			VALUES (?, ?, ?, '', '[]', UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
			ON DUPLICATE KEY UPDATE updated_at = VALUES(updated_at)
		`, userID, sessionID, truncateTitle(text)); err != nil {
			return fmt.Errorf("store: ensure session: %w", err)
		}
	}
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+turnsTable+` (user_id, session_id, turn, role, text, refined, created_at)
		VALUES (?, ?, ?, ?, ?, ?, UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE text = VALUES(text), refined = VALUES(refined)
	`, userID, sessionID, turn, role, text, refined); err != nil {
		return fmt.Errorf("store: save turn: %w", err)
	}
	return nil
}

func (s *MySQLStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	corrJSON, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: encode correction: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+turnsTable+` SET correction = ?
		WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'user'
	`, string(corrJSON), userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: save correction: %w", err)
	}
	return nil
}

func (s *MySQLStore) DeleteTurns(ctx context.Context, userID, sessionID string) error {
	if _, err := s.rw.ExecContext(ctx, `
		DELETE FROM `+turnsTable+` WHERE user_id = ? AND session_id = ?
	`, userID, sessionID); err != nil {
		return fmt.Errorf("store: delete turns: %w", err)
	}
	return nil
}

func (s *MySQLStore) ListSessions(ctx context.Context, userID string) ([]SessionMeta, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, title, created_at, updated_at FROM `+sessionsTable+`
		WHERE user_id = ? ORDER BY updated_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()

	out := []SessionMeta{}
	for rows.Next() {
		var m SessionMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list sessions: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *MySQLStore) SessionDetail(ctx context.Context, userID, sessionID string) (SessionMeta, []Turn, error) {
	meta := SessionMeta{ID: sessionID}
	err := s.ro.QueryRowContext(ctx, `
		SELECT title, created_at, updated_at FROM `+sessionsTable+` WHERE user_id = ? AND id = ?
	`, userID, sessionID).Scan(&meta.Title, &meta.CreatedAt, &meta.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionMeta{}, nil, ErrNotFound
	}
	if err != nil {
		return SessionMeta{}, nil, fmt.Errorf("store: session detail: %w", err)
	}

	// role = 'assistant' sorts after 'user' within a turn (false < true).
	rows, err := s.ro.QueryContext(ctx, `
		SELECT turn, role, text, refined, correction, meta FROM `+turnsTable+`
		WHERE user_id = ? AND session_id = ? ORDER BY turn ASC, role = 'assistant' ASC
	`, userID, sessionID)
	if err != nil {
		return SessionMeta{}, nil, fmt.Errorf("store: session detail: %w", err)
	}
	defer rows.Close()

	turns := []Turn{}
	for rows.Next() {
		var t Turn
		var refined int
		var correctionJSON, metaJSON sql.NullString
		if err := rows.Scan(&t.Turn, &t.Role, &t.Text, &refined, &correctionJSON, &metaJSON); err != nil {
			return SessionMeta{}, nil, fmt.Errorf("store: session detail: %w", err)
		}
		t.Refined = refined != 0
		if correctionJSON.Valid {
			var c protocol.Correction
			if err := json.Unmarshal([]byte(correctionJSON.String), &c); err == nil {
				t.Correction = &c
			}
		}
		if metaJSON.Valid {
			t.Meta = json.RawMessage(metaJSON.String)
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("store: session detail: %w", err)
	}
	return meta, turns, nil
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
