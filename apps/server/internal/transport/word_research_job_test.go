package transport

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
)

type researchJobStore struct {
	*fakeWordReviewStore
	*deletedOwner
	wordreview.ResearchStore
	saved              []wordreview.ResearchSuggestion
	savedUser, savedID string
	saveErr            error
	finishCount        int
}

func (s *researchJobStore) FinishResearch(_ context.Context, before wordreview.Word, results []wordreview.ResearchSuggestion) (wordreview.Word, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedUser, s.savedID, s.saved = before.UserID, before.ID, results
	s.finishCount++
	if s.saveErr != nil {
		return wordreview.Word{}, s.saveErr
	}
	before.ResearchStatus, before.ResearchResults = wordreview.ResearchDone, results
	if len(results) == 0 {
		before.ResearchStatus = wordreview.ResearchFailed
	}
	s.words[before.ID] = before
	return before, nil
}

func TestWordResearchQueuedAndInlineShareExecution(t *testing.T) {
	for _, mode := range []string{"queued", "inline"} {
		t.Run(mode, func(t *testing.T) {
			for _, scenario := range []string{"original spelling", "legacy spelling", "missing", "deleted", "model error", "save error"} {
				t.Run(scenario, func(t *testing.T) {
					word := wordreview.Word{ID: "word", UserID: "alex", Word: "learn", OriginalWord: "learned", Example: "I learned English.", ResearchStatus: wordreview.ResearchPending}
					if scenario == "legacy spelling" {
						word.OriginalWord = ""
					}
					st := &researchJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}}
					if scenario == "missing" {
						delete(st.words, word.ID)
					}
					st.deleted.Store(scenario == "deleted")
					if scenario == "save error" {
						st.saveErr = errors.New("save failed")
					}
					var inputs []string
					pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func(msgs []llm.Message) (string, error) {
						inputs = append(inputs, msgs[len(msgs)-1].Content)
						if scenario == "model error" {
							return "", errors.New("model unavailable")
						}
						return `{"word":"learn","valid":true,"suggestions":[{"word":"learn","meaning":"배우다","example":"I learn English."}]}`, nil
					}}, ChatModel: "chat"}
					ctx := context.Background()
					var err error
					if mode == "queued" {
						err = WordResearchJobHandler(pipe, st)(ctx, asyncjob.Job{Payload: mustPayload(wordResearchJobPayload{UserID: word.UserID, WordID: word.ID})})
					} else {
						err = RunWordResearchInline(ctx, pipe, st, word.UserID, word.ID, word.ResearchRevision)
					}
					switch scenario {
					case "deleted":
						if !errors.Is(err, workguard.ErrDeleted) || len(inputs) != 0 || st.saved != nil {
							t.Fatalf("deleted work ran: inputs=%v saved=%v err=%v", inputs, st.saved, err)
						}
						return
					case "missing":
						if err != nil || len(inputs) != 0 || st.saved != nil {
							t.Fatalf("missing work ran: inputs=%v saved=%v err=%v", inputs, st.saved, err)
						}
						return
					case "model error":
						if err == nil || st.saved != nil {
							t.Fatalf("model failure was not preserved: saved=%v err=%v", st.saved, err)
						}
						wantStatus := wordreview.ResearchPending
						if mode == "inline" {
							wantStatus = wordreview.ResearchFailed
						}
						if st.words[word.ID].ResearchStatus != wantStatus {
							t.Fatalf("model failure status=%s, want %s", st.words[word.ID].ResearchStatus, wantStatus)
						}
						return
					case "save error":
						if !errors.Is(err, st.saveErr) {
							t.Fatalf("save error = %v, want %v", err, st.saveErr)
						}
					default:
						if err != nil {
							t.Fatal(err)
						}
					}
					spelling := word.OriginalWord
					if spelling == "" {
						spelling = word.Word
					}
					if len(inputs) != 3 || !strings.HasPrefix(inputs[0], "word: "+spelling+"\n") || !strings.Contains(inputs[1], word.Example) {
						t.Fatalf("lookup did not preserve source spelling/context: %v", inputs)
					}
					want := []wordreview.ResearchSuggestion{{Word: "learn", Meaning: "배우다", Example: "I learn English.", Verified: true}}
					if st.savedUser != word.UserID || st.savedID != word.ID || !reflect.DeepEqual(st.saved, want) {
						t.Fatalf("saved to %s/%s: %+v, want %s/%s: %+v", st.savedUser, st.savedID, st.saved, word.UserID, word.ID, want)
					}
				})
			}
		})
	}
}

func TestWordResearchExhaustionIsTerminalAndPreservesRejection(t *testing.T) {
	for _, mode := range []string{"queued", "inline"} {
		t.Run(mode, func(t *testing.T) {
			word := wordreview.Word{ID: "word", UserID: "alex", Word: "recapitalize", Meaning: "재자본화하다", Example: "They recapitalize the road.",
				Status: wordreview.StatusRejected, VerifyReason: "wrong object", ResearchStatus: wordreview.ResearchPending, ResearchRevision: 3}
			st := &researchJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}}
			calls := 0
			pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
				calls++
				return `{"word":"recapitalize","suggestions":[{"word":"recapitalize","meaning":"새 자본을 공급하다","example":"They recapitalize the road."}]}`, nil
			}}}
			run := func() error {
				if mode == "inline" {
					return RunWordResearchInline(context.Background(), pipe, st, word.UserID, word.ID, word.ResearchRevision)
				}
				return WordResearchJobHandler(pipe, st)(context.Background(), asyncjob.Job{Payload: mustPayload(wordResearchJobPayload{UserID: word.UserID, WordID: word.ID})})
			}
			if err := run(); err != nil {
				t.Fatalf("semantic exhaustion must not retry as a queue error: %v", err)
			}
			want := word
			want.ResearchStatus = wordreview.ResearchFailed
			if !reflect.DeepEqual(st.words[word.ID], want) || calls != 4 || st.finishCount != 1 {
				t.Fatalf("word=%+v calls=%d finishes=%d", st.words[word.ID], calls, st.finishCount)
			}
			if err := run(); err != nil || calls != 4 || st.finishCount != 1 {
				t.Fatalf("replayed terminal work: calls=%d finishes=%d err=%v", calls, st.finishCount, err)
			}
		})
	}
}

func TestWordResearchLastQueueFailureTerminatesPolling(t *testing.T) {
	word := wordreview.Word{ID: "word", UserID: "alex", Word: "bank", ResearchStatus: wordreview.ResearchPending}
	st := &researchJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}}
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) { return "", errors.New("unavailable") }}}
	err := WordResearchJobHandler(pipe, st)(context.Background(), asyncjob.Job{Attempts: asyncjob.MaxAttempts - 1, Payload: mustPayload(wordResearchJobPayload{UserID: word.UserID, WordID: word.ID})})
	if err == nil || st.words[word.ID].ResearchStatus != wordreview.ResearchFailed || st.finishCount != 1 {
		t.Fatalf("last failure not persisted: word=%+v finishes=%d err=%v", st.words[word.ID], st.finishCount, err)
	}
}

func TestWordResearchDeletionDuringGenerationCannotPublishResults(t *testing.T) {
	word := wordreview.Word{ID: "word", UserID: "alex", Word: "bank", ResearchStatus: wordreview.ResearchPending}
	st := &researchJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}}
	calls := 0
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
		calls++
		if calls == 2 {
			st.deleted.Store(true)
		}
		return `{"word":"bank","suggestions":[{"word":"bank","meaning":"은행","example":"I went to the bank."}]}`, nil
	}}}
	err := RunWordResearchInline(context.Background(), pipe, st, word.UserID, word.ID, word.ResearchRevision)
	if !errors.Is(err, workguard.ErrDeleted) || st.finishCount != 0 || calls != 2 {
		t.Fatalf("deleted work continued: calls=%d finishes=%d err=%v", calls, st.finishCount, err)
	}
}

func TestWordResearchReplacementInvalidatesInFlightReviewQuestion(t *testing.T) {
	word := wordreview.Word{ID: "word", UserID: "alex", Word: "bank", Meaning: "은행", Example: "They sat on the river bank.", Status: wordreview.StatusVerified}
	st := newFakeWordReviewStore(word)
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
		st.mu.Lock()
		updated := st.words[word.ID]
		updated.Example = "I went to the bank."
		updated.MeaningRevision++
		st.words[word.ID] = updated
		st.mu.Unlock()
		return `{"prompt":"They sat on the river ___.","answers":["bank"]}`, nil
	}}}
	if err := RunWordVerifyInline(context.Background(), pipe, st, word.UserID, word.ID); err != nil {
		t.Fatal(err)
	}
	if wordreview.QuestionReady(st.words[word.ID]) || st.words[word.ID].Example != "I went to the bank." {
		t.Fatalf("old question overwrote replacement: %+v", st.words[word.ID])
	}
}

func TestWordResearchStaleQueueRevisionDoesNotConsumeNewRequest(t *testing.T) {
	word := wordreview.Word{ID: "word", UserID: "alex", Word: "bank", ResearchStatus: wordreview.ResearchPending, ResearchRevision: 2}
	st := &researchJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}}
	err := WordResearchJobHandler(nil, st)(context.Background(), asyncjob.Job{Payload: mustPayload(wordResearchJobPayload{UserID: word.UserID, WordID: word.ID, Revision: 1})})
	if err != nil || st.finishCount != 0 || !reflect.DeepEqual(st.words[word.ID], word) {
		t.Fatalf("stale job consumed new request: %+v err=%v", st.words[word.ID], err)
	}
}

type pausedResearchCompletion struct {
	*researchJobStore
	firstSaved, releaseFirst, secondSaved chan struct{}
}

func (s *pausedResearchCompletion) FinishResearch(ctx context.Context, before wordreview.Word, results []wordreview.ResearchSuggestion) (wordreview.Word, error) {
	w, err := s.researchJobStore.FinishResearch(ctx, before, results)
	if before.ResearchRevision == 1 {
		close(s.firstSaved)
		<-s.releaseFirst
	} else {
		close(s.secondSaved)
	}
	return w, err
}

func TestWordResearchNewRequestSurvivesPreviousQueueClaim(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	word := wordreview.Word{ID: "word", UserID: t.Name(), Word: "bank", ResearchStatus: wordreview.ResearchPending, ResearchRevision: 1}
	st := &pausedResearchCompletion{
		researchJobStore: &researchJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}},
		firstSaved:       make(chan struct{}), releaseFirst: make(chan struct{}), secondSaved: make(chan struct{}),
	}
	t.Cleanup(func() {
		close(st.releaseFirst)
		// Join both queue completions so a repeated test starts without leftover
		// claims or goroutines touching the shared test Redis connection.
		waitForCondition(t, 5*time.Second, func() bool {
			for _, suffix := range []string{":r1", ":r2"} {
				pending, err := queue.Pending(context.Background(), asyncjob.KindWordResearch, word.UserID+"/"+word.ID+suffix)
				if err != nil {
					t.Fatal(err)
				}
				if pending {
					return false
				}
			}
			return true
		})
	})
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
		return `{"word":"bank","valid":true,"suggestions":[{"word":"bank","meaning":"은행","example":"I went to the bank."}]}`, nil
	}}}
	ctx := context.Background()
	if err := EnqueueWordResearchJob(ctx, queue, pipe, st, word.UserID, word.ID, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.firstSaved:
	case <-time.After(5 * time.Second):
		t.Fatal("first research did not reach persistence")
	}
	// Results are visible, but the old handler still owns its Redis claim.
	st.mu.Lock()
	word.ResearchRevision = 2
	st.words[word.ID] = word
	st.mu.Unlock()
	if err := EnqueueWordResearchJob(ctx, queue, pipe, st, word.UserID, word.ID, 2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.secondSaved:
	case <-time.After(5 * time.Second):
		t.Fatal("new research was lost behind the previous queue claim")
	}
	got, err := st.Get(ctx, word.UserID, word.ID)
	if err != nil || got.ResearchStatus != wordreview.ResearchDone || got.ResearchRevision != 2 || len(got.ResearchResults) != 1 || !got.ResearchResults[0].Verified {
		t.Fatalf("new request did not finish: %+v err=%v", got, err)
	}
}
