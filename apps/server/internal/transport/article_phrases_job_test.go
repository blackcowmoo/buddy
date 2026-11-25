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
// counts phrases- and hints-phase calls separately (one job run makes one
// call of each), returns canned responses, and can block inside Complete so
// a test can dispatch a second generation while the first is still running.
// The two pipeline prompts are told apart by the batch hint prompt's JSON
// shape, which the single phrases prompt never contains.
type fakePhraseLLM struct {
	mu              sync.Mutex
	phrasesCalls    int
	hintsCalls      int
	phrasesResponse string
	// hintsResponse is returned by the hint phase; empty means the hint
	// phase "fails" (an unparsable reply), exercising the phrases- survive-hints-fail path.
	hintsResponse string
	block         chan struct{}
}

func (f *fakePhraseLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errors.New("phrase extraction must use the non-streaming chat path")
}

func (f *fakePhraseLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	isHints := len(msgs) > 0 && strings.Contains(msgs[0].Content, `"hints":[`)
	f.mu.Lock()
	canned := f.phrasesResponse
	if isHints {
		f.hintsCalls++
		canned = f.hintsResponse
	} else {
		f.phrasesCalls++
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if canned == "" {
		return "", errors.New("fakePhraseLLM: no canned response configured for this call")
	}
	return canned, nil
}

func (f *fakePhraseLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phrasesCalls + f.hintsCalls
}

func (f *fakePhraseLLM) phrasesCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phrasesCalls
}

func (f *fakePhraseLLM) hintsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hintsCalls
}

func phrasesPipe(llmClient *fakePhraseLLM) *pipeline.Pipeline {
	return &pipeline.Pipeline{LLM: llmClient, ChatModel: "chat"}
}

func donePhrasesArticle(id string) newsarticle.Article {
	return newsarticle.Article{ID: id, Source: "BBC", Title: "T", URL: "https://example.com/" + id,
		Summary: phrasesTestSummary, Status: newsarticle.StatusDone}
}

const phrasesTestHintJSON = `{"hints":[{"phrase":"in spite of","hint":"두 가지 사이를 이어 주는 연결 표현이에요."},{"phrase":"months of silence","hint":"아무 말도 하지 않던 시간의 길이를 나타내는 말이에요."}]}`

// TestRunArticlePhrasesGeneratesThenReusesCache guards the feature's cost
// model: one Chat call per phase per shared Article (list, then the hint
// map keyed by that exact list), and every later caller (any reader of the
// same story, or the study job racing the lazy read) is served straight
// from the stored answers with zero further calls.
func TestRunArticlePhrasesGeneratesThenReusesCache(t *testing.T) {
	articles := newFakeNewsArticleStore(donePhrasesArticle("a1"))
	client := &fakePhraseLLM{phrasesResponse: `{"phrases":["in spite of","months of silence"]}`, hintsResponse: phrasesTestHintJSON}

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
	if len(got.KeyPhraseHints) != 2 || got.KeyPhraseHints["in spite of"] == "" || got.KeyPhraseHints["months of silence"] == "" {
		t.Fatalf("KeyPhraseHints = %v, want a hint for every stored phrase keyed by the exact phrase", got.KeyPhraseHints)
	}

	if err := runArticlePhrases(context.Background(), phrasesPipe(client), articles, "a1"); err != nil {
		t.Fatalf("second runArticlePhrases() error = %v", err)
	}
	if client.callCount() != 2 {
		t.Fatalf("LLM calls = %d, want exactly 2 (one per phase, cached answers reused)", client.callCount())
	}
}

// TestRunArticlePhrasesHintFailureKeepsPhrases guards the phase boundary: a
// hint pass that fails must not lose the phrase list that already landed —
// the job reports the error so the durable queue retries only the missing
// phase (the next run skips phrases, as the stored list is non-empty).
func TestRunArticlePhrasesHintFailureKeepsPhrases(t *testing.T) {
	articles := newFakeNewsArticleStore(donePhrasesArticle("a2"))
	failing := &fakePhraseLLM{phrasesResponse: `{"phrases":["in spite of"]}`}
	if err := runArticlePhrases(context.Background(), phrasesPipe(failing), articles, "a2"); err == nil {
		t.Fatal("expected an error when the hint pass has no canned answer")
	}
	got, _, err := articles.GetArticle(context.Background(), "a2")
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if strings.Join(got.KeyPhrases, "|") != "in spite of" || len(got.KeyPhraseHints) > 0 {
		t.Fatalf("after hint failure: phrases = %v, hints = %v, want the list kept and hints left empty", got.KeyPhrases, got.KeyPhraseHints)
	}

	// The retry pays only for the hint phase.
	retry := &fakePhraseLLM{hintsResponse: `{"hints":[{"phrase":"in spite of","hint":"이어 주는 연결 표현이에요."}]}`}
	if err := runArticlePhrases(context.Background(), phrasesPipe(retry), articles, "a2"); err != nil {
		t.Fatalf("retry runArticlePhrases() error = %v", err)
	}
	if retry.phrasesCallCount() != 0 {
		t.Fatalf("phrases calls on retry = %d, want 0 (the stored list was reused)", retry.phrasesCallCount())
	}
	got, _, err = articles.GetArticle(context.Background(), "a2")
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if len(got.KeyPhraseHints) != 1 || got.KeyPhraseHints["in spite of"] == "" {
		t.Fatalf("KeyPhraseHints = %v, want the retried hint persisted", got.KeyPhraseHints)
	}
}

// TestRunArticlePhrasesBackfillsLegacyRow guards a done article whose list
// was generated before hints existed: the run must skip the finished phrase
// phase entirely and generate only the missing hints.
func TestRunArticlePhrasesBackfillsLegacyRow(t *testing.T) {
	legacy := donePhrasesArticle("legacy")
	legacy.KeyPhrases = []string{"in spite of"}
	articles := newFakeNewsArticleStore(legacy)
	client := &fakePhraseLLM{hintsResponse: `{"hints":[{"phrase":"in spite of","hint":"이어 주는 연결 표현이에요."}]}`}

	if err := runArticlePhrases(context.Background(), phrasesPipe(client), articles, "legacy"); err != nil {
		t.Fatalf("runArticlePhrases() error = %v", err)
	}
	if client.phrasesCallCount() != 0 {
		t.Fatalf("phrases calls = %d, want 0 (a legacy row's list must never be re-paid)", client.phrasesCallCount())
	}
	got, _, err := articles.GetArticle(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if len(got.KeyPhraseHints) != 1 || got.KeyPhraseHints["in spite of"] == "" {
		t.Fatalf("KeyPhraseHints = %v, want hints backfilled behind the existing list", got.KeyPhraseHints)
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
	client := &fakePhraseLLM{phrasesResponse: `{"phrases":["in spite of"]}`}

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
// article ID, and it persists both shared answers when it finishes.
func TestDispatchArticlePhrasesInlineDeduplicatesWhileRunning(t *testing.T) {
	articles := newFakeNewsArticleStore(donePhrasesArticle("dedupe-1"))
	release := make(chan struct{})
	client := &fakePhraseLLM{phrasesResponse: `{"phrases":["in spite of"]}`, hintsResponse: `{"hints":[{"phrase":"in spite of","hint":"이어 주는 연결 표현이에요."}]}`, block: release}
	pipe := phrasesPipe(client)
	ctx := context.Background()

	if err := DispatchArticlePhrases(ctx, nil, pipe, articles, "dedupe-1"); err != nil {
		t.Fatalf("first DispatchArticlePhrases() error = %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool { return client.phrasesCallCount() == 1 })
	if err := DispatchArticlePhrases(ctx, nil, pipe, articles, "dedupe-1"); err != nil {
		t.Fatalf("second DispatchArticlePhrases() error = %v", err)
	}
	close(release)

	waitForCondition(t, 2*time.Second, func() bool {
		a, _, err := articles.GetArticle(context.Background(), "dedupe-1")
		return err == nil && len(a.KeyPhrases) == 1 && len(a.KeyPhraseHints) == 1
	})
	if got := client.phrasesCallCount(); got != 1 {
		t.Fatalf("phrases calls = %d, want 1 (inline runner dedupes by article ID)", got)
	}
}
