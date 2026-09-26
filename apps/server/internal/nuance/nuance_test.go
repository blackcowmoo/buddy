package nuance

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func testContent() Content {
	c := Content{Meaning: "저렴한", Distinction: "가격과 품질", Caveat: "문맥에 따라 달라요", Words: []Word{
		{Word: "cheap", Tone: "부정적일 수 있음", Description: "가격이나 품질", Example: "It looks cheap.", Translation: "싸구려 같아요."},
		{Word: "inexpensive", Tone: "중립", Description: "가격", Example: "It is inexpensive.", Translation: "저렴해요."},
	}}
	for i := 0; i < 5; i++ {
		c.Questions = append(c.Questions, Question{ID: fmt.Sprintf("q%d", i), Context: "상황", Sentence: fmt.Sprintf("Item %d is ____.", i), Translation: "번역", Answer: c.Words[i%2].Word, Explanation: "선택에 따른 차이"})
	}
	return c
}
func testLesson() Lesson {
	c := testContent()
	return Lesson{ID: "lesson", Status: StatusDone, Content: &c, State: State{Progress: map[string]Progress{}, Queue: []string{}}}
}
func apply(t *testing.T, l *Lesson, now time.Time, a Action) {
	t.Helper()
	a.Revision = l.Revision
	if err := l.Apply(a, now); err != nil {
		t.Fatal(err)
	}
}

func TestPracticeRepeatsMistakesAndResumesFeedback(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := testLesson()
	apply(t, &l, now, Action{Kind: "start"})
	apply(t, &l, now, Action{Kind: "answer", QuestionID: "q0", Selected: "inexpensive"})
	if l.State.Feedback.Correct || l.State.Queue[0] != "q0" || l.State.Progress["q0"].NextReviewAt != now.Unix() {
		t.Fatalf("lost reveal or immediate review: %+v", l.State)
	}
	// Nothing advances until the saved reveal is acknowledged.
	apply(t, &l, now, Action{Kind: "next"})
	if got := fmt.Sprint(l.State.Queue); got != "[q1 q2 q3 q4 q0]" {
		t.Fatal(got)
	}
	for len(l.State.Queue) > 0 {
		id := l.State.Queue[0]
		answer := ""
		for _, q := range l.Content.Questions {
			if q.ID == id {
				answer = q.Answer
			}
		}
		apply(t, &l, now, Action{Kind: "answer", QuestionID: id, Selected: answer})
		apply(t, &l, now, Action{Kind: "next"})
	}
	p := l.State.Progress["q0"]
	if p.Attempts != 2 || p.Correct != 1 || p.Stage != 1 || p.NextReviewAt != now.Add(24*time.Hour).Unix() {
		t.Fatalf("bad schedule: %+v", p)
	}
	apply(t, &l, now, Action{Kind: "start"})
	if got := fmt.Sprint(l.State.Queue); got != "[q0 q1 q2 q3 q4]" {
		t.Fatalf("individual practice did not restore every question: %s", got)
	}
	if got := l.State.Progress["q0"]; got != p {
		t.Fatalf("restarting individual practice changed schedule: before=%+v after=%+v", p, got)
	}
}

func TestPracticeConfidentAndForcedGuessScheduling(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			now := time.Unix(1700000000, 0)
			l := testLesson()
			l.State.Queue = []string{"q0"}
			l.State.Progress["q0"] = Progress{Stage: 3}
			apply(t, &l, now, Action{Kind: "answer", QuestionID: "q0", Selected: "cheap"})
			apply(t, &l, now, Action{Kind: "next", Repeat: repeat})
			wantStage, days := 4, 7
			if repeat {
				wantStage, days = 3, 4
			}
			p := l.State.Progress["q0"]
			if p.Stage != wantStage || p.NextReviewAt != now.Add(time.Duration(days)*24*time.Hour).Unix() {
				t.Fatalf("repeat=%v: %+v", repeat, p)
			}
		})
	}
}

func TestPracticeRejectsStaleOutOfOrderAndInvalidAnswers(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		action Action
		err    error
	}{
		{Action{Kind: "answer", QuestionID: "q0", Selected: "cheap", Revision: 0}, ErrConflict},
		{Action{Kind: "answer", QuestionID: "q1", Selected: "cheap", Revision: 1}, ErrConflict},
		{Action{Kind: "answer", QuestionID: "q0", Selected: "unknown", Revision: 1}, ErrInvalid},
		{Action{Kind: "next", Revision: 1}, ErrConflict},
		{Action{Kind: "delete", Revision: 1}, ErrInvalid},
	}
	for _, tc := range cases {
		l := testLesson()
		apply(t, &l, now, Action{Kind: "start"})
		if err := l.Apply(tc.action, now); !errors.Is(err, tc.err) {
			t.Fatalf("%+v: %v", tc.action, err)
		}
		if len(l.State.Progress) > 0 || l.Revision != 1 {
			t.Fatal("invalid action changed progress")
		}
	}
}

func TestValidateModelContent(t *testing.T) {
	if err := testContent().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Content){
		"missing translation": func(c *Content) { c.Questions[0].Translation = "" },
		"unknown answer":      func(c *Content) { c.Questions[0].Answer = "affordable" },
		"duplicate id":        func(c *Content) { c.Questions[1].ID = c.Questions[0].ID },
		"duplicate sentence":  func(c *Content) { c.Questions[1].Sentence = c.Questions[0].Sentence },
		"two blanks":          func(c *Content) { c.Questions[0].Sentence = "____ and ____" },
		"wrong blank":         func(c *Content) { c.Questions[0].Sentence = "It is _____." },
		"no coverage": func(c *Content) {
			for i := range c.Questions {
				c.Questions[i].Answer = "cheap"
			}
		},
		"no caveat":           func(c *Content) { c.Caveat = "" },
		"duplicate word":      func(c *Content) { c.Words[1].Word = "CHEAP" },
		"only four questions": func(c *Content) { c.Questions = c.Questions[:4] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := testContent()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid generated lesson accepted")
			}
		})
	}
}

func TestMissingAnswersAndSupplementValidation(t *testing.T) {
	c := testContent()
	for i := range c.Questions {
		c.Questions[i].Answer = "cheap"
	}
	if got := fmt.Sprint(c.MissingAnswers()); got != "[inexpensive]" {
		t.Fatalf("MissingAnswers() = %s", got)
	}
	valid := Question{
		Context: "격식을 갖춘 가격 안내문", Sentence: "This option is ____.",
		Translation: "이 선택지는 저렴합니다.", Answer: "inexpensive",
		Explanation: "inexpensive는 중립적이고 cheap은 품질이 낮다는 인상을 줄 수 있어요.",
	}
	if err := c.ValidateSupplement([]Question{valid}); err != nil {
		t.Fatal(err)
	}
	for name, questions := range map[string][]Question{
		"missing question":   nil,
		"wrong answer":       {{Context: valid.Context, Sentence: valid.Sentence, Translation: valid.Translation, Answer: "cheap", Explanation: valid.Explanation}},
		"duplicate sentence": {{Context: valid.Context, Sentence: c.Questions[0].Sentence, Translation: valid.Translation, Answer: valid.Answer, Explanation: valid.Explanation}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := c.ValidateSupplement(questions); !errors.Is(err, ErrInvalid) {
				t.Fatalf("ValidateSupplement() error = %v", err)
			}
		})
	}
}

func TestComparisonKeyUsesTheWholeNormalizedWordSet(t *testing.T) {
	key := func(words ...string) [32]byte {
		c := Content{}
		for _, word := range words {
			c.Words = append(c.Words, Word{Word: word})
		}
		return c.comparisonKey()
	}
	for _, tc := range []struct {
		name  string
		a, b  []string
		equal bool
	}{
		{"reversed and capitalized", []string{"cheap", "inexpensive"}, []string{"Inexpensive", "CHEAP"}, true},
		{"whitespace", []string{"put off", "postpone"}, []string{" postpone ", "PUT\t OFF"}, true},
		{"three words", []string{"cheap", "inexpensive", "affordable"}, []string{"AFFORDABLE", "cheap", "inexpensive"}, true},
		{"different pair", []string{"cheap", "inexpensive"}, []string{"cheap", "affordable"}, false},
		{"additional word", []string{"cheap", "inexpensive"}, []string{"cheap", "inexpensive", "affordable"}, false},
		{"unambiguous encoding", []string{"a / b", "c"}, []string{"a", "b / c"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := key(tc.a...) == key(tc.b...); got != tc.equal {
				t.Fatalf("comparison equality=%v, want %v", got, tc.equal)
			}
		})
	}
}
