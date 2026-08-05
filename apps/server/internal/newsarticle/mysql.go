package newsarticle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"buddy/server/internal/mysqlerr"
)

// Tables carry a buddy_ prefix for the same reason as internal/store's and
// internal/wordreview's: the database is shared with other services.
const (
	articlesTable  = "buddy_articles"
	instancesTable = "buddy_article_instances"
)

// MySQLStore is the default Store. rw/ro are shared with internal/store's
// MySQLStore (see its DB() accessor), same reasoning as
// wordreview.MySQLStore: this package has no storage needs beyond plain
// rows.
type MySQLStore struct {
	rw, ro *sql.DB
}

// NewMySQL ensures buddy_articles/buddy_article_instances exist and returns
// a Store backed by them.
func NewMySQL(ctx context.Context, rw, ro *sql.DB) (*MySQLStore, error) {
	// UNIQUE KEY on url is the cache key ReserveArticle relies on: one row
	// per distinct story no matter how many learners draw it.
	const articlesSchema = `CREATE TABLE IF NOT EXISTS ` + articlesTable + ` (
		id            VARCHAR(64)   NOT NULL,
		source        VARCHAR(64)   NOT NULL,
		title         VARCHAR(512)  NOT NULL,
		url           VARCHAR(1024) NOT NULL,
		summary       TEXT          NOT NULL,
		choices_json  TEXT          NOT NULL,
		correct_index INT           NOT NULL,
		explanation   TEXT          NOT NULL,
		status        VARCHAR(16)   NOT NULL DEFAULT '` + StatusDone + `',
		created_at    BIGINT        NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY idx_url (url(255))
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, articlesSchema); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: articles: %w", err)
	}
	// Predates asyncjob.KindArticleStudy, back when SaveArticle only ever
	// inserted an already-fully-generated row (the LLM call ran synchronously
	// in httpserver.articleDrawHandler's request path) — every pre-existing
	// row is therefore already StatusDone, hence the DEFAULT above backfilling
	// them automatically. See mysqlerr's doc for why this ADD COLUMN needs to
	// swallow "already applied" rather than use IF NOT EXISTS.
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.ExecContext(ctx, `ALTER TABLE `+articlesTable+` ADD COLUMN status VARCHAR(16) NOT NULL DEFAULT '`+StatusDone+`'`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: add status column: %w", err)
	}
	// description backs Article.Description — see its doc comment for why
	// it's persisted rather than only passed transiently through the draw
	// request. claimed_at backs StalePending/ClaimArticle's DB-only orphan
	// sweep; defaulting both new columns to '' / created_at-equivalent 0
	// leaves every pre-existing row (all already StatusDone, per the status
	// column above) permanently ineligible for the sweep's `status =
	// StatusPending` filter regardless of claimed_at's backfilled value.
	// VARCHAR, not TEXT: MySQL rejects a literal DEFAULT on BLOB/TEXT/JSON
	// columns outright (only expression defaults are allowed there), and a
	// feed snippet (see newsfeed.Candidate.Description's doc comment) is
	// short by construction anyway, same reasoning as title's VARCHAR(512).
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.ExecContext(ctx, `ALTER TABLE `+articlesTable+` ADD COLUMN description VARCHAR(2048) NOT NULL DEFAULT ''`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: add description column: %w", err)
	}
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.ExecContext(ctx, `ALTER TABLE `+articlesTable+` ADD COLUMN claimed_at BIGINT NOT NULL DEFAULT 0`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: add claimed_at column: %w", err)
	}
	// published_at backs Article.PublishedAt — the source feed's own <pubDate>
	// (see newsfeed.Candidate.PublishedAt), separate from created_at (when
	// this row was reserved). DEFAULT 0 leaves every pre-existing row with a
	// zero PublishedAt, same "unknown, render nothing" fallback a fresh row
	// gets if its feed item had no parseable pubDate.
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.ExecContext(ctx, `ALTER TABLE `+articlesTable+` ADD COLUMN published_at BIGINT NOT NULL DEFAULT 0`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: add published_at column: %w", err)
	}

	// selected_index defaults to -1 (not yet answered) rather than 0, which
	// would be indistinguishable from an actual "chose choice 0" answer.
	// idx_user_created covers List's per-user, most-recent-first query;
	// idx_article supports a future cross-instance lookup by article
	// (nothing uses it yet, but it's the natural join key so it's indexed
	// up front rather than added later under load).
	const instancesSchema = `CREATE TABLE IF NOT EXISTS ` + instancesTable + ` (
		id             VARCHAR(64)  NOT NULL,
		user_id        VARCHAR(255) NOT NULL,
		article_id     VARCHAR(64)  NOT NULL,
		answered       TINYINT(1)   NOT NULL DEFAULT 0,
		selected_index INT          NOT NULL DEFAULT -1,
		correct        TINYINT(1)   NOT NULL DEFAULT 0,
		created_at     BIGINT       NOT NULL,
		PRIMARY KEY (id),
		KEY idx_user_created (user_id, created_at),
		KEY idx_article (article_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, instancesSchema); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: instances: %w", err)
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

// scanner lets scan* helpers read from either *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

const articleColumns = `id, source, title, url, summary, choices_json, correct_index, explanation, description, status, created_at, published_at`

func scanArticle(row scanner) (Article, error) {
	var a Article
	var choicesJSON string
	var createdAt, publishedAt int64
	if err := row.Scan(&a.ID, &a.Source, &a.Title, &a.URL, &a.Summary, &choicesJSON, &a.CorrectIndex, &a.Explanation, &a.Description, &a.Status, &createdAt, &publishedAt); err != nil {
		return Article{}, err
	}
	if err := json.Unmarshal([]byte(choicesJSON), &a.Choices); err != nil {
		return Article{}, fmt.Errorf("decode choices: %w", err)
	}
	a.CreatedAt = time.Unix(createdAt, 0)
	if publishedAt > 0 {
		a.PublishedAt = time.Unix(publishedAt, 0)
	}
	return a, nil
}

func (s *MySQLStore) ReserveArticle(ctx context.Context, source, title, url, description string, publishedAt time.Time) (Article, error) {
	// INSERT IGNORE: the UNIQUE KEY on url makes this a no-op if two
	// concurrent draws (by different learners, or a retry) reserved the same
	// story at the same time — the loser's row is discarded in favor of
	// whichever write landed first, and both callers end up reading the same
	// row back, same shape as wordreview.MySQLStore.Save. claimed_at starts
	// at the same instant as created_at (in nanoseconds — see ClaimArticle's
	// doc comment for why seconds aren't precise enough): the caller
	// (httpserver.articleDrawHandler) always dispatches generation
	// immediately after reserving, in the same request, so "just reserved"
	// and "just claimed" really are the same moment for a fresh row.
	now := time.Now().Unix()
	// publishedAt.Unix() on a zero time.Time is a large negative number, not
	// 0 — normalize so an unknown pubDate (see newsfeed.Candidate.
	// PublishedAt's doc comment) round-trips as "no date" through scanArticle
	// the same way a pre-existing row's DEFAULT 0 does.
	var publishedAtUnix int64
	if !publishedAt.IsZero() {
		publishedAtUnix = publishedAt.Unix()
	}
	_, err := s.rw.ExecContext(ctx, `
		INSERT IGNORE INTO `+articlesTable+` (id, source, title, url, summary, choices_json, correct_index, explanation, description, status, created_at, claimed_at, published_at)
		VALUES (?, ?, ?, ?, '', '[]', 0, '', ?, ?, ?, ?, ?)
	`, uuid.New().String(), source, title, url, description, StatusPending, now, time.Now().UnixNano(), publishedAtUnix)
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: reserve article: insert: %w", err)
	}
	saved, err := scanArticle(s.rw.QueryRowContext(ctx, `SELECT `+articleColumns+` FROM `+articlesTable+` WHERE url = ?`, url))
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: reserve article: lookup: %w", err)
	}
	return saved, nil
}

// StalePending implements Store.StalePending — see its doc comment.
func (s *MySQLStore) StalePending(ctx context.Context, olderThan time.Duration) ([]Article, error) {
	cutoff := time.Now().Add(-olderThan).UnixNano()
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+articleColumns+` FROM `+articlesTable+` WHERE status = ? AND claimed_at < ?
	`, StatusPending, cutoff)
	if err != nil {
		return nil, fmt.Errorf("newsarticle: stale pending: %w", err)
	}
	defer rows.Close()
	var out []Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, fmt.Errorf("newsarticle: stale pending: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimArticle implements Store.ClaimArticle — see its doc comment. Reads
// via rw (not ro) so the RowsAffected check right after a fresh
// StalePending read is never fooled by replica lag into thinking it lost a
// claim it actually won. claimed_at is nanoseconds, not seconds: this
// driver's RowsAffected reports rows actually *changed*, not just matched,
// so a claim landing within the same wall-clock second as the row's
// existing claimed_at (e.g. immediately after ReserveArticle, or two sweep
// ticks close together) would otherwise look like "0 rows affected" — i.e.
// lost the claim — even though the UPDATE's WHERE matched and ran.
func (s *MySQLStore) ClaimArticle(ctx context.Context, id string) (bool, error) {
	res, err := s.rw.ExecContext(ctx, `
		UPDATE `+articlesTable+` SET claimed_at = ? WHERE id = ? AND status = ?
	`, time.Now().UnixNano(), id, StatusPending)
	if err != nil {
		return false, fmt.Errorf("newsarticle: claim article: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("newsarticle: claim article: rows affected: %w", err)
	}
	return n == 1, nil
}

func (s *MySQLStore) CompleteArticle(ctx context.Context, id, summary string, choices []string, correctIndex int, explanation string) (Article, error) {
	choicesJSON, err := json.Marshal(choices)
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: encode choices: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+articlesTable+` SET summary = ?, choices_json = ?, correct_index = ?, explanation = ?, status = ?
		WHERE id = ? AND status = ?
	`, summary, string(choicesJSON), correctIndex, explanation, StatusDone, id, StatusPending); err != nil {
		return Article{}, fmt.Errorf("newsarticle: complete article: update: %w", err)
	}
	saved, err := scanArticle(s.rw.QueryRowContext(ctx, `SELECT `+articleColumns+` FROM `+articlesTable+` WHERE id = ?`, id))
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: complete article: lookup: %w", err)
	}
	return saved, nil
}

func (s *MySQLStore) FailArticle(ctx context.Context, id string) error {
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+articlesTable+` SET status = ? WHERE id = ? AND status = ?
	`, StatusFailed, id, StatusPending); err != nil {
		return fmt.Errorf("newsarticle: fail article: %w", err)
	}
	return nil
}

func (s *MySQLStore) GetArticle(ctx context.Context, id string) (Article, bool, error) {
	a, err := scanArticle(s.ro.QueryRowContext(ctx, `SELECT `+articleColumns+` FROM `+articlesTable+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Article{}, false, nil
	}
	if err != nil {
		return Article{}, false, fmt.Errorf("newsarticle: get article: %w", err)
	}
	return a, true, nil
}

func (s *MySQLStore) UsedURLs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT a.url FROM `+instancesTable+` i JOIN `+articlesTable+` a ON a.id = i.article_id
		WHERE i.user_id = ?
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("newsarticle: used urls: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			return nil, fmt.Errorf("newsarticle: used urls: scan: %w", err)
		}
		out[url] = true
	}
	return out, rows.Err()
}

func (s *MySQLStore) CreateInstance(ctx context.Context, userID, articleID string) (Instance, error) {
	id := uuid.New().String()
	if _, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+instancesTable+` (id, user_id, article_id, answered, selected_index, correct, created_at)
		VALUES (?, ?, ?, 0, -1, 0, ?)
	`, id, userID, articleID, time.Now().Unix()); err != nil {
		return Instance{}, fmt.Errorf("newsarticle: create instance: %w", err)
	}
	return s.Get(ctx, userID, id)
}

const instanceColumns = `i.id, i.answered, i.selected_index, i.correct, i.created_at, ` +
	`a.id, a.source, a.title, a.url, a.summary, a.choices_json, a.correct_index, a.explanation, a.description, a.status, a.created_at, a.published_at`

// scanInstance reads one instanceColumns row (Instance columns followed by
// its joined Article columns, in that order) — answered/correct come off
// the wire as ints, not bools, same convention as internal/store's
// turn.refined (see mysql_turns.go), which this driver config doesn't
// natively scan TINYINT(1) into *bool for.
func scanInstance(row scanner, userID string) (Instance, error) {
	var inst Instance
	var answered, correct int
	var instCreatedAt int64
	var choicesJSON string
	var articleCreatedAt, articlePublishedAt int64
	if err := row.Scan(
		&inst.ID, &answered, &inst.SelectedIndex, &correct, &instCreatedAt,
		&inst.Article.ID, &inst.Article.Source, &inst.Article.Title, &inst.Article.URL, &inst.Article.Summary,
		&choicesJSON, &inst.Article.CorrectIndex, &inst.Article.Explanation, &inst.Article.Description, &inst.Article.Status, &articleCreatedAt, &articlePublishedAt,
	); err != nil {
		return Instance{}, err
	}
	if err := json.Unmarshal([]byte(choicesJSON), &inst.Article.Choices); err != nil {
		return Instance{}, fmt.Errorf("decode choices: %w", err)
	}
	inst.UserID = userID
	inst.Answered = answered != 0
	inst.Correct = correct != 0
	inst.CreatedAt = time.Unix(instCreatedAt, 0)
	inst.Article.CreatedAt = time.Unix(articleCreatedAt, 0)
	if articlePublishedAt > 0 {
		inst.Article.PublishedAt = time.Unix(articlePublishedAt, 0)
	}
	return inst, nil
}

func (s *MySQLStore) List(ctx context.Context, userID string) ([]Instance, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+instanceColumns+` FROM `+instancesTable+` i JOIN `+articlesTable+` a ON a.id = i.article_id
		WHERE i.user_id = ? ORDER BY i.created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("newsarticle: list: %w", err)
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		inst, err := scanInstance(rows, userID)
		if err != nil {
			return nil, fmt.Errorf("newsarticle: list: scan: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

func (s *MySQLStore) Get(ctx context.Context, userID, id string) (Instance, error) {
	inst, err := scanInstance(s.ro.QueryRowContext(ctx, `
		SELECT `+instanceColumns+` FROM `+instancesTable+` i JOIN `+articlesTable+` a ON a.id = i.article_id
		WHERE i.id = ? AND i.user_id = ?
	`, id, userID), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Instance{}, nil
	}
	if err != nil {
		return Instance{}, fmt.Errorf("newsarticle: get: %w", err)
	}
	return inst, nil
}

// Answer reads/writes via rw (not ro) so an instance created moments ago is
// never missed because of replica lag, same reasoning as
// wordreview.MySQLStore.Review.
func (s *MySQLStore) Answer(ctx context.Context, userID, id string, selectedIndex int) (Instance, error) {
	inst, err := scanInstance(s.rw.QueryRowContext(ctx, `
		SELECT `+instanceColumns+` FROM `+instancesTable+` i JOIN `+articlesTable+` a ON a.id = i.article_id
		WHERE i.id = ? AND i.user_id = ?
	`, id, userID), userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Instance{}, nil
	}
	if err != nil {
		return Instance{}, fmt.Errorf("newsarticle: answer: lookup: %w", err)
	}
	if inst.Answered {
		return inst, nil
	}
	correct := selectedIndex == inst.Article.CorrectIndex
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+instancesTable+` SET answered = 1, selected_index = ?, correct = ? WHERE id = ? AND user_id = ?
	`, selectedIndex, correct, id, userID); err != nil {
		return Instance{}, fmt.Errorf("newsarticle: answer: update: %w", err)
	}
	inst.Answered = true
	inst.SelectedIndex = selectedIndex
	inst.Correct = correct
	return inst, nil
}

func (s *MySQLStore) Delete(ctx context.Context, userID, id string) error {
	if _, err := s.rw.ExecContext(ctx, `DELETE FROM `+instancesTable+` WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return fmt.Errorf("newsarticle: delete: %w", err)
	}
	return nil
}

// Close is a no-op: the rw/ro pools are owned by internal/store's
// MySQLStore, which closes them.
func (s *MySQLStore) Close() error { return nil }
