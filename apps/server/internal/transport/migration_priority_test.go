package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
)

func TestMigrationsYieldToWordLookupAndAnswerChecks(t *testing.T) {
	for _, path := range []string{"inline meanings", "durable meanings", "meaning preflight", "article translation"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				var calls []string
				model := fakeLLM{completeFn: func(msgs []llm.Message) (string, error) {
					switch {
					case strings.Contains(msgs[len(msgs)-1].Content, "occupy model"):
						calls = append(calls, "occupied")
						<-release
						return `{"correct":true}`, nil
					case strings.Contains(msgs[0].Content, "You normalize one English word"):
						calls = append(calls, "lookup")
						return `{"word":"facility"}`, nil
					case strings.Contains(msgs[0].Content, "who tapped an English word"):
						calls = append(calls, "definition")
						return `{"word":"facility","meaning":"시설","example":"The facility closed."}`, nil
					case strings.Contains(msgs[0].Content, "You are grading"):
						calls = append(calls, "answer")
						return `{"correct":true}`, nil
					default:
						calls = append(calls, "migration")
						if path == "article translation" {
							return "기존 기사 번역", nil
						}
						if path == "meaning preflight" {
							return `{"needsCleanup":false}`, nil
						}
						return `{"sameSense":true,"meaning":"시설"}`, nil
					}
				}}
				pipe := &pipeline.Pipeline{LLM: model, FeedbackLang: "ko"}
				word := wordreview.Word{ID: "w1", UserID: "alex", Word: "facility", Meaning: "시설", Example: "The facility closed.", Status: wordreview.StatusVerified, MeaningStatus: wordreview.MeaningPending, MeaningTargetVersion: wordreview.CurrentMeaningVersion}
				words := &meaningJobStore{fakeWordReviewStore: newFakeWordReviewStore(word), deletedOwner: &deletedOwner{}}
				articles := newFakeNewsArticleStore(newsarticle.Article{ID: "legacy", Summary: "An older article.", Status: newsarticle.StatusDone})
				jobs := []func() error{
					func() error {
						_, err := pipe.CheckQuizAnswer(ctx, "occupy model", "facility", nil, "building")
						return err
					},
					func() error {
						switch path {
						case "durable meanings":
							// A recovered job starts with a fresh context, so the handler
							// must restore priority from its durable cleanup mode.
							payload, err := json.Marshal(wordResearchJobPayload{UserID: word.UserID, CleanupMeanings: true})
							if err != nil {
								return err
							}
							return WordResearchJobHandler(pipe, words)(ctx, asyncjob.Job{Kind: asyncjob.KindWordResearch, Payload: payload})
						case "meaning preflight":
							word.MeaningTargetVersion = wordreview.MeaningOptimizationVersion
							got, err := wordMeaningForCleanup(ctx, pipe, word)
							if err == nil && got != word.Meaning {
								return fmt.Errorf("preflight meaning = %q", got)
							}
							return err
						case "article translation":
							return RunArticleTranslationBackfill(ctx, pipe, articles, "legacy")
						default:
							return RunWordMeaningCleanupInline(ctx, pipe, words, word.UserID)
						}
					},
					func() error {
						got, err := pipe.DefineWord(ctx, "facility", "The facility closed.")
						if err == nil && (got.Word != "facility" || got.Meaning != "시설") {
							return fmt.Errorf("definition = %+v", got)
						}
						return err
					},
					func() error {
						correct, err := pipe.CheckQuizAnswer(ctx, "The ___ closed.", "facility", nil, "building")
						if err == nil && !correct {
							return fmt.Errorf("answer was rejected")
						}
						return err
					},
				}
				for i, job := range jobs {
					go func() {
						if err := job(); err != nil {
							t.Errorf("job %d: %v", i, err)
						}
					}()
					// Register migration before the interactive requests without sleeps.
					synctest.Wait()
				}
				if !slices.Equal(calls, []string{"occupied"}) {
					t.Fatalf("calls overlapped the active model: %v", calls)
				}
				unblock()
				synctest.Wait()
				if len(calls) != 5 || !slices.Equal(calls[:3], []string{"occupied", "lookup", "answer"}) || !slices.Contains(calls[3:], "migration") {
					t.Fatalf("calls = %v, want lookup and answer before migration", calls)
				}
				switch path {
				case "inline meanings", "durable meanings":
					if words.saves != 1 || words.failures != 0 || words.words[word.ID].MeaningStatus != wordreview.MeaningDone {
						t.Fatalf("migration did not resume: saves=%d failures=%d word=%+v", words.saves, words.failures, words.words[word.ID])
					}
				case "article translation":
					article, _, err := articles.GetArticle(ctx, "legacy")
					if err != nil || article.Translation != "기존 기사 번역" {
						t.Fatalf("backfill did not persist: article=%+v err=%v", article, err)
					}
				}
			})
		})
	}
}
