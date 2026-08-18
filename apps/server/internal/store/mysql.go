package store

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"buddy/server/internal/migration"
	"buddy/server/internal/mysqlerr"
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
	// addColumn runs one additive ALTER TABLE ... ADD COLUMN, swallowing
	// ER_DUP_FIELDNAME as the "already applied on this deployment" case (see
	// internal/mysqlerr's doc) — shared by every additive migration below,
	// each of which predates the feature/table it's adding a column for; the
	// CREATE TABLE IF NOT EXISTS block above only covers fresh deployments.
	addColumn := func(table, ddl, label string) error {
		if err := mysqlerr.ApplyAdditive(func() error {
			_, err := rw.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + ddl)
			return err
		}, mysqlerr.DupFieldName); err != nil {
			closeAll()
			return fmt.Errorf("store: schema: add %s column: %w", label, err)
		}
		return nil
	}
	steps := []migration.Step{
		{1, "turns.translation", func(context.Context, *sql.DB) error {
			return addColumn(turnsTable, "translation TEXT NULL", "translation")
		}},
		{2, "turns.source", func(context.Context, *sql.DB) error {
			return addColumn(turnsTable, "source VARCHAR(8) NOT NULL DEFAULT ''", "source")
		}},
		// Predates the auto-title feature. Tracks whether a session's title has
		// already been LLM-generated (see SaveGeneratedTitle) so a reconnect can
		// never re-trigger and flap it.
		{3, "sessions.title_generated", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "title_generated TINYINT(1) NOT NULL DEFAULT 0", "title_generated")
		}},
		// Predates the permanent end-conversation feature. ended+study_summary
		// together let a learner's confirmed "end this conversation" wrap-up
		// survive a reload instead of being regenerated (or lost) — see
		// EndSession.
		{4, "sessions.ended", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "ended TINYINT(1) NOT NULL DEFAULT 0", "ended")
		}},
		// No DEFAULT clause: MySQL rejects a literal default on a TEXT column
		// (error 1101) — same reason summary/recent/interlocutor_style above
		// never carry one either. ADD COLUMN still backfills existing rows with
		// '' on its own; every INSERT that can create a new row from here on
		// just has to list this column explicitly (see ensureSessionRow /
		// SaveGeneratedTitle below).
		{5, "sessions.study_summary", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "study_summary TEXT NOT NULL", "study_summary")
		}},
		// Predates the cross-session learner-profile feature. Unlike
		// interlocutor_style (a learner-set preference), this is LLM-maintained —
		// see Pipeline.UpdateLearnerProfile — and layered into BuildSystemPrompt
		// alongside it so a brand-new conversation still carries forward what
		// earlier, unrelated conversations revealed about this learner. Same
		// no-DEFAULT reasoning as study_summary above.
		{6, "settings.learner_profile", func(context.Context, *sql.DB) error {
			return addColumn(settingsTable, "learner_profile TEXT NOT NULL", "learner_profile")
		}},
		// Predates asyncjob.KindWordAutoAdd: lets httpserver.wordAutoAddHandler
		// return immediately after marking this row JobStatusPending, before
		// pipeline.Pipeline.SuggestNewWords' LLM call even starts, with these two
		// columns tracking that background job's progress for a reopened word-
		// review page to poll — see store.MySQLStore.StartWordAutoAdd/
		// CompleteWordAutoAdd/FailWordAutoAdd. VARCHAR (not TEXT) so it can carry
		// a DEFAULT, same reasoning as quiz_status above.
		{7, "settings.word_auto_add_status", func(context.Context, *sql.DB) error {
			return addColumn(settingsTable, "word_auto_add_status VARCHAR(16) NOT NULL DEFAULT ''", "word_auto_add_status")
		}},
		{8, "settings.word_auto_add_count", func(context.Context, *sql.DB) error {
			return addColumn(settingsTable, "word_auto_add_count INT NOT NULL DEFAULT 0", "word_auto_add_count")
		}},
		// Predates asyncjob.KindStudySummary: lets EndSession freeze a room and
		// return immediately, before the wrap-up LLM call even starts, with this
		// column tracking that background job's progress (JobStatusPending/Done/
		// Failed) for a reopened room or the room list to poll — see
		// CompleteStudySummary/FailStudySummary. A VARCHAR, unlike study_summary
		// above, so it can carry a DEFAULT: existing ended rows (frozen back when
		// the wrap-up was generated synchronously, before this column existed)
		// backfill to '' automatically, which SessionMeta.StudySummaryStatus's
		// doc comment treats the same as JobStatusDone.
		{9, "sessions.study_summary_status", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "study_summary_status VARCHAR(16) NOT NULL DEFAULT ''", "study_summary_status")
		}},
		// Predates asyncjob.KindStudyQuiz: pre-generates the practice quiz
		// alongside the wrap-up, right when EndSession freezes the room, instead
		// of on demand when the learner opens it — so tapping "퀴즈 풀기" shows an
		// already-finished quiz instantly. Same no-DEFAULT reasoning as
		// study_summary above.
		{10, "sessions.quiz", func(context.Context, *sql.DB) error { return addColumn(sessionsTable, "quiz TEXT NOT NULL", "quiz") }},
		// Tracks asyncjob.KindStudyQuiz's own progress independently of
		// study_summary_status — the two jobs run in parallel from the same
		// EndSession call, not one after the other, so they need separate status
		// columns. Same DEFAULT reasoning as study_summary_status above.
		{11, "sessions.quiz_status", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "quiz_status VARCHAR(16) NOT NULL DEFAULT ''", "quiz_status")
		}},
		// A one-way "studied this" checkmark for the room list — see
		// SessionMeta.QuizCompleted's doc comment.
		{12, "sessions.quiz_completed", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "quiz_completed TINYINT(1) NOT NULL DEFAULT 0", "quiz_completed")
		}},
		// Marks a room opened from "오늘의 한 문장"/instant mode (see MarkInstant) —
		// set right after the server mints the session ID, independently of
		// SaveTurn's own row-creating upsert (ensureSessionRow), since the two
		// writes race the same way SaveGeneratedTitle already does against it.
		// ListSessions excludes these (WHERE instant = 0) so they never clutter
		// the main room list; ListInstantSessions is the one place that reads
		// them back, for their own dedicated list page.
		{13, "sessions.instant", func(context.Context, *sql.DB) error {
			return addColumn(sessionsTable, "instant TINYINT(1) NOT NULL DEFAULT 0", "instant")
		}},
		// One-time reset for rows written before GenerateStudySummary switched to
		// the bilingual (English + native-translation, sentence-by-sentence) JSON
		// shape decodeStudySummary now expects: a pre-existing "done" summary is
		// plain native-language prose, not JSON, so leaving it in place would
		// make it silently vanish (decodeStudySummary treats anything that
		// doesn't parse as nil) with no way for the learner to tell a wrap-up
		// once existed. Reset back to JobStatusPending — not "" — so it reads as
		// "regenerating", not "done, nothing to show": httpserver.sessionDetailHandler's
		// needsStudySummaryBackfill re-enqueues asyncjob.KindStudySummary for
		// exactly this state (Ended, StudySummaryStatus == JobStatusPending, no
		// StudySummary yet) the next time the learner opens the session, and
		// regenerates it from each turn's still-intact store.Turn.Correction —
		// the underlying issues were never touched by this migration, only the
		// old free-text wrap-up column was. Guarded by the LEFT(...) <> '[' check
		// so it only ever touches legacy plain-text rows: a summary already in
		// the new JSON-array shape (from a session ended after this migration
		// first ran) always starts with '[' and is left untouched. The migration
		// marker makes this compatibility rewrite run only once.
		{14, "reset_legacy_study_summaries", func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET study_summary = '', study_summary_status = ?
		WHERE study_summary_status = 'done' AND study_summary <> '' AND LEFT(study_summary, 1) <> '['
	`, JobStatusPending)
			return err
		}},
	}
	if err := migration.Apply(context.Background(), rw, "store", steps); err != nil {
		closeAll()
		return nil, fmt.Errorf("store: schema migrations: %w", err)
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

// withTx runs fn inside a transaction on the primary, rolling back if fn (or
// Commit) fails — the shared begin/rollback/commit scaffolding behind
// SaveCorrection, ReserveAssistantTurn, CompleteAssistantTurn, and
// DeleteSession. label appears in the wrapped begin/commit error, matching
// each call site's own "store: <op>: ..." error-wrapping convention; fn's own
// per-statement errors keep their own more specific labels.
func (s *MySQLStore) withTx(ctx context.Context, label string, fn func(*sql.Tx) error) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %s: begin: %w", label, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: %s: commit: %w", label, err)
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
