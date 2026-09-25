package nuance

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// Read progress from the primary: a replica lagging behind an answer would
// resurrect an old question or overwrite a newer cross-device revision.
type MySQLStore struct{ db *sql.DB }

func NewMySQL(db *sql.DB) *MySQLStore { return &MySQLStore{db: db} }

const columns = `id,user_id,status,created_at,revision,content_json,state_json`

func scan(s interface{ Scan(...any) error }) (Lesson, error) {
	var l Lesson
	var content, state []byte
	if err := s.Scan(&l.ID, &l.UserID, &l.Status, &l.CreatedAt, &l.Revision, &content, &state); err != nil {
		if err == sql.ErrNoRows {
			return l, ErrNotFound
		}
		return l, err
	}
	if len(content) > 0 {
		if err := json.Unmarshal(content, &l.Content); err != nil {
			return l, err
		}
	}
	if err := json.Unmarshal(state, &l.State); err != nil {
		return l, err
	}
	return l, nil
}
func (s *MySQLStore) Create(ctx context.Context, userID string) (Lesson, error) {
	l := Lesson{ID: uuid.NewString(), UserID: userID, Status: StatusPending, CreatedAt: time.Now().Unix(), State: State{Progress: map[string]Progress{}, Queue: []string{}}}
	state, _ := json.Marshal(l.State)
	_, err := s.db.ExecContext(ctx, `INSERT INTO buddy_nuance_lessons (id,user_id,status,created_at,state_json) VALUES (?,?,?,?,?)`, l.ID, userID, l.Status, l.CreatedAt, string(state))
	return l, err
}
func (s *MySQLStore) List(ctx context.Context, userID string) ([]Lesson, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM buddy_nuance_lessons WHERE user_id=? ORDER BY created_at ASC,id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Lesson{}
	for rows.Next() {
		l, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s *MySQLStore) Get(ctx context.Context, userID, id string) (Lesson, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM buddy_nuance_lessons WHERE user_id=? AND id=?`, userID, id))
}
func (s *MySQLStore) Delete(ctx context.Context, userID, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM buddy_nuance_lessons WHERE user_id=? AND id=?`, userID, id)
	return err
}
func (s *MySQLStore) SetStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET status=?,revision=revision+1 WHERE id=? AND status<>?`, status, id, StatusDone)
	return err
}
func (s *MySQLStore) Complete(ctx context.Context, id string, c Content) error {
	if err := c.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, status string
	err = tx.QueryRowContext(ctx, `SELECT user_id,status FROM buddy_nuance_lessons WHERE id=? FOR UPDATE`, id).Scan(&userID, &status)
	// First successful generation wins even if a recovered worker finishes late.
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status == StatusDone) {
		return nil
	}
	if err != nil {
		return err
	}
	key := c.comparisonKey()
	// Legacy lessons have no comparison key. Check all of them without rewriting
	// existing duplicates or their practice history during the schema upgrade.
	if err = checkLegacyComparisons(ctx, tx, userID, key); err != nil {
		return err
	}
	// The unique key arbitrates simultaneous draws across replicas. Reserving it
	// and publishing the content in one transaction prevents partial completions.
	_, err = tx.ExecContext(ctx, `INSERT INTO buddy_nuance_comparisons (user_id,comparison_key,lesson_id) VALUES (?,?,?)`, userID, key[:], id)
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrDuplicate
	}
	if err != nil {
		return err
	}
	content, _ := json.Marshal(c)
	if _, err = tx.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET content_json=?,status=?,revision=revision+1 WHERE id=?`, string(content), StatusDone, id); err != nil {
		return err
	}
	return tx.Commit()
}

func checkLegacyComparisons(ctx context.Context, tx *sql.Tx, userID string, key [32]byte) error {
	rows, err := tx.QueryContext(ctx, `SELECT l.content_json FROM buddy_nuance_lessons l
		LEFT JOIN buddy_nuance_comparisons c ON c.lesson_id=l.id
		WHERE l.user_id=? AND l.content_json IS NOT NULL AND c.lesson_id IS NULL`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var old Content
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		if old.comparisonKey() == key {
			return ErrDuplicate
		}
	}
	return rows.Err()
}

// StartReview draws one durable five-question batch from every due context,
// regardless of which generated comparison owns it. The row locks make the
// draw and all per-lesson queues one atomic operation across tabs/replicas.
func (s *MySQLStore) StartReview(ctx context.Context, userID string) (ReviewBatch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewBatch{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM buddy_nuance_lessons WHERE user_id=? AND status=? ORDER BY id FOR UPDATE`, userID, StatusDone)
	if err != nil {
		return ReviewBatch{}, err
	}
	lessons := []Lesson{}
	for rows.Next() {
		l, scanErr := scan(rows)
		if scanErr != nil {
			rows.Close()
			return ReviewBatch{}, scanErr
		}
		lessons = append(lessons, l)
	}
	if err = rows.Close(); err != nil {
		return ReviewBatch{}, err
	}
	if err = rows.Err(); err != nil {
		return ReviewBatch{}, err
	}

	now := time.Now().Unix()
	reveals := []ReviewItem{}
	candidates := []ReviewItem{}
	for _, l := range lessons {
		if l.Content == nil {
			continue
		}
		// Resume a saved reveal before replacing any queue from that lesson.
		if l.State.Feedback != nil {
			reveals = append(reveals, ReviewItem{LessonID: l.ID, QuestionID: l.State.Feedback.QuestionID})
			continue
		}
		for _, q := range l.Content.Questions {
			if l.State.Progress[q.ID].NextReviewAt <= now {
				candidates = append(candidates, ReviewItem{LessonID: l.ID, QuestionID: q.ID})
			}
		}
	}
	shuffle := func(items []ReviewItem) error {
		for i := len(items) - 1; i > 0; i-- {
			n, randomErr := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
			if randomErr != nil {
				return randomErr
			}
			j := int(n.Int64())
			items[i], items[j] = items[j], items[i]
		}
		return nil
	}
	if err = shuffle(reveals); err != nil {
		return ReviewBatch{}, err
	}
	if err = shuffle(candidates); err != nil {
		return ReviewBatch{}, err
	}
	candidates = append(reveals, candidates...)
	if len(candidates) > ReviewBatchSize {
		candidates = candidates[:ReviewBatchSize]
	}
	queues := map[string][]string{}
	for _, item := range candidates {
		queues[item.LessonID] = append(queues[item.LessonID], item.QuestionID)
	}
	for i := range lessons {
		queue, selected := queues[lessons[i].ID]
		if !selected || lessons[i].State.Feedback != nil {
			continue
		}
		lessons[i].State.Queue = queue
		lessons[i].State.Feedback = nil
		lessons[i].Revision++
		state, _ := json.Marshal(lessons[i].State)
		if _, err = tx.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET state_json=?,revision=? WHERE user_id=? AND id=?`, string(state), lessons[i].Revision, userID, lessons[i].ID); err != nil {
			return ReviewBatch{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return ReviewBatch{}, err
	}
	return ReviewBatch{Items: candidates, Lessons: lessons}, nil
}

func (s *MySQLStore) Act(ctx context.Context, userID, id string, a Action) (Lesson, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lesson{}, err
	}
	defer tx.Rollback()
	l, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM buddy_nuance_lessons WHERE user_id=? AND id=? FOR UPDATE`, userID, id))
	if err != nil {
		return l, err
	}
	previousFeedback := l.State.Feedback
	now := time.Now()
	if err = l.Apply(a, now); err != nil {
		return Lesson{}, err
	}
	if a.Kind == "answer" {
		f := l.State.Feedback
		_, err = tx.ExecContext(ctx, `INSERT INTO buddy_nuance_attempts (lesson_id,revision,question_id,selected_word,correct,reviewed_at) VALUES (?,?,?,?,?,?)`, id, l.Revision, f.QuestionID, f.Selected, f.Correct, now.Unix())
	} else if a.Kind == "next" && a.Repeat && previousFeedback.Correct {
		_, err = tx.ExecContext(ctx, `UPDATE buddy_nuance_attempts SET repeat_review=TRUE WHERE lesson_id=? AND revision=?`, id, previousFeedback.AnswerRevision)
	}
	if err != nil {
		return Lesson{}, err
	}
	state, _ := json.Marshal(l.State)
	_, err = tx.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET state_json=?,revision=? WHERE id=?`, string(state), l.Revision, id)
	if err != nil {
		return Lesson{}, err
	}
	return l, tx.Commit()
}
