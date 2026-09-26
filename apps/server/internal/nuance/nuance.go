// Package nuance owns generated word comparisons and durable spaced practice.
package nuance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"buddy/server/internal/wordreview"
)

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusFailed     = "failed"
	ReviewBatchSize  = 5
)

var (
	ErrNotFound  = errors.New("nuance: not found")
	ErrConflict  = errors.New("nuance: progress changed; reload")
	ErrInvalid   = errors.New("nuance: invalid action")
	ErrDuplicate = errors.New("nuance: comparison already exists")
)

type Word struct {
	Word        string `json:"word"`
	Tone        string `json:"tone"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Translation string `json:"translation"`
}
type Question struct {
	ID          string `json:"id"`
	Context     string `json:"context"`
	Sentence    string `json:"sentence"`
	Translation string `json:"translation"`
	Answer      string `json:"answer"`
	Explanation string `json:"explanation"`
}
type Content struct {
	Meaning     string     `json:"meaning"`
	Distinction string     `json:"distinction"`
	Caveat      string     `json:"caveat"`
	Words       []Word     `json:"words"`
	Questions   []Question `json:"questions"`
}

// A comparison is the complete word set, independent of order, case, or spacing.
func (c Content) comparisonKey() [32]byte {
	words := make([]string, len(c.Words))
	for i, w := range c.Words {
		words[i] = strings.ToLower(strings.Join(strings.Fields(w.Word), " "))
	}
	slices.Sort(words)
	encoded, _ := json.Marshal(words)
	return sha256.Sum256(encoded)
}

// Validate rejects incomplete model output before it can enter review rotation.
func (c Content) Validate() error {
	nonempty := func(values ...string) bool {
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return false
			}
		}
		return true
	}
	if !nonempty(c.Meaning, c.Distinction, c.Caveat) || len(c.Words) < 2 || len(c.Words) > 3 || len(c.Questions) < 5 || len(c.Questions) > 6 {
		return fmt.Errorf("%w: incomplete comparison", ErrInvalid)
	}
	words := map[string]bool{}
	for _, w := range c.Words {
		if !nonempty(w.Word, w.Tone, w.Description, w.Example, w.Translation) || words[strings.ToLower(w.Word)] || utf8.RuneCountInString(w.Word) > 255 || w.Word != strings.TrimSpace(w.Word) {
			return fmt.Errorf("%w: invalid word", ErrInvalid)
		}
		words[strings.ToLower(w.Word)] = true
	}
	ids, sentences, used := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, q := range c.Questions {
		if !nonempty(q.ID, q.Context, q.Sentence, q.Translation, q.Explanation) || !words[strings.ToLower(q.Answer)] || !exactAnswer(c.Words, q.Answer) || len(q.ID) > 64 || ids[q.ID] || sentences[q.Sentence] || strings.Count(q.Sentence, "____") != 1 || strings.Contains(strings.Replace(q.Sentence, "____", "", 1), "_") {
			return fmt.Errorf("%w: invalid question", ErrInvalid)
		}
		ids[q.ID], sentences[q.Sentence], used[strings.ToLower(q.Answer)] = true, true, true
	}
	if len(used) != len(words) {
		return fmt.Errorf("%w: each word needs practice", ErrInvalid)
	}
	return nil
}

// MissingAnswers returns words that never appear as the preferred answer.
// The original word order is preserved so a supplement prompt is stable.
func (c Content) MissingAnswers() []string {
	answered := make(map[string]bool, len(c.Questions))
	for _, q := range c.Questions {
		answered[q.Answer] = true
	}
	missing := make([]string, 0, len(c.Words))
	for _, w := range c.Words {
		if !answered[w.Word] {
			missing = append(missing, w.Word)
		}
	}
	return missing
}

// ValidateSupplement accepts exactly one new question for every currently
// uncovered word. Existing legacy questions are left untouched, while each
// addition still has to satisfy the current question contract.
func (c Content) ValidateSupplement(questions []Question) error {
	missing := c.MissingAnswers()
	if len(missing) == 0 && len(questions) == 0 {
		return nil
	}
	if len(questions) != len(missing) {
		return fmt.Errorf("%w: supplement must cover each missing word once", ErrInvalid)
	}
	words, required := map[string]bool{}, map[string]bool{}
	for _, w := range c.Words {
		words[w.Word] = true
	}
	for _, word := range missing {
		required[word] = true
	}
	sentences, covered := map[string]bool{}, map[string]bool{}
	for _, q := range c.Questions {
		sentences[q.Sentence] = true
	}
	for _, q := range questions {
		if !nonemptyQuestion(q) || !words[q.Answer] || !required[q.Answer] || covered[q.Answer] || sentences[q.Sentence] || strings.Count(q.Sentence, "____") != 1 || strings.Contains(strings.Replace(q.Sentence, "____", "", 1), "_") {
			return fmt.Errorf("%w: invalid supplement question", ErrInvalid)
		}
		covered[q.Answer], sentences[q.Sentence] = true, true
	}
	return nil
}

func nonemptyQuestion(q Question) bool {
	for _, value := range []string{q.Context, q.Sentence, q.Translation, q.Answer, q.Explanation} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func exactAnswer(words []Word, answer string) bool {
	for _, w := range words {
		if w.Word == answer {
			return true
		}
	}
	return false
}

type Progress struct {
	Stage          int   `json:"stage"`
	Attempts       int   `json:"attempts"`
	Correct        int   `json:"correct"`
	LastReviewedAt int64 `json:"lastReviewedAt"`
	NextReviewAt   int64 `json:"nextReviewAt"`
}
type Feedback struct {
	QuestionID     string `json:"questionId"`
	Selected       string `json:"selected"`
	Correct        bool   `json:"correct"`
	PreviousStage  int    `json:"previousStage"`
	AnswerRevision int    `json:"answerRevision"`
}
type State struct {
	Progress map[string]Progress `json:"progress"`
	Queue    []string            `json:"queue"`
	Feedback *Feedback           `json:"feedback,omitempty"`
}
type Lesson struct {
	ID        string   `json:"id"`
	UserID    string   `json:"-"`
	Status    string   `json:"status"`
	CreatedAt int64    `json:"createdAt"`
	Revision  int      `json:"revision"`
	Content   *Content `json:"content,omitempty"`
	State     State    `json:"state"`
}
type ReviewItem struct {
	LessonID   string `json:"lessonId"`
	QuestionID string `json:"questionId"`
}
type ReviewBatch struct {
	Items   []ReviewItem `json:"items"`
	Lessons []Lesson     `json:"lessons"`
}
type Action struct {
	Kind       string `json:"kind"`
	Revision   int    `json:"revision"`
	QuestionID string `json:"questionId,omitempty"`
	Selected   string `json:"selected,omitempty"`
	Repeat     bool   `json:"repeat,omitempty"`
}

// Apply runs under the lesson's row lock. Revisions reject double submissions
// and stale tabs; queue + reveal are committed together so reloads resume exactly.
func (l *Lesson) Apply(a Action, now time.Time) error {
	if l.Revision != a.Revision {
		return ErrConflict
	}
	if l.Status != StatusDone || l.Content == nil {
		return ErrInvalid
	}
	switch a.Kind {
	case "start":
		if len(l.State.Queue) != 0 {
			return nil
		}
		// Opening one comparison is deliberate practice, not the scheduled
		// cross-lesson review. Include every saved context here; StartReview
		// separately applies due dates and the five-question batch limit.
		for _, q := range l.Content.Questions {
			l.State.Queue = append(l.State.Queue, q.ID)
		}
	case "answer":
		if len(l.State.Queue) == 0 || l.State.Queue[0] != a.QuestionID || l.State.Feedback != nil {
			return ErrConflict
		}
		valid := false
		for _, w := range l.Content.Words {
			if w.Word == a.Selected {
				valid = true
			}
		}
		if !valid {
			return ErrInvalid
		}
		var question *Question
		for i := range l.Content.Questions {
			if l.Content.Questions[i].ID == a.QuestionID {
				question = &l.Content.Questions[i]
				break
			}
		}
		if question == nil {
			return ErrInvalid
		}
		if l.State.Progress == nil {
			l.State.Progress = map[string]Progress{}
		}
		p := l.State.Progress[a.QuestionID]
		correct := a.Selected == question.Answer
		l.State.Feedback = &Feedback{QuestionID: a.QuestionID, Selected: a.Selected, Correct: correct, PreviousStage: p.Stage, AnswerRevision: l.Revision + 1}
		p.Attempts++
		if correct {
			p.Correct++
		}
		stage, next := wordreview.NextSchedule(p.Stage, correct, false, now)
		p.Stage, p.NextReviewAt, p.LastReviewedAt = stage, next.Unix(), now.Unix()
		l.State.Progress[a.QuestionID] = p
	case "next":
		f := l.State.Feedback
		if f == nil || len(l.State.Queue) == 0 {
			return ErrConflict
		}
		l.State.Queue = l.State.Queue[1:]
		if !f.Correct {
			l.State.Queue = append(l.State.Queue, f.QuestionID)
		}
		if a.Repeat && f.Correct {
			p := l.State.Progress[f.QuestionID]
			stage, next := wordreview.NextSchedule(f.PreviousStage, true, true, time.Unix(p.LastReviewedAt, 0))
			p.Stage, p.NextReviewAt = stage, next.Unix()
			l.State.Progress[f.QuestionID] = p
		}
		l.State.Feedback = nil
	default:
		return ErrInvalid
	}
	l.Revision++
	return nil
}

type Store interface {
	Create(context.Context, string) (Lesson, error)
	List(context.Context, string) ([]Lesson, error)
	Get(context.Context, string, string) (Lesson, error)
	Delete(context.Context, string, string) error
	SetStatus(context.Context, string, string) error
	Complete(context.Context, string, Content) error
	AddQuestions(context.Context, string, string, []Question) error
	StartReview(context.Context, string) (ReviewBatch, error)
	Act(context.Context, string, string, Action) (Lesson, error)
}
