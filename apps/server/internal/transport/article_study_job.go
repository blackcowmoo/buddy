package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
)

// ArticleStudyClaimTTL/ArticleStudyWorkerConcurrency mirror
// WordVerifyClaimTTL/WordVerifyWorkerConcurrency's reasoning: one
// Analysis-ensemble call against a potentially slow local model (see
// pipeline.Pipeline.GenerateArticleStudy), so the claim TTL stays generous.
// Concurrency is modest — drawing a fresh, never-before-seen article is a
// deliberate, occasional action, not something tapped repeatedly like
// "학습하기".
const (
	ArticleStudyClaimTTL          = 25 * time.Hour
	ArticleStudyWorkerConcurrency = 4
)

// articleStudyJobPayload carries what pipeline.GenerateArticleStudy needs —
// not just ArticleID — because that content only ever existed transiently in
// the newsfeed.Candidate httpserver.articleDrawHandler fetched; the
// newsarticle.Article row itself stays StatusPending (Summary/Choices empty)
// until this job completes it, so there's nothing to re-read off the row on
// a reap-retry.
type articleStudyJobPayload struct {
	ArticleID   string
	Source      string
	Title       string
	Description string
}

// runArticleStudy is the actual work behind asyncjob.KindArticleStudy: check
// the reserved article is still StatusPending (a no-op otherwise — already
// completed by an earlier attempt, or by a racing concurrent draw of the
// same URL), ask pipeline.Pipeline.GenerateArticleStudy to summarize +
// quiz it, and persist the result. Shared by ArticleStudyJobHandler (the
// queued, durable path) and RunArticleStudyInline (httpserver.
// articleDrawHandler's fallback when Redis isn't configured) so both paths
// behave identically.
func runArticleStudy(ctx context.Context, pipe *pipeline.Pipeline, articles newsarticle.Store, articleID, source, title, description string) error {
	target, ok, err := articles.GetArticle(ctx, articleID)
	if err != nil {
		return fmt.Errorf("article study: get: %w", err)
	}
	if !ok || target.Status != newsarticle.StatusPending {
		return nil
	}

	study, err := pipe.GenerateArticleStudy(ctx, source, title, description)
	if err != nil {
		if failErr := articles.FailArticle(context.Background(), articleID); failErr != nil {
			log.Printf("article study: fail %s: %v", articleID, failErr)
		}
		return fmt.Errorf("article study: generate: %w", err)
	}
	if _, err := articles.CompleteArticle(ctx, articleID, study.Summary, study.Choices, study.CorrectIndex, study.Explanation); err != nil {
		return fmt.Errorf("article study: complete: %w", err)
	}
	return nil
}

// ArticleStudyJobHandler builds the asyncjob.Handler that runs one queued
// article-study job, independent of any connection or its context — see
// StudySummaryJobHandler's doc comment for the shared durability rationale.
func ArticleStudyJobHandler(pipe *pipeline.Pipeline, articles newsarticle.Store) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload articleStudyJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("article study job: bad payload: %w", err)
		}
		return runArticleStudy(ctx, pipe, articles, payload.ArticleID, payload.Source, payload.Title, payload.Description)
	}
}

// RunArticleStudyInline runs the exact same work as ArticleStudyJobHandler,
// synchronously — mirrors RunWordVerifyInline; the caller is expected to run
// this on a detached context.Background() goroutine of its own so a slow
// LLM call never blocks the draw response.
func RunArticleStudyInline(ctx context.Context, pipe *pipeline.Pipeline, articles newsarticle.Store, articleID, source, title, description string) error {
	return runArticleStudy(ctx, pipe, articles, articleID, source, title, description)
}

// EnqueueArticleStudyJob durably queues study generation for a
// just-reserved pending article — mirrors EnqueueWordVerifyJob. Deduped by
// articleID, so multiple learners drawing the same brand-new URL at once
// (each reserving/observing the same StatusPending row) only ever trigger
// one actual generation.
func EnqueueArticleStudyJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, articles newsarticle.Store, articleID, source, title, description string) error {
	payload := articleStudyJobPayload{ArticleID: articleID, Source: source, Title: title, Description: description}
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindArticleStudy, articleID,
		articleID, payload, ArticleStudyClaimTTL, ArticleStudyJobHandler(pipe, articles))
}
