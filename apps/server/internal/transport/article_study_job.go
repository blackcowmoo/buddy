package transport

import (
	"context"
	"fmt"
	"log"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/tts"
	"buddy/server/internal/ttsstore"
)

// ArticleAudio best-effort generates and caches "오늘의 아티클" read-aloud
// audio once, right after its summary is completed (see runArticleStudy) —
// the same text every future learner who draws this article will hear, so
// generating it here means nobody's browser (or a later on-demand request —
// see httpserver's article audio handler, which regenerates lazily on a
// cache miss/TTL expiry instead of failing) ever waits on it from cold.
// A nil *ArticleAudio (BUDDY_TTS_URL unset) just skips generation — same
// "empty config = feature off" convention as recording's S3Bucket — so
// call sites never need to branch on whether TTS is configured.
type ArticleAudio struct {
	Client tts.Speaker
	Cache  ttsstore.Cache
}

// ArticleAudioKey is the ttsstore.Store cache key for articleID's read-aloud
// audio — one function so the job below and httpserver's audio handler
// (which also needs it, to check the cache before falling back to
// on-demand generation) never drift apart on the "article:" prefix.
func ArticleAudioKey(articleID string) string { return "article:" + articleID }

// Generate synthesizes text via Client and caches it under key, returning
// the generated audio bytes. Shared by generate below (the article study
// job's best-effort pre-generation, which discards the error) and
// httpserver's audio handler (which propagates a generation failure to the
// HTTP response instead) so both go through identical generate-then-cache
// logic. Panics if called on a nil *ArticleAudio — callers check that
// first (generate's nil guard; httpserver's handler answers 503 instead of
// calling this at all when TTS isn't configured).
func (a *ArticleAudio) Generate(ctx context.Context, key, text string) ([]byte, error) {
	audio, err := a.Client.Speak(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("tts: generate: %w", err)
	}
	if err := a.Cache.Put(ctx, key, audio); err != nil {
		return nil, fmt.Errorf("tts: cache: %w", err)
	}
	return audio, nil
}

// generate is deliberately swallow-and-log, not error-returning: a failed
// or skipped pre-generation must never fail the article study job itself
// (the summary/quiz are the feature; read-aloud is a bonus, and the audio
// handler's own lazy-generate-on-miss path covers this case anyway).
func (a *ArticleAudio) generate(ctx context.Context, articleID, text string) {
	if a == nil {
		return
	}
	if _, err := a.Generate(ctx, ArticleAudioKey(articleID), text); err != nil {
		log.Printf("article study: tts: %s: %v", articleID, err)
	}
}

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

	// ArticleStudyStaleAfter/ArticleStudySweepInterval bound
	// RunArticleStudySweepLoop's DB-only orphan sweep — see its doc comment.
	// Deliberately much shorter than ArticleStudyClaimTTL: that TTL exists so
	// asyncjob's own Redis-backed reaper never reaps a call that's still
	// legitimately (if slowly) running, whereas this sweep is the *only*
	// recovery path at all when Redis isn't configured (see
	// asyncjob.EnqueueOrRunInline's fallback), so it errs toward retrying
	// sooner. A duplicate retry of a call that's actually still running slow
	// is wasted work, not a correctness problem — runArticleStudy's
	// StatusPending check and CompleteArticle's own guard make two
	// concurrent attempts for the same article converge safely.
	ArticleStudyStaleAfter    = 10 * time.Minute
	ArticleStudySweepInterval = 2 * time.Minute
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
func runArticleStudy(ctx context.Context, pipe *pipeline.Pipeline, articles newsarticle.Store, audio *ArticleAudio, articleID, source, title, description string) error {
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
	audio.generate(ctx, articleID, study.Summary)
	return nil
}

// ArticleStudyJobHandler builds the asyncjob.Handler that runs one queued
// article-study job, independent of any connection or its context — see
// StudySummaryJobHandler's doc comment for the shared durability rationale.
func ArticleStudyJobHandler(pipe *pipeline.Pipeline, articles newsarticle.Store, audio *ArticleAudio) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindArticleStudy, func(ctx context.Context, payload articleStudyJobPayload) error {
		return runArticleStudy(ctx, pipe, articles, audio, payload.ArticleID, payload.Source, payload.Title, payload.Description)
	})
}

// RunArticleStudyInline runs the exact same work as ArticleStudyJobHandler,
// synchronously — mirrors RunWordVerifyInline; the caller is expected to run
// this on a detached context.Background() goroutine of its own so a slow
// LLM call never blocks the draw response.
func RunArticleStudyInline(ctx context.Context, pipe *pipeline.Pipeline, articles newsarticle.Store, audio *ArticleAudio, articleID, source, title, description string) error {
	return runArticleStudy(ctx, pipe, articles, audio, articleID, source, title, description)
}

// EnqueueArticleStudyJob durably queues study generation for a
// just-reserved pending article — mirrors EnqueueWordVerifyJob. Deduped by
// articleID, so multiple learners drawing the same brand-new URL at once
// (each reserving/observing the same StatusPending row) only ever trigger
// one actual generation.
func EnqueueArticleStudyJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, articles newsarticle.Store, audio *ArticleAudio, articleID, source, title, description string) error {
	payload := articleStudyJobPayload{ArticleID: articleID, Source: source, Title: title, Description: description}
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindArticleStudy, articleID,
		articleID, payload, ArticleStudyClaimTTL, ArticleStudyJobHandler(pipe, articles, audio))
}

// SweepStaleArticleStudies resumes generation for every StatusPending
// article newsarticle.Store.StalePending reports abandoned — the DB-only
// safety net behind RunArticleStudySweepLoop. Each candidate is re-claimed
// via ClaimArticle first, atomically: if that fails (already completed/
// failed by the attempt this sweep thought was abandoned, or claimed a
// moment ago by a racing sweep on another replica), it's skipped rather than
// redispatched, so at most one redundant generation ever runs per truly
// abandoned article per sweep tick.
func SweepStaleArticleStudies(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, articles newsarticle.Store, audio *ArticleAudio) error {
	stale, err := articles.StalePending(ctx, ArticleStudyStaleAfter)
	if err != nil {
		return fmt.Errorf("article study: sweep: list stale: %w", err)
	}
	for _, a := range stale {
		claimed, err := articles.ClaimArticle(ctx, a.ID)
		if err != nil {
			log.Printf("article study: sweep: claim %s: %v", a.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		log.Printf("article study: sweep: resuming abandoned generation %s", a.ID)
		asyncjob.EnqueueOrRunInline(queue, ctx,
			"articles: sweep enqueue study "+a.ID,
			func(ctx context.Context) error {
				return EnqueueArticleStudyJob(ctx, queue, pipe, articles, audio, a.ID, a.Source, a.Title, a.Description)
			},
			"articles: sweep study "+a.ID,
			func(ctx context.Context) error {
				return RunArticleStudyInline(ctx, pipe, articles, audio, a.ID, a.Source, a.Title, a.Description)
			},
		)
	}
	return nil
}

// RunArticleStudySweepLoop runs SweepStaleArticleStudies every
// ArticleStudySweepInterval until ctx is canceled — started unconditionally
// at server startup (see cmd/server/main.go), regardless of whether Redis is
// configured, because it's what makes a redeploy- or crash-abandoned "오늘의
// 아티클" draw resume automatically even when articleStudyQueue is nil and
// the original generation was only ever a best-effort in-process goroutine
// with no durability of its own (see asyncjob.EnqueueOrRunInline).
func RunArticleStudySweepLoop(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, articles newsarticle.Store, audio *ArticleAudio) {
	ticker := time.NewTicker(ArticleStudySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := SweepStaleArticleStudies(context.Background(), queue, pipe, articles, audio); err != nil {
				log.Printf("article study: sweep: %v", err)
			}
		}
	}
}
