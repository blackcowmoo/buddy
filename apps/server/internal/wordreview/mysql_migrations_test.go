package wordreview

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"buddy/server/internal/mysqlerr"
	"buddy/server/internal/testdocker"

	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
)

func TestLegacyColumnMigrationsPreserveWords(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()
	// This predates the additive migrations. A partially applied meaning
	// migration also leaves its first column behind without a ledger entry.
	_, err := db.ExecContext(ctx, `CREATE TABLE buddy_word_reviews (
		id VARCHAR(64) PRIMARY KEY, user_id VARCHAR(255) NOT NULL,
		word VARCHAR(255) NOT NULL, meaning TEXT NOT NULL, example TEXT NOT NULL,
		stage INT NOT NULL, review_count INT NOT NULL, correct_streak INT NOT NULL,
		next_review_at BIGINT NOT NULL, last_reviewed_at BIGINT NOT NULL,
		status VARCHAR(16) NOT NULL, verify_reason TEXT NOT NULL, created_at BIGINT NOT NULL,
		meaning_version INT NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO buddy_word_reviews
		(id, user_id, word, meaning, example, stage, review_count, correct_streak,
		 next_review_at, last_reviewed_at, status, verify_reason, created_at)
		VALUES ('legacy', 'learner', 'bank', '은행', 'At the bank.', 2, 5, 1,
		 1700000200, 1700000100, 'verified', '', 1700000000)`)
	if err != nil {
		t.Fatal(err)
	}
	want := Word{
		ID: "legacy", UserID: "learner", Word: "bank", Meaning: "은행", Example: "At the bank.",
		Stage: 2, ReviewCount: 5, CorrectStreak: 1, Status: StatusVerified,
		NextReviewAt: time.Unix(1700000200, 0), LastReviewedAt: time.Unix(1700000100, 0), CreatedAt: time.Unix(1700000000, 0),
	}
	for _, phase := range []string{"upgrade", "restart", "replay existing columns"} {
		if phase == "replay existing columns" {
			if _, err := db.ExecContext(ctx, `DELETE FROM buddy_schema_migrations WHERE component = 'wordreview'`); err != nil {
				t.Fatal(err)
			}
		}
		st, err := NewMySQL(ctx, db, db)
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		got, err := st.Get(ctx, want.UserID, want.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: word = %+v, error = %v; want %+v", phase, got, err, want)
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT version, name FROM buddy_schema_migrations WHERE component = 'wordreview' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		if version != len(names)+1 {
			t.Fatalf("migration version = %d, want %d", version, len(names)+1)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"word_reviews.original_word", "word_reviews.research_status", "word_reviews.research_results",
		"word_reviews.review_question_version", "word_reviews.review_prompt", "word_reviews.review_answer",
		"word_reviews.review_answers", "word_reviews.dictionary_meaning", "word_reviews.meaning_revision",
		"word_reviews.meaning_target_version",
		"word_reviews.research_revision",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("migration names = %v, want %v", names, wantNames)
	}
}

func TestAddWordColumnsStopsOnRealFailure(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE buddy_word_reviews (id INT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	err := addWordColumns("already_added INT", "invalid_column UNKNOWN_TYPE", "not_reached INT")(ctx, db)
	if err == nil || mysqlerr.Is(err, mysqlerr.DupFieldName) {
		t.Fatalf("invalid column error = %v, want a non-duplicate SQL error", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'buddy_word_reviews' AND column_name = 'not_reached'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("migration continued after a failed column")
	}
	if err := addWordColumns("already_added INT", "recovered INT")(ctx, db); err != nil {
		t.Fatalf("resume after repairing the migration: %v", err)
	}
}

func TestMeaningVersionMigrationPreservesOnlyExplicitConfirmations(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()
	st, err := NewMySQL(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	wants := make(map[string]Word)
	for _, status := range []string{MeaningPending, MeaningDone, MeaningFailed, MeaningConfirmed} {
		w, err := st.Save(ctx, "learner", status, "시설", "The facility closed.")
		if err != nil {
			t.Fatal(err)
		}
		legacyVersion := 0
		if status == MeaningDone || status == MeaningConfirmed {
			legacyVersion = 1
		}
		if _, err := db.ExecContext(ctx, `UPDATE `+table+` SET meaning_status=?, meaning_version=?,
			meaning_revision=3, stage=2, review_count=5, previous_meaning='원래 뜻' WHERE id=?`, status, legacyVersion, w.ID); err != nil {
			t.Fatal(err)
		}
		w, err = st.Get(ctx, "learner", w.ID)
		if err != nil {
			t.Fatal(err)
		}
		w.MeaningTargetVersion = legacyVersion
		if status != MeaningConfirmed {
			w.MeaningVersion = 0
		}
		wants[w.ID] = w
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN meaning_target_version`); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"upgrade", "restart", "replay after partial migration"} {
		if phase != "restart" {
			if _, err := db.ExecContext(ctx, `DELETE FROM buddy_schema_migrations WHERE component='wordreview' AND version=10`); err != nil {
				t.Fatal(err)
			}
		}
		st, err := NewMySQL(ctx, db, db)
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		for id, want := range wants {
			got, err := st.Get(ctx, "learner", id)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: migration changed confirmation or progress: %+v want=%+v err=%v", phase, got, want, err)
			}
		}
	}
}

func newMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// Schema changes need a private database; the package's shared store is
	// also used by word/review tests and must retain its schema and data.
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	container, err := tcmysql.Run(ctx, "mysql:8.0", testdocker.WithProcessSession(),
		tcmysql.WithDatabase("buddy"), tcmysql.WithUsername("buddy"), tcmysql.WithPassword("buddy"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Error(err)
		}
	})
	dsn, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}
