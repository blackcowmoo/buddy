package nuance

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

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
	content, _ := json.Marshal(c)
	// First successful generation wins even if a recovered worker finishes late.
	_, err := s.db.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET content_json=?,status=?,revision=revision+1 WHERE id=? AND status<>?`, string(content), StatusDone, id, StatusDone)
	return err
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
