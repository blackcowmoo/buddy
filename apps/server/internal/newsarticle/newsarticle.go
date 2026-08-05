// Package newsarticle persists "오늘의 아티클" — a news article drawn from
// internal/newsfeed, summarized into one English study paragraph plus a
// Korean-translation multiple-choice quiz (see
// pipeline.Pipeline.GenerateArticleStudy) — and each learner's own attempts
// at it.
//
// Two tables back this, for the same reason internal/wordreview's Word and
// this package's Instance are separate concerns: Article is the
// expensive-to-produce, globally shared result of one LLM call per unique
// article URL — cached so a second learner (or a retry) who draws the same
// story never re-pays that cost — while Instance is cheap, per-learner
// state: which article a specific learner drew, whether they've answered,
// and what they picked. Excluding a learner's own already-drawn articles
// from future draws (see httpserver's article-draw handler) is done by URL,
// scoped to userID via Instance, so two learners never compete over the
// same pool of "already seen" stories, and there's no daily limit — same
// "instant, unlimited, own list" shape as store.Store's instant/"오늘의 한
// 문장" conversations.
package newsarticle

import (
	"context"
	"time"
)

// Article is one news story, summarized and quizzed exactly once no matter
// how many learners eventually draw it — see the package doc.
type Article struct {
	ID     string
	Source string // outlet name, e.g. "BBC"
	Title  string
	URL    string // canonical source link; the cache/de-dupe key
	// Summary is a single English study paragraph synthesized from the
	// feed's own title/snippet (see pipeline.GenerateArticleStudy) — not a
	// verbatim copy of the source article. Empty while Status is
	// StatusPending.
	Summary string
	// Choices are four Korean-language candidate translations/
	// interpretations of Summary, exactly one of them (at CorrectIndex)
	// accurate — the other three are plausible-looking but wrong (a swapped
	// fact, a flipped negation, a wrong entity) rather than obviously
	// unrelated, so guessing without having actually read Summary is a real
	// gamble. Never sent to the learner before they ask to see the quiz, and
	// CorrectIndex/Explanation are never sent before they answer — see
	// httpserver's handlers. Empty while Status is StatusPending.
	Choices      []string
	CorrectIndex int
	// Explanation is a Korean-language note on why Choices[CorrectIndex] is
	// the accurate one — shown only after the learner answers.
	Explanation string
	// Description is the feed's own short snippet (newsfeed.Candidate.
	// Description) that pipeline.GenerateArticleStudy needs as input,
	// persisted here — not just held transiently in the draw request — so
	// StalePending's orphan sweep can retry generation from the DB alone
	// after this replica dies mid-job, without needing the original
	// newsfeed.Candidate or a durable Redis job payload. Never sent to the
	// learner.
	Description string
	// Status is StatusPending/StatusDone/StatusFailed — see those constants'
	// doc comments. Every Article starts StatusPending, reserved by
	// ReserveArticle the instant a fresh URL is first drawn, before
	// pipeline.GenerateArticleStudy's LLM call even starts (see
	// httpserver.articleDrawHandler) — so the draw response, and the
	// Instance it's attached to, exist immediately and survive the learner
	// navigating away mid-generation (see asyncjob.KindArticleStudy, which
	// finishes the job independent of that request).
	Status    string
	CreatedAt time.Time
}

// Article.Status values — mirrors store.JobStatus*/wordreview.Status*'s
// three-state shape for a background-generated result.
const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Instance is one learner's own attempt at one Article — the "instant
// conversation"-style unit this feature lists and never limits per day (see
// the package doc); only which articles a learner has already drawn is ever
// restricted (see Store.UsedURLs).
type Instance struct {
	ID       string
	UserID   string
	Article  Article // denormalized in full for display — see Store.List
	Answered bool
	// SelectedIndex is -1 until Answered.
	SelectedIndex int
	Correct       bool
	CreatedAt     time.Time
}

// Store persists the shared Article cache and each learner's own Instances,
// scoped per user the same way internal/wordreview and internal/store are.
type Store interface {
	// ReserveArticle atomically creates a StatusPending Article row for url if
	// one doesn't already exist under any status, or returns the existing row
	// (whatever its current status — StatusPending, StatusDone, or
	// StatusFailed) if it does — same insert-ignore-then-reselect shape as
	// wordreview.Store.Save, so a race with another learner's simultaneous
	// draw of the same story never creates two rows. Callers check the
	// returned Status: StatusPending means this call (or a concurrent racer)
	// owns kicking off pipeline.GenerateArticleStudy (see
	// asyncjob.KindArticleStudy); anything else means the study content is
	// already there (or already being generated by an earlier draw) and
	// nothing new needs to run. description is persisted onto the reserved
	// row (see Article.Description's doc comment) even though a fresh draw
	// also passes it straight to the same-request generation call — it's the
	// only way a later orphan-sweep retry (see StalePending) can regenerate
	// without the original newsfeed.Candidate.
	ReserveArticle(ctx context.Context, source, title, url, description string) (Article, error)
	// CompleteArticle fills in a StatusPending Article's generated study
	// content and flips Status to StatusDone. A no-op that just re-reads the
	// row if it's no longer StatusPending (e.g. another attempt already
	// completed it first) — same race-tolerant idempotence ReserveArticle's
	// insert-ignore gives the reservation itself.
	CompleteArticle(ctx context.Context, id, summary string, choices []string, correctIndex int, explanation string) (Article, error)
	// FailArticle marks a StatusPending Article StatusFailed after a
	// pipeline.GenerateArticleStudy attempt errored — observability only; the
	// asyncjob reaper still retries the job from scratch regardless (see
	// asyncjob.Queue.Execute), same reasoning as store.Store.FailStudyQuiz.
	FailArticle(ctx context.Context, id string) error
	// GetArticle returns one Article by id — (Article{}, false, nil) if it
	// doesn't exist. Used by asyncjob.KindArticleStudy's handler to check
	// whether the work it was handed is already done before redoing the LLM
	// call, same early-exit reasoning as runWordVerify.
	GetArticle(ctx context.Context, id string) (Article, bool, error)
	// StalePending returns every StatusPending Article last (re)claimed more
	// than olderThan ago — see ClaimArticle. This is the DB-only orphan
	// detection transport.SweepStaleArticleStudies polls on, independent of
	// whether Redis (and asyncjob's own claim-based reaper) is configured at
	// all: a generation goroutine killed mid-job by a crash or a redeploy
	// (see asyncjob's package doc on why an in-process fallback goroutine has
	// no durability of its own) otherwise leaves its row StatusPending
	// forever, with nothing left to ever retry it.
	StalePending(ctx context.Context, olderThan time.Duration) ([]Article, error)
	// ClaimArticle marks id as freshly (re)claimed — refreshing the
	// timestamp StalePending compares against — and reports whether this
	// call actually won the claim: false means id is no longer StatusPending
	// (already completed/failed by a racing attempt), so the caller must
	// skip redispatching it. Guards against two sweepers (or a sweep racing
	// the original in-flight attempt) both kicking off a redundant duplicate
	// generation for the same article.
	ClaimArticle(ctx context.Context, id string) (bool, error)

	// UsedURLs returns every Article URL userID has already drawn an
	// Instance for — the exclusion set httpserver's draw handler filters
	// newsfeed candidates against, so a learner is never offered the same
	// story twice.
	UsedURLs(ctx context.Context, userID string) (map[string]bool, error)
	// CreateInstance records that userID has just drawn articleID, as a
	// brand-new, unanswered Instance.
	CreateInstance(ctx context.Context, userID, articleID string) (Instance, error)
	// List returns userID's own Instances, most recently drawn first, each
	// with its Article populated for display — see InstantSessions.tsx's
	// list page for the frontend shape this mirrors.
	List(ctx context.Context, userID string) ([]Instance, error)
	// Get returns one Instance by id, its Article populated. A no-op — zero
	// Instance, nil error — if id doesn't exist or belongs to a different
	// user, same indistinguishable-from-missing contract as
	// wordreview.Store.Get.
	Get(ctx context.Context, userID, id string) (Instance, error)
	// Answer records userID's choice for Instance id: Answered becomes true,
	// SelectedIndex becomes selectedIndex, and Correct is computed against
	// the Instance's Article.CorrectIndex server-side (never trusting a
	// client-supplied verdict — same reasoning as
	// pipeline.Pipeline.CheckQuizAnswer). Answering is one-shot: if id was
	// already answered, this just returns the existing row unchanged rather
	// than overwriting the first recorded outcome. Returns a zero Instance
	// and nil error if id doesn't exist or belongs to a different user, same
	// indistinguishable-from-missing contract as Get.
	Answer(ctx context.Context, userID, id string, selectedIndex int) (Instance, error)
	// Delete removes one Instance — mirrors
	// store.Store.DeleteSession/wordreview.Store.Delete's "just gone, not
	// archived" contract. Never touches the shared Article row, which other
	// learners' Instances may still reference.
	Delete(ctx context.Context, userID, id string) error
	Close() error
}
