package transport

import (
	"context"
	"fmt"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/workguard"
)

// ArticlePhrasesClaimTTL mirrors ArticleStudyClaimTTL's reasoning (one call
// against a potentially slow local model). Concurrency stays lower than
// article study's: phrase suggestions are a convenience layer over an
// already-readable article, so their queue may wait behind study work for
// the same model slot rather than compete with it.
const (
	ArticlePhrasesClaimTTL          = 25 * time.Hour
	ArticlePhrasesWorkerConcurrency = 2
)

type articlePhrasesJobPayload struct {
	ArticleID string
}

// articlePhrasesInlineJobs dedupes the no-Redis inline dispatch by article
// ID, so a learner's polling loop against httpserver's phrases GET can't
// stack up concurrent generations while one is already running in-process.
var articlePhrasesInlineJobs asyncjob.InlineRunner

// runArticlePhrases generates and caches one article's key-phrase list.
// Every early-exit is a nil no-op rather than an error: the article is
// gone, its study isn't done (nothing to extract phrases from yet — the
// study job will dispatch this itself right after CompleteArticle), or the
// list already exists (the shared cached answer every future reader reuses,
// which is the entire point of persisting it). SetArticlePhrases's own
// first-writer-wins guard covers the race between two generators that both
// passed the cached check.
func runArticlePhrases(ctx context.Context, pipe *pipeline.Pipeline, articles newsarticle.Store, articleID string) error {
	ctx = workguard.BindStore(ctx, articles, "", articleID)
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	target, ok, err := articles.GetArticle(ctx, articleID)
	if err != nil {
		return fmt.Errorf("article phrases: get: %w", err)
	}
	if !ok || target.Status != newsarticle.StatusDone || len(target.KeyPhrases) > 0 {
		return nil
	}
	phrases, err := pipe.SuggestArticlePhrases(ctx, target.Summary)
	if err != nil {
		return fmt.Errorf("article phrases: generate: %w", err)
	}
	if err := articles.SetArticlePhrases(ctx, articleID, phrases); err != nil {
		return fmt.Errorf("article phrases: save: %w", err)
	}
	return nil
}

// ArticlePhrasesJobHandler builds the asyncjob.Handler for one queued
// phrases job — see StudySummaryJobHandler's doc comment for the shared
// durability rationale.
func ArticlePhrasesJobHandler(pipe *pipeline.Pipeline, articles newsarticle.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindArticlePhrases, func(ctx context.Context, payload articlePhrasesJobPayload) error {
		return runArticlePhrases(ctx, pipe, articles, payload.ArticleID)
	})
}

// DispatchArticlePhrases kicks off phrase generation for one done article:
// durably queued (deduped by article ID) when Redis is configured, best-
// effort in-process otherwise. Both callers — the article study job after
// CompleteArticle, and httpserver's phrases GET on a cache miss — go
// through here so the Redis dedupe key and the InlineRunner key are the
// same key, and neither caller can double-generate against the other.
func DispatchArticlePhrases(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, articles newsarticle.Store, articleID string) error {
	if queue != nil {
		return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindArticlePhrases, articleID, articleID,
			articlePhrasesJobPayload{ArticleID: articleID}, ArticlePhrasesClaimTTL, ArticlePhrasesJobHandler(pipe, articles))
	}
	articlePhrasesInlineJobs.Start(articleID, "article phrases: inline "+articleID, func(ctx context.Context) error {
		return runArticlePhrases(ctx, pipe, articles, articleID)
	})
	return nil
}
