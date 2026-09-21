package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
)

type checkOwner struct{ deleted atomic.Bool }

func (s *checkOwner) WorkExists(context.Context, string, string) (bool, error) {
	return !s.deleted.Load(), nil
}

type ownedWriting struct {
	*fakeWritingStore
	*checkOwner
}

func TestAnswerChecksDiscardDeletedOwner(t *testing.T) {
	for _, kind := range []string{"writing", "session quiz", "word quiz"} {
		t.Run(kind, func(t *testing.T) {
			for _, before := range []bool{true, false} {
				t.Run(map[bool]string{true: "before call", false: "during call"}[before], func(t *testing.T) {
					owner := &checkOwner{}
					owner.deleted.Store(before)
					calls := 0
					model := &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
						calls++
						owner.deleted.Store(true)
						return `{"correct":true}`, nil
					}}
					pipe := &pipeline.Pipeline{LLM: model, ChatModel: "chat"}
					ident := fakeIdentifier{id: "alex", ok: true}
					cache := &fakeAnswerCache{}
					var handler http.HandlerFunc
					var body string
					switch kind {
					case "writing":
						handler = writingCheckHandler(ident, pipe, &ownedWriting{&fakeWritingStore{}, owner})
						body = `{"promptId":"item","prompt":"문장","answer":"sentence"}`
					case "session quiz":
						handler = quizAnswerCheckWithOwners(ident, pipe, quizOwners{Sessions: owner}, cache)
						body = `{"sessionId":"item","prompt":"p","answer":"a","learnerAnswer":"b"}`
					case "word quiz":
						handler = quizAnswerCheckWithOwners(ident, pipe, quizOwners{Words: owner}, cache)
						body = `{"wordId":"item","prompt":"p","answer":"a","learnerAnswer":"b"}`
					}
					rec := httptest.NewRecorder()
					handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)))
					if rec.Code != http.StatusNotFound {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					want := 1
					if before {
						want = 0
					}
					if calls != want {
						t.Fatalf("calls=%d", calls)
					}
					if cache.saves != 0 {
						t.Fatal("deleted check cached a verdict")
					}
				})
			}
		})
	}
}
