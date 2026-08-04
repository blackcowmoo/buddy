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

const articleColumns = `id, source, title, url, summary, choices_json, correct_index, explanation, status, created_at`

func scanArticle(row scanner) (Article, error) {
	var a Article
	var choicesJSON string
	var createdAt int64
	if err := row.Scan(&a.ID, &a.Source, &a.Title, &a.URL, &a.Summary, &choicesJSON, &a.CorrectIndex, &a.Explanation, &a.Status, &createdAt); err != nil {
		return Article{}, err
	}
	if err := json.Unmarshal([]byte(choicesJSON), &a.Choices); err != nil {
		return Article{}, fmt.Errorf("decode choices: %w", err)
	}
	a.CreatedAt = time.Unix(createdAt, 0)
	return a, nil
}

func (s *MySQLStore) ReserveArticle(ctx context.Context, source, title, url string) (Article, error) {
	// INSERT IGNORE: the UNIQUE KEY on url makes this a no-op if two
	// concurrent draws (by different learners, or a retry) reserved the same
	// story at the same time — the loser's row is discarded in favor of
	// whichever write landed first, and both callers end up reading the same
	// row back, same shape as wordreview.MySQLStore.Save.
	_, err := s.rw.ExecContext(ctx, `
		INSERT IGNORE INTO `+articlesTable+` (id, source, title, url, summary, choices_json, correct_index, explanation, status, created_at)
		VALUES (?, ?, ?, ?, '', '[]', 0, '', ?, ?)
	`, uuid.New().String(), source, title, url, StatusPending, time.Now().Unix())
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: reserve article: insert: %w", err)
	}
	saved, err := scanArticle(s.rw.QueryRowContext(ctx, `SELECT `+articleColumns+` FROM `+articlesTable+` WHERE url = ?`, url))
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: reserve article: lookup: %w", err)
	}
	return saved, nil
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
	`a.id, a.source, a.title, a.url, a.summary, a.choices_json, a.correct_index, a.explanation, a.status, a.created_at`

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
	var articleCreatedAt int64
	if err := row.Scan(
		&inst.ID, &answered, &inst.SelectedIndex, &correct, &instCreatedAt,
		&inst.Article.ID, &inst.Article.Source, &inst.Article.Title, &inst.Article.URL, &inst.Article.Summary,
		&choicesJSON, &inst.Article.CorrectIndex, &inst.Article.Explanation, &inst.Article.Status, &articleCreatedAt,
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
