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
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"buddy/server/internal/llm"
	"buddy/server/internal/mysqlerr"
	"buddy/server/internal/protocol"
)

// Table names carry a buddy_ prefix because the database is shared with
// other services — unprefixed names would risk colliding with another
// service's tables.
const (
	sessionsTable = "buddy_sessions"
	turnsTable    = "buddy_turns"
	settingsTable = "buddy_user_settings"
	jobsTable     = "buddy_jobs"
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
	// Reuse the primary pool for reads unless a distinct replica is named, so
	// the common single-endpoint case doesn't open two pools to one host.
	distinctRO := cfg.ROHost != "" && cfg.ROHost != cfg.RWHost

	var rw, ro *sql.DB
	var rwErr, roErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rw, rwErr = openPool(cfg, cfg.RWHost)
	}()
	if distinctRO {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ro, roErr = openPool(cfg, cfg.ROHost)
		}()
	}
	wg.Wait()

	if rwErr != nil {
		if ro != nil {
			ro.Close()
		}
		return nil, rwErr
	}
	if roErr != nil {
		rw.Close()
		return nil, roErr
	}
	if !distinctRO {
		ro = rw
	}
	// Closes whatever pools ended up open, for the migration failure paths
	// below — ro may or may not be a distinct connection from rw at this point.
	closeAll := func() {
		rw.Close()
		if ro != rw {
			ro.Close()
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
			user_id     VARCHAR(255) NOT NULL,
			session_id  VARCHAR(64)  NOT NULL,
			turn        INT          NOT NULL,
			role        VARCHAR(16)  NOT NULL,
			text        TEXT         NOT NULL,
			refined     TINYINT(1)   NOT NULL DEFAULT 0,
			source      VARCHAR(8)   NOT NULL DEFAULT '',
			correction  TEXT         NULL,
			translation TEXT         NULL,
			meta        TEXT         NULL,
			created_at  BIGINT       NOT NULL,
			PRIMARY KEY (user_id, session_id, turn, role)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS ` + settingsTable + ` (
			user_id            VARCHAR(255) NOT NULL,
			interlocutor_style TEXT         NOT NULL,
			updated_at         BIGINT       NOT NULL,
			PRIMARY KEY (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// buddy_jobs tracks the status of durable background jobs
		// (internal/asyncjob) per (turn, kind) — separate from buddy_turns
		// because several kinds are session-scoped rather than turn-scoped
		// (title, compaction), and a single turn can have more than one kind
		// of job in flight at once (reply, correction, translation). turn=0
		// is the reserved sentinel already used for session-scoped/opening
		// work (see pipeline.StartConversation).
		`CREATE TABLE IF NOT EXISTS ` + jobsTable + ` (
			user_id    VARCHAR(255) NOT NULL,
			session_id VARCHAR(64)  NOT NULL,
			turn       INT          NOT NULL,
			kind       VARCHAR(24)  NOT NULL,
			status     VARCHAR(16)  NOT NULL DEFAULT 'pending',
			error      TEXT         NULL,
			created_at BIGINT       NOT NULL,
			updated_at BIGINT       NOT NULL,
			PRIMARY KEY (user_id, session_id, turn, kind)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, stmt := range schema {
		if _, err := rw.Exec(stmt); err != nil {
			closeAll()
			return nil, fmt.Errorf("store: schema: %w", err)
		}
	}
	// Additive: buddy_turns predates the translation feature, so existing
	// deployments need this column added on top of their already-created
	// table — the CREATE TABLE IF NOT EXISTS above only helps fresh ones.
	// See internal/mysqlerr's doc for why ER_DUP_FIELDNAME is swallowed here
	// as the "already applied" case.
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.Exec(`ALTER TABLE ` + turnsTable + ` ADD COLUMN translation TEXT NULL`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		closeAll()
		return nil, fmt.Errorf("store: schema: add translation column: %w", err)
	}
	// Additive, same reasoning as the translation column above: buddy_turns
	// predates input-source tracking.
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.Exec(`ALTER TABLE ` + turnsTable + ` ADD COLUMN source VARCHAR(8) NOT NULL DEFAULT ''`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		closeAll()
		return nil, fmt.Errorf("store: schema: add source column: %w", err)
	}
	// Additive, same reasoning as the translation column above: predates the
	// auto-title feature. Tracks whether a session's title has already been
	// LLM-generated (see SaveGeneratedTitle) so a reconnect can never
	// re-trigger and flap it.
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.Exec(`ALTER TABLE ` + sessionsTable + ` ADD COLUMN title_generated TINYINT(1) NOT NULL DEFAULT 0`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		closeAll()
		return nil, fmt.Errorf("store: schema: add title_generated column: %w", err)
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

func (s *MySQLStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error {
	// The session row is created lazily by whichever turn lands first for a
	// room — either the opening greeting (turn 0, assistant) or the
	// learner's own turn 1 — so a room the learner opened shows up in
	// ListSessions/SessionDetail (and can be deleted from there) even if
	// they never answered the greeting, instead of leaving an invisible
	// orphan sitting in buddy_turns with no row in buddy_sessions to find it
	// by. Once turn 1 lands, its title (the learner's own first message)
	// always supersedes the greeting-derived placeholder: title is only
	// overwritten while title_generated is still 0, the same guard
	// SaveGeneratedTitle relies on to pin a title for good, so a session
	// that already has a real (possibly LLM-generated) title — e.g. after a
	// "reset" that cleared buddy_turns but left buddy_sessions alone —
	// keeps it.
	if turn == 0 || (turn == 1 && role == "user") {
		if _, err := s.rw.ExecContext(ctx, `
			INSERT INTO `+sessionsTable+` (user_id, id, title, summary, recent, created_at, updated_at)
			VALUES (?, ?, ?, '', '[]', UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
			ON DUPLICATE KEY UPDATE
				title = IF(title_generated = 0, VALUES(title), title),
				updated_at = VALUES(updated_at)
		`, userID, sessionID, truncateTitle(text)); err != nil {
			return fmt.Errorf("store: ensure session: %w", err)
		}
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

// SaveCorrection writes the correction text and marks its job "done" in one
// transaction — same atomicity reasoning as CompleteAssistantTurn, so a
// poller never sees a "done" correction-job status before the correction it
// belongs to is readable. The job update is a plain UPDATE (not an upsert),
// so it's a harmless no-op when no job was ever reserved for this turn (e.g.
// a caller that predates ReserveCorrectionJob, or a test double).
func (s *MySQLStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	corrJSON, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: encode correction: %w", err)
	}
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save correction: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	if _, err := tx.ExecContext(ctx, `
		UPDATE `+turnsTable+` SET correction = ?
		WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'user'
	`, string(corrJSON), userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: save correction: text: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE `+jobsTable+` SET status = ?, updated_at = UNIX_TIMESTAMP()
		WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = 'correction'
	`, JobStatusDone, userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: save correction: job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: save correction: commit: %w", err)
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

// SaveGeneratedTitle overwrites a session's title with an LLM-generated one,
// but only the first time it's called for that session: the
// `title_generated = 0` guard in the WHERE clause makes this a no-op on
// every subsequent call (0 rows affected, no error), the same
// write-once-then-pinned semantics SaveTurn already gives the
// truncated-first-message title. See internal/transport for why that
// matters — the trigger condition alone (WS turn 1) fires once per
// *connection*, not once per session, so this DB-level guard is what
// actually prevents a reconnect from re-rolling the title.
func (s *MySQLStore) SaveGeneratedTitle(ctx context.Context, userID, sessionID, title string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET title = ?, title_generated = 1
		WHERE user_id = ? AND id = ? AND title_generated = 0
	`, truncateTitle(title), userID, sessionID); err != nil {
		return fmt.Errorf("store: save generated title: %w", err)
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

// SessionDetail loads the session's metadata and its turns (or transcript
// page — see the Store interface doc). The two are independent reads
// against s.ro (the metadata row isn't needed to look up turns, only to
// confirm the session exists), so they run concurrently rather than paying
// two sequential round trips to what may be a network-hop-away replica.
func (s *MySQLStore) SessionDetail(ctx context.Context, userID, sessionID string, beforeTurn, limit int) (SessionMeta, []Turn, bool, error) {
	meta := SessionMeta{ID: sessionID}
	var metaErr, turnsErr error
	var turns []Turn
	var hasMore bool

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		metaErr = s.ro.QueryRowContext(ctx, `
			SELECT title, created_at, updated_at FROM `+sessionsTable+` WHERE user_id = ? AND id = ?
		`, userID, sessionID).Scan(&meta.Title, &meta.CreatedAt, &meta.UpdatedAt)
	}()
	go func() {
		defer wg.Done()
		turns, hasMore, turnsErr = s.sessionTurns(ctx, userID, sessionID, beforeTurn, limit)
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
	return meta, turns, hasMore, nil
}

// sessionTurns loads (userID, sessionID)'s turns, ordered so that role =
// 'assistant' sorts after 'user' within a turn (false < true). Two LEFT
// JOINs against buddy_jobs — one for kind='reply' (assistant turns), one for
// kind='correction' (user turns) — surface each turn's job status, so the
// frontend can tell "still generating"/"still checking"
// (ReplyStatus/CorrectionStatus == JobStatusPending, no result yet) apart
// from "no job was ever tracked for this turn" (older turns saved before
// that job kind's tracking existed) — both look identical in buddy_turns
// alone.
//
// limit <= 0 loads every turn (beforeTurn ignored) — same query this always
// ran before pagination existed. limit > 0 first resolves the page's lower
// turn-number bound with a cheap DISTINCT-turn probe (fetching one extra row
// to detect whether older turns remain beyond the page, without a second
// round trip), then reuses the same join for the page's actual rows.
func (s *MySQLStore) sessionTurns(ctx context.Context, userID, sessionID string, beforeTurn, limit int) ([]Turn, bool, error) {
	minTurn := 0
	hasMore := false
	if limit <= 0 {
		beforeTurn = 0 // unbounded fetch: ignore any cursor, same as before pagination existed
	} else {
		boundRows, err := s.ro.QueryContext(ctx, `
			SELECT DISTINCT turn FROM `+turnsTable+`
			WHERE user_id = ? AND session_id = ? AND (? <= 0 OR turn < ?)
			ORDER BY turn DESC LIMIT ?
		`, userID, sessionID, beforeTurn, beforeTurn, limit+1)
		if err != nil {
			return nil, false, fmt.Errorf("store: session detail: page bounds: %w", err)
		}
		var turnNums []int
		for boundRows.Next() {
			var t int
			if err := boundRows.Scan(&t); err != nil {
				boundRows.Close()
				return nil, false, fmt.Errorf("store: session detail: page bounds: %w", err)
			}
			turnNums = append(turnNums, t)
		}
		boundRows.Close()
		if err := boundRows.Err(); err != nil {
			return nil, false, fmt.Errorf("store: session detail: page bounds: %w", err)
		}
		if len(turnNums) == 0 {
			return []Turn{}, false, nil
		}
		if len(turnNums) > limit {
			hasMore = true
			turnNums = turnNums[:limit]
		}
		minTurn = turnNums[len(turnNums)-1] // DESC order, so the last kept entry is the smallest
	}

	rows, err := s.ro.QueryContext(ctx, `
		SELECT t.turn, t.role, t.text, t.refined, t.source, t.correction, t.translation, t.meta, t.created_at, rj.status, cj.status
		FROM `+turnsTable+` t
		LEFT JOIN `+jobsTable+` rj
			ON rj.user_id = t.user_id AND rj.session_id = t.session_id AND rj.turn = t.turn
			AND rj.kind = 'reply' AND t.role = 'assistant'
		LEFT JOIN `+jobsTable+` cj
			ON cj.user_id = t.user_id AND cj.session_id = t.session_id AND cj.turn = t.turn
			AND cj.kind = 'correction' AND t.role = 'user'
		WHERE t.user_id = ? AND t.session_id = ? AND t.turn >= ? AND (? <= 0 OR t.turn < ?)
		ORDER BY t.turn ASC, t.role = 'assistant' ASC
	`, userID, sessionID, minTurn, beforeTurn, beforeTurn)
	if err != nil {
		return nil, false, fmt.Errorf("store: session detail: %w", err)
	}
	defer rows.Close()

	turns := []Turn{}
	for rows.Next() {
		var t Turn
		var refined int
		var correctionJSON, translation, metaJSON, replyStatus, correctionStatus sql.NullString
		if err := rows.Scan(&t.Turn, &t.Role, &t.Text, &refined, &t.Source, &correctionJSON, &translation, &metaJSON, &t.CreatedAt, &replyStatus, &correctionStatus); err != nil {
			return nil, false, fmt.Errorf("store: session detail: %w", err)
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
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: session detail: %w", err)
	}
	return turns, hasMore, nil
}

// ReserveAssistantTurn writes a placeholder assistant-turn row plus a
// pending reply-job row, in one transaction, before the reply job is even
// enqueued. Both inserts are no-ops on a second call for the same
// (userID, sessionID, turn) — `ON DUPLICATE KEY UPDATE user_id = user_id`
// touches nothing — so a race between two callers (e.g. a reconnect racing
// the original connection's inline claim) can never clobber an
// already-completed row's text or job status.
func (s *MySQLStore) ReserveAssistantTurn(ctx context.Context, userID, sessionID string, turn int) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: reserve assistant turn: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+turnsTable+` (user_id, session_id, turn, role, text, refined, source, created_at)
		VALUES (?, ?, ?, 'assistant', '', 0, '', UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE user_id = user_id
	`, userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: reserve assistant turn: placeholder: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO `+jobsTable+` (user_id, session_id, turn, kind, status, created_at, updated_at)
		VALUES (?, ?, ?, 'reply', ?, UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE user_id = user_id
	`, userID, sessionID, turn, JobStatusPending); err != nil {
		return fmt.Errorf("store: reserve assistant turn: job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: reserve assistant turn: commit: %w", err)
	}
	return nil
}

// CompleteAssistantTurn writes the finished reply text and marks its job
// done, atomically — so a poller (or another replica reloading Profile
// after this job ran elsewhere) never observes a "done" status with the
// old empty placeholder text, or vice versa.
func (s *MySQLStore) CompleteAssistantTurn(ctx context.Context, userID, sessionID string, turn int, text string) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: complete assistant turn: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	// Turn 0 is the opening greeting (see pipeline.StartConversation): its
	// text is only known now, at completion, not at ReserveAssistantTurn
	// time — so the session-row-creation side effect SaveTurn's turn==0
	// branch normally provides (a greeting-only room already visible to
	// ListSessions/SessionDetail) happens here instead.
	if turn == 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO `+sessionsTable+` (user_id, id, title, summary, recent, created_at, updated_at)
			VALUES (?, ?, ?, '', '[]', UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
			ON DUPLICATE KEY UPDATE
				title = IF(title_generated = 0, VALUES(title), title),
				updated_at = VALUES(updated_at)
		`, userID, sessionID, truncateTitle(text)); err != nil {
			return fmt.Errorf("store: complete assistant turn: ensure session: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE `+turnsTable+` SET text = ? WHERE user_id = ? AND session_id = ? AND turn = ? AND role = 'assistant'
	`, text, userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: complete assistant turn: text: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE `+jobsTable+` SET status = ?, updated_at = UNIX_TIMESTAMP()
		WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = 'reply'
	`, JobStatusDone, userID, sessionID, turn); err != nil {
		return fmt.Errorf("store: complete assistant turn: job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: complete assistant turn: commit: %w", err)
	}
	return nil
}

func (s *MySQLStore) FailJob(ctx context.Context, userID, sessionID string, turn int, kind, errMsg string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+jobsTable+` SET status = ?, error = ?, updated_at = UNIX_TIMESTAMP()
		WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = ?
	`, JobStatusFailed, errMsg, userID, sessionID, turn, kind); err != nil {
		return fmt.Errorf("store: fail job: %w", err)
	}
	return nil
}

// JobStatus reads from s.rw (the primary), not s.ro: callers use this to
// decide whether to redo expensive work (see pipeline.ReplyJobHandler's
// idempotency guard), so a stale "not done yet" read from a lagging replica
// would cause a duplicate LLM call — the same reasoning as LastTurn.
func (s *MySQLStore) JobStatus(ctx context.Context, userID, sessionID string, turn int, kind string) (string, error) {
	var status string
	err := s.rw.QueryRowContext(ctx, `
		SELECT status FROM `+jobsTable+` WHERE user_id = ? AND session_id = ? AND turn = ? AND kind = ?
	`, userID, sessionID, turn, kind).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: job status: %w", err)
	}
	return status, nil
}

// DeleteSession removes a session and its transcript in one transaction, so
// a crash or error partway through never leaves an orphaned buddy_turns row
// pointing at a session that no longer exists in buddy_sessions.
func (s *MySQLStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete session: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete session: commit: %w", err)
	}
	return nil
}

func (s *MySQLStore) GetInterlocutorStyle(ctx context.Context, userID string) (string, error) {
	var style string
	err := s.ro.QueryRowContext(ctx,
		`SELECT interlocutor_style FROM `+settingsTable+` WHERE user_id = ?`, userID,
	).Scan(&style)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get interlocutor style: %w", err)
	}
	return style, nil
}

func (s *MySQLStore) SaveInterlocutorStyle(ctx context.Context, userID, style string) error {
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+settingsTable+` (user_id, interlocutor_style, updated_at)
		VALUES (?, ?, UNIX_TIMESTAMP())
		ON DUPLICATE KEY UPDATE interlocutor_style = VALUES(interlocutor_style), updated_at = VALUES(updated_at)
	`, userID, style); err != nil {
		return fmt.Errorf("store: save interlocutor style: %w", err)
	}
	return nil
}

// DB exposes the underlying read-write and read-only pools so other stores
// that persist to the same MySQL instance (e.g. internal/recording) can share
// these connections instead of opening a second pool to the same host.
func (s *MySQLStore) DB() (rw, ro *sql.DB) { return s.rw, s.ro }

func (s *MySQLStore) Close() error {
	err := s.rw.Close()
	if s.ro != s.rw {
		if e := s.ro.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
