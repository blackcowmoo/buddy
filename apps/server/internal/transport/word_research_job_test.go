package transport

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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
}

func (s *researchJobStore) FinishResearch(_ context.Context, userID, id string, results []wordreview.ResearchSuggestion) (wordreview.Word, error) {
	s.savedUser, s.savedID, s.saved = userID, id, results
	return wordreview.Word{}, s.saveErr
}

func TestWordResearchQueuedAndInlineShareExecution(t *testing.T) {
	for _, mode := range []string{"queued", "inline"} {
		t.Run(mode, func(t *testing.T) {
			for _, scenario := range []string{"original spelling", "legacy spelling", "missing", "deleted", "model error", "save error"} {
				t.Run(scenario, func(t *testing.T) {
					word := wordreview.Word{ID: "word", UserID: "alex", Word: "learn", OriginalWord: "learned", Example: "I learned English."}
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
						return `{"word":"learn","suggestions":[{"word":"learn","meaning":"배우다","example":"I learn English."}]}`, nil
					}}, ChatModel: "chat"}
					ctx := context.Background()
					var err error
					if mode == "queued" {
						err = WordResearchJobHandler(pipe, st)(ctx, asyncjob.Job{Payload: mustPayload(wordResearchJobPayload{UserID: word.UserID, WordID: word.ID})})
					} else {
						err = RunWordResearchInline(ctx, pipe, st, word.UserID, word.ID)
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
					if len(inputs) != 2 || !strings.HasPrefix(inputs[0], "word: "+spelling+"\n") || !strings.Contains(inputs[1], word.Example) {
						t.Fatalf("lookup did not preserve source spelling/context: %v", inputs)
					}
					want := []wordreview.ResearchSuggestion{{Word: "learn", Meaning: "배우다", Example: "I learn English."}}
					if st.savedUser != word.UserID || st.savedID != word.ID || !reflect.DeepEqual(st.saved, want) {
						t.Fatalf("saved to %s/%s: %+v, want %s/%s: %+v", st.savedUser, st.savedID, st.saved, word.UserID, word.ID, want)
					}
				})
			}
		})
	}
}
