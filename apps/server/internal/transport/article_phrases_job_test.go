package transport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
)

const phrasesTestSummary = "The two sides finally agreed to talk again in spite of months of silence."

// fakePhraseLLM is a chat-only llm.Client double for runArticlePhrases: it
// counts calls, returns a canned response, and can block inside Complete so
// a test can dispatch a second generation while the first is still running.
type fakePhraseLLM struct {
	mu       sync.Mutex
	calls    int
	response string
	block    chan struct{}
}

func (f *fakePhraseLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errors.New("phrase extraction must use the non-streaming chat path")
}

func (f *fakePhraseLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	f.mu.Lock()
	f.calls++
	block, response := f.block, f.response
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return response, nil
}

func (f *fakePhraseLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func phrasesPipe(llmClient *fakePhraseLLM) *pipeline.Pipeline {
	return &pipeline.Pipeline{LLM: llmClient, ChatModel: "chat"}
}

func donePhrasesArticle(id string) newsarticle.Article {
	return newsarticle.Article{ID: id, Source: "BBC", Title: "T", URL: "https://example.com/" + id,
		Summary: phrasesTestSummary, Status: newsarticle.StatusDone}
}

// TestRunArticlePhrasesGeneratesThenReusesCache guards the feature's cost
// model: one Chat call per shared Article, then every later caller (any
// reader of the same story, or the study job racing the lazy read) is served
// straight from the stored list with zero further calls.
func TestRunArticlePhrasesGeneratesThenReusesCache(t *testing.T) {
	articles := newFakeNewsArticleStore(donePhrasesArticle("a1"))
	client := &fakePhraseLLM{response: `{"phrases":["in spite of","months of silence"]}`}

	if err := runArticlePhrases(context.Background(), phrasesPipe(client), articles, "a1"); err != nil {
		t.Fatalf("runArticlePhrases() error = %v", err)
	}
	got, _, err := articles.GetArticle(context.Background(), "a1")
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if strings.Join(got.KeyPhrases, "|") != "in spite of|months of silence" {
		t.Fatalf("KeyPhrases = %v, want the generated list persisted", got.KeyPhrases)
	}

	if err := runArticlePhrases(context.Background(), phrasesPipe(client), articles, "a1"); err != nil {
		t.Fatalf("second runArticlePhrases() error = %v", err)
	}
	if client.callCount() != 1 {
		t.Fatalf("LLM calls = %d, want exactly 1 (cached list reused)", client.callCount())
	}
}

// TestRunArticlePhrasesSkipsArticleWithoutSummaryOrNotDone guards both
// "nothing to extract yet" shapes — a pending article and a gone row — as
// nil no-ops: the study job dispatches phrase generation itself right after
// completing the article, so racing it must not error (which would make the
// durable queue retry against an unfinished summary).
func TestRunArticlePhrasesSkipsArticleWithoutSummaryOrNotDone(t *testing.T) {
	articles := newFakeNewsArticleStore(
		newsarticle.Article{ID: "pending", Summary: phrasesTestSummary, Status: newsarticle.StatusPending},
	)
	client := &fakePhraseLLM{response: `{"phrases":["in spite of"]}`}

	if err := runArticlePhrases(context.Background(), phrasesPipe(client), articles, "pending"); err != nil {
		t.Fatalf("pending article: error = %v, want nil no-op", err)
	}
	if err := runArticlePhrases(context.Background(), phrasesPipe(client), articles, "missing"); err != nil {
		t.Fatalf("missing article: error = %v, want nil no-op", err)
	}
	if client.callCount() != 0 {
		t.Fatalf("LLM calls = %d, want 0", client.callCount())
	}
}

// TestDispatchArticlePhrasesInlineDeduplicatesWhileRunning guards the
// no-Redis half of the dedupe contract (queue.Pending + the durable dedupe
// key cover the Redis half): the learner's polling loop against the phrases
// GET dispatches repeatedly, but only one in-process generation runs per
// article ID, and it persists the shared list when it finishes.
func TestDispatchArticlePhrasesInlineDeduplicatesWhileRunning(t *testing.T) {
	articles := newFakeNewsArticleStore(donePhrasesArticle("dedupe-1"))
	release := make(chan struct{})
	client := &fakePhraseLLM{response: `{"phrases":["in spite of"]}`, block: release}
	pipe := phrasesPipe(client)
	ctx := context.Background()

	if err := DispatchArticlePhrases(ctx, nil, pipe, articles, "dedupe-1"); err != nil {
		t.Fatalf("first DispatchArticlePhrases() error = %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool { return client.callCount() == 1 })
	if err := DispatchArticlePhrases(ctx, nil, pipe, articles, "dedupe-1"); err != nil {
		t.Fatalf("second DispatchArticlePhrases() error = %v", err)
	}
	close(release)

	waitForCondition(t, 2*time.Second, func() bool {
		a, _, err := articles.GetArticle(context.Background(), "dedupe-1")
		return err == nil && len(a.KeyPhrases) == 1
	})
	if got := client.callCount(); got != 1 {
		t.Fatalf("LLM calls = %d, want 1 (inline runner dedupes by article ID)", got)
	}
}
