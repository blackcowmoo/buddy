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
	for i := 0; i < 4; i++ {
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
	if got := fmt.Sprint(l.State.Queue); got != "[q1 q2 q3 q0]" {
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
	if len(l.State.Queue) != 0 {
		t.Fatal("early reviews inflate progress")
	}
	apply(t, &l, now.Add(24*time.Hour), Action{Kind: "start"})
	if len(l.State.Queue) != 4 {
		t.Fatal("due questions not restored")
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
		"no caveat":         func(c *Content) { c.Caveat = "" },
		"duplicate word":    func(c *Content) { c.Words[1].Word = "CHEAP" },
		"too few questions": func(c *Content) { c.Questions = c.Questions[:2] },
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
