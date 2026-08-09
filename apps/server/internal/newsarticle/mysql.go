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

// addColumn runs an idempotent `ALTER TABLE ... ADD COLUMN` migration,
// swallowing mysqlerr.DupFieldName the same way every such migration in
// this package needs to (see mysqlerr's doc for why ADD COLUMN, unlike DROP
// COLUMN, has no native idempotent "already applied" story).
func addColumn(ctx context.Context, rw *sql.DB, ddl, label string) error {
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.ExecContext(ctx, ddl)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		return fmt.Errorf("newsarticle: schema: add %s column: %w", label, err)
	}
	return nil
}

// NewMySQL ensures buddy_articles/buddy_article_instances exist and returns
// a Store backed by them.
func NewMySQL(ctx context.Context, rw, ro *sql.DB) (*MySQLStore, error) {
	// UNIQUE KEY on url is the cache key ReserveArticle relies on: one row
	// per distinct story no matter how many learners draw it.
	const articlesSchema = `CREATE TABLE IF NOT EXISTS ` + articlesTable + ` (
		id                 VARCHAR(64)   NOT NULL,
		source             VARCHAR(64)   NOT NULL,
		title              VARCHAR(512)  NOT NULL,
		url                VARCHAR(1024) NOT NULL,
		summary            TEXT          NOT NULL,
		sub_questions_json TEXT          NOT NULL,
		status             VARCHAR(16)   NOT NULL DEFAULT '` + StatusDone + `',
		created_at         BIGINT        NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY idx_url (url(255))
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, articlesSchema); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: articles: %w", err)
	}
	// sub_questions_json replaced the original choices_json/correct_index/
	// explanation columns (a whole-paragraph 4-choice quiz shape, replaced
	// by several independent 2-choice sub-questions — see SubQuestion's doc)
	// — a clean break, not a migration: Article rows are a regenerable LLM-
	// output cache (see the package doc), so scanArticle treats a
	// pre-existing row's backfilled NULL the same as any other article
	// whose study content needs (re)generating, same as StatusPending. NULL
	// with no DEFAULT, not '' — MySQL rejects a literal DEFAULT on a
	// TEXT/BLOB column outright (see the description column below for the
	// same constraint), and NULL needs no default to begin with. The old
	// columns are left in place on an already-existing table rather than
	// dropped — harmless, unused dead weight, and DROP COLUMN has no
	// idempotent "already applied" story to swallow the way ADD COLUMN does
	// via mysqlerr.ApplyAdditive.
	if err := addColumn(ctx, rw, `ALTER TABLE `+articlesTable+` ADD COLUMN sub_questions_json TEXT NULL AFTER summary`, "sub_questions_json"); err != nil {
		return nil, err
	}
	// Predates asyncjob.KindArticleStudy, back when SaveArticle only ever
	// inserted an already-fully-generated row (the LLM call ran synchronously
	// in httpserver.articleDrawHandler's request path) — every pre-existing
	// row is therefore already StatusDone, hence the DEFAULT above backfilling
	// them automatically. See mysqlerr's doc for why this ADD COLUMN needs to
	// swallow "already applied" rather than use IF NOT EXISTS.
	if err := addColumn(ctx, rw, `ALTER TABLE `+articlesTable+` ADD COLUMN status VARCHAR(16) NOT NULL DEFAULT '`+StatusDone+`'`, "status"); err != nil {
		return nil, err
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
	if err := addColumn(ctx, rw, `ALTER TABLE `+articlesTable+` ADD COLUMN description VARCHAR(2048) NOT NULL DEFAULT ''`, "description"); err != nil {
		return nil, err
	}
	if err := addColumn(ctx, rw, `ALTER TABLE `+articlesTable+` ADD COLUMN claimed_at BIGINT NOT NULL DEFAULT 0`, "claimed_at"); err != nil {
		return nil, err
	}
	// published_at backs Article.PublishedAt — the source feed's own <pubDate>
	// (see newsfeed.Candidate.PublishedAt), separate from created_at (when
	// this row was reserved). DEFAULT 0 leaves every pre-existing row with a
	// zero PublishedAt, same "unknown, render nothing" fallback a fresh row
	// gets if its feed item had no parseable pubDate.
	if err := addColumn(ctx, rw, `ALTER TABLE `+articlesTable+` ADD COLUMN published_at BIGINT NOT NULL DEFAULT 0`, "published_at"); err != nil {
		return nil, err
	}

	// selected_options_json defaults to NULL — not yet answered, distinct
	// from an empty '[]' (which would mean "answered with 0 sub-questions",
	// impossible per articleQuizMinSubQuestions) — see scanInstance.
	// idx_user_created covers List's per-user, most-recent-first query;
	// idx_article supports a future cross-instance lookup by article
	// (nothing uses it yet, but it's the natural join key so it's indexed
	// up front rather than added later under load).
	const instancesSchema = `CREATE TABLE IF NOT EXISTS ` + instancesTable + ` (
		id                    VARCHAR(64)  NOT NULL,
		user_id               VARCHAR(255) NOT NULL,
		article_id            VARCHAR(64)  NOT NULL,
		answered              TINYINT(1)   NOT NULL DEFAULT 0,
		selected_options_json TEXT         NULL,
		correct               TINYINT(1)   NOT NULL DEFAULT 0,
		created_at            BIGINT       NOT NULL,
		PRIMARY KEY (id),
		KEY idx_user_created (user_id, created_at),
		KEY idx_article (article_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, instancesSchema); err != nil {
		return nil, fmt.Errorf("newsarticle: schema: instances: %w", err)
	}
	// selected_options_json replaced selected_index (a single 0-based pick,
	// meaningless once a "choice" became several independent sub-question
	// picks) — same clean-break, NULL-with-no-default reasoning as
	// sub_questions_json above. A pre-existing row's backfilled NULL scans
	// as SelectedOptions == nil, same as any other never-answered Instance.
	if err := addColumn(ctx, rw, `ALTER TABLE `+instancesTable+` ADD COLUMN selected_options_json TEXT NULL AFTER answered`, "selected_options_json"); err != nil {
		return nil, err
	}
	return &MySQLStore{rw: rw, ro: ro}, nil
}

// scanner lets scan* helpers read from either *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

const articleColumns = `id, source, title, url, summary, sub_questions_json, description, status, created_at, published_at`

// decodeSubQuestions decodes sub_questions_json — nil for SQL NULL or an
// empty string (a pre-existing row predating this column, or one whose
// study content genuinely hasn't been generated yet), otherwise the parsed
// array. Shared by scanArticle and scanInstance (which reads the same
// column through its joined Article).
func decodeSubQuestions(ns sql.NullString) ([]SubQuestion, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	var qs []SubQuestion
	if err := json.Unmarshal([]byte(ns.String), &qs); err != nil {
		return nil, fmt.Errorf("decode sub_questions_json: %w", err)
	}
	return qs, nil
}

func scanArticle(row scanner) (Article, error) {
	var a Article
	var subQuestionsJSON sql.NullString
	var createdAt, publishedAt int64
	if err := row.Scan(&a.ID, &a.Source, &a.Title, &a.URL, &a.Summary, &subQuestionsJSON, &a.Description, &a.Status, &createdAt, &publishedAt); err != nil {
		return Article{}, err
	}
	subQuestions, err := decodeSubQuestions(subQuestionsJSON)
	if err != nil {
		return Article{}, err
	}
	a.SubQuestions = subQuestions
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
		INSERT IGNORE INTO `+articlesTable+` (id, source, title, url, summary, sub_questions_json, description, status, created_at, claimed_at, published_at)
		VALUES (?, ?, ?, ?, '', '[]', ?, ?, ?, ?, ?)
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

// StalePending implements Store.StalePending — see its doc comment. Failed
// rows are included because a failed generation is retryable and must not
// remain stuck when Redis is unavailable.
func (s *MySQLStore) StalePending(ctx context.Context, olderThan time.Duration) ([]Article, error) {
	cutoff := time.Now().Add(-olderThan).UnixNano()
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+articleColumns+` FROM `+articlesTable+`
		WHERE status IN (?, ?) AND claimed_at < ?
	`, StatusPending, StatusFailed, cutoff)
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
		UPDATE `+articlesTable+` SET claimed_at = ?, status = ?
		WHERE id = ? AND status IN (?, ?)
	`, time.Now().UnixNano(), StatusPending, id, StatusPending, StatusFailed)
	if err != nil {
		return false, fmt.Errorf("newsarticle: claim article: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("newsarticle: claim article: rows affected: %w", err)
	}
	return n == 1, nil
}

// ReopenIncompleteArticle implements Store.ReopenIncompleteArticle — see its
// doc comment. The sub_questions_json condition covers every shape
// decodeSubQuestions treats as "no data": a real SQL NULL (the nullable
// ApplyAdditive column on a row that predates it entirely), the
// pre-migration ReserveArticle default of '[]' surviving unchanged into a
// StatusDone row from before CompleteArticle ever wrote real content into
// it, an empty string, and the literal text "null" (what json.Marshal
// encodes a nil []SubQuestion as, if CompleteArticle is ever called with
// one).
func (s *MySQLStore) ReopenIncompleteArticle(ctx context.Context, id string) (bool, error) {
	res, err := s.rw.ExecContext(ctx, `
		UPDATE `+articlesTable+` SET status = ?, claimed_at = ?
		WHERE id = ? AND status = ? AND (sub_questions_json IS NULL OR sub_questions_json IN ('', '[]', 'null'))
	`, StatusPending, time.Now().UnixNano(), id, StatusDone)
	if err != nil {
		return false, fmt.Errorf("newsarticle: reopen incomplete article: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("newsarticle: reopen incomplete article: rows affected: %w", err)
	}
	return n == 1, nil
}

func (s *MySQLStore) CompleteArticle(ctx context.Context, id, summary string, subQuestions []SubQuestion) (Article, error) {
	subQuestionsJSON, err := json.Marshal(subQuestions)
	if err != nil {
		return Article{}, fmt.Errorf("newsarticle: encode sub questions: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+articlesTable+` SET summary = ?, sub_questions_json = ?, status = ?
		WHERE id = ? AND status = ?
	`, summary, string(subQuestionsJSON), StatusDone, id, StatusPending); err != nil {
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
		INSERT INTO `+instancesTable+` (id, user_id, article_id, answered, selected_options_json, correct, created_at)
		VALUES (?, ?, ?, 0, NULL, 0, ?)
	`, id, userID, articleID, time.Now().Unix()); err != nil {
		return Instance{}, fmt.Errorf("newsarticle: create instance: %w", err)
	}
	return s.Get(ctx, userID, id)
}

const instanceColumns = `i.id, i.answered, i.selected_options_json, i.correct, i.created_at, ` +
	`a.id, a.source, a.title, a.url, a.summary, a.sub_questions_json, a.description, a.status, a.created_at, a.published_at`

// decodeSelectedOptions decodes selected_options_json the same "NULL/empty
// means nil, not an error" way decodeSubQuestions treats
// sub_questions_json — NULL before Answer, always populated after.
func decodeSelectedOptions(ns sql.NullString) ([]int, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	var opts []int
	if err := json.Unmarshal([]byte(ns.String), &opts); err != nil {
		return nil, fmt.Errorf("decode selected_options_json: %w", err)
	}
	return opts, nil
}

// scanInstance reads one instanceColumns row (Instance columns followed by
// its joined Article columns, in that order) — answered/correct come off
// the wire as ints, not bools, same convention as internal/store's
// turn.refined (see mysql_turns.go), which this driver config doesn't
// natively scan TINYINT(1) into *bool for.
func scanInstance(row scanner, userID string) (Instance, error) {
	var inst Instance
	var answered, correct int
	var instCreatedAt int64
	var selectedOptionsJSON, subQuestionsJSON sql.NullString
	var articleCreatedAt, articlePublishedAt int64
	if err := row.Scan(
		&inst.ID, &answered, &selectedOptionsJSON, &correct, &instCreatedAt,
		&inst.Article.ID, &inst.Article.Source, &inst.Article.Title, &inst.Article.URL, &inst.Article.Summary,
		&subQuestionsJSON, &inst.Article.Description, &inst.Article.Status, &articleCreatedAt, &articlePublishedAt,
	); err != nil {
		return Instance{}, err
	}
	selectedOptions, err := decodeSelectedOptions(selectedOptionsJSON)
	if err != nil {
		return Instance{}, err
	}
	subQuestions, err := decodeSubQuestions(subQuestionsJSON)
	if err != nil {
		return Instance{}, err
	}
	inst.SelectedOptions = selectedOptions
	inst.Article.SubQuestions = subQuestions
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
// allCorrect reports whether every selected option matches its
// sub-question's CorrectOptionIndex, index-wise — false (not a panic) on a
// length mismatch, which shouldn't happen from the real client (see
// httpserver.articleAnswerHandler's validation) but must never be trusted
// blindly against a server-side index anyway.
func allCorrect(selected []int, subQuestions []SubQuestion) bool {
	if len(selected) != len(subQuestions) {
		return false
	}
	for i, q := range subQuestions {
		if selected[i] != q.CorrectOptionIndex {
			return false
		}
	}
	return true
}

func (s *MySQLStore) Answer(ctx context.Context, userID, id string, selectedOptions []int) (Instance, error) {
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
	correct := allCorrect(selectedOptions, inst.Article.SubQuestions)
	selectedOptionsJSON, err := json.Marshal(selectedOptions)
	if err != nil {
		return Instance{}, fmt.Errorf("newsarticle: answer: encode selected options: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `
		UPDATE `+instancesTable+` SET answered = 1, selected_options_json = ?, correct = ? WHERE id = ? AND user_id = ?
	`, string(selectedOptionsJSON), correct, id, userID); err != nil {
		return Instance{}, fmt.Errorf("newsarticle: answer: update: %w", err)
	}
	inst.Answered = true
	inst.SelectedOptions = selectedOptions
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
