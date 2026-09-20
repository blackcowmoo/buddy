package transport

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/nuance"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
	"buddy/server/internal/writing"
)

type deletedOwner struct{ deleted atomic.Bool }

func (s *deletedOwner) WorkExists(context.Context, string, string) (bool, error) {
	return !s.deleted.Load(), nil
}

type deletingModel struct {
	owner *deletedOwner
	calls int
}

func (m *deletingModel) Complete(context.Context, string, []llm.Message, bool) (string, error) {
	m.calls++
	m.owner.deleted.Store(true)
	return "late response", nil
}
func (m *deletingModel) ChatStream(ctx context.Context, model string, msgs []llm.Message, token func(string)) (string, error) {
	return m.Complete(ctx, model, msgs, false)
}

type guardedSessionStore struct {
	*fakeStore
	*deletedOwner
}
type guardedArticleStore struct {
	*fakeNewsArticleStore
	*deletedOwner
}
type guardedWordStore struct {
	*fakeWordReviewStore
	*deletedOwner
}
type guardedNuanceStore struct {
	*nuanceJobStore
	*deletedOwner
}
type guardedWritingStore struct {
	writing.Store
	*deletedOwner
	completed bool
}

func (s *guardedWritingStore) Get(context.Context, string, string) (writing.Prompt, error) {
	return writing.Prompt{ID: "item", Status: writing.StatusPending}, nil
}
func (s *guardedWritingStore) List(context.Context, string) ([]writing.Prompt, error) {
	return nil, nil
}
func (s *guardedWritingStore) Complete(context.Context, string, string) error {
	s.completed = true
	return nil
}
func (s *guardedWritingStore) Fail(context.Context, string) error {
	return errors.New("must not mark deletion failed")
}

func TestItemJobsDiscardDeletionDuringModelCall(t *testing.T) {
	for _, kind := range []string{"article", "article translation", "writing", "nuance", "word verification", "word question", "reply", "correction", "translation", "title", "summary", "quiz"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			owner := &deletedOwner{}
			model := &deletingModel{owner: owner}
			pipe := &pipeline.Pipeline{LLM: model, ChatModel: "chat", Analysis: []pipeline.Candidate{{LLM: model, Model: "analysis"}}}
			st := &guardedSessionStore{newFakeStore(), owner}
			if err := st.SaveTurn(ctx, "alex", "item", 1, "user", "he go", false, "text"); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCorrection(ctx, "alex", "item", 1, protocol.Correction{Original: "he go", Corrected: "he goes", Issues: []protocol.Issue{{Type: "grammar", Explanation: "agreement"}}}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "article", "article translation":
				a := newsarticle.Article{ID: "item", Status: newsarticle.StatusPending}
				if kind == "article translation" {
					a.Status = newsarticle.StatusDone
					a.Summary = "read me"
				}
				articles := &guardedArticleStore{newFakeNewsArticleStore(a), owner}
				speaker := &fakeSpeaker{}
				if kind == "article" {
					err = runArticleStudy(ctx, pipe, articles, &ArticleAudio{Client: speaker}, "item", "s", "t", "d")
				} else {
					err = RunArticleTranslationBackfill(ctx, pipe, articles, "item")
				}
				got, _, _ := articles.GetArticle(ctx, "item")
				if got.Status != a.Status || got.Translation != "" || len(speaker.spokenTexts()) != 0 {
					t.Fatalf("deleted article was published or spoken: %+v", got)
				}
			case "writing":
				writingStore := &guardedWritingStore{deletedOwner: owner}
				err = generateWritingPrompt(ctx, pipe, writingStore, "item", "alex", "")
				if writingStore.completed {
					t.Fatal("deleted prompt completed")
				}
			case "nuance":
				lessons := &guardedNuanceStore{&nuanceJobStore{lesson: nuance.Lesson{ID: "item", Status: nuance.StatusPending}}, owner}
				err = NuanceJobHandler(pipe, lessons, func(context.Context, string) (string, error) { return "", nil })(ctx, asyncjob.Job{Payload: mustPayload(nuanceJobPayload{"alex", "item"})})
				if lessons.completionCalls != 0 || lessons.lesson.Status == nuance.StatusFailed {
					t.Fatal("deleted lesson completed/failed")
				}
			case "word verification", "word question":
				status := wordreview.StatusPending
				if kind == "word question" {
					status = wordreview.StatusVerified
				}
				words := &guardedWordStore{newFakeWordReviewStore(wordreview.Word{ID: "item", UserID: "alex", Word: "hello", Status: status}), owner}
				err = runWordVerify(ctx, pipe, words, "alex", "item", wordreview.CurrentQuestionVersion)
				got, _ := words.Get(ctx, "alex", "item")
				if got.Status != status || got.ReviewQuestion.Version != 0 {
					t.Fatalf("word published: %+v", got)
				}
			case "reply":
				err = ReplyJobHandler(pipe, st, nil, func(string) { t.Fatal("deleted reply callback") })(ctx, asyncjob.Job{Payload: mustPayload(replyJobPayload{UserID: "alex", SessionID: "item", Turn: 1, Fallback: "fallback"})})
			case "correction":
				err = CorrectionJobHandler(pipe, st, nil, nil, nil, nil, func(string, []protocol.Issue, string) { t.Fatal("deleted correction callback") })(ctx, asyncjob.Job{Payload: mustPayload(correctionJobPayload{UserID: "alex", SessionID: "item", Turn: 1, Text: "he go", ChatDraft: "draft"})})
			case "translation":
				err = TranslationJobHandler(pipe, st, func(string) { t.Fatal("deleted translation callback") })(ctx, asyncjob.Job{Payload: mustPayload(liveTranslationJobPayload{UserID: "alex", SessionID: "item", Turn: 1, Text: "hello"})})
			case "title":
				err = TitleJobHandler(pipe, st)(ctx, asyncjob.Job{Payload: mustPayload(titleJobPayload{UserID: "alex", SessionID: "item", Transcript: []llm.Message{{Role: "user", Content: "hello"}}})})
			case "summary":
				err = runStudySummary(ctx, pipe, st, "alex", "item")
			case "quiz":
				err = runStudyQuiz(ctx, pipe, st, "alex", "item")
			}
			if !errors.Is(err, workguard.ErrDeleted) {
				t.Fatalf("error=%v, calls=%d", err, model.calls)
			}
			if model.calls != 1 {
				t.Fatalf("model calls=%d", model.calls)
			}
		})
	}
}
