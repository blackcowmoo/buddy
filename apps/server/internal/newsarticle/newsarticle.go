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
	// verbatim copy of the source article.
	Summary string
	// Choices are four Korean-language candidate translations/
	// interpretations of Summary, exactly one of them (at CorrectIndex)
	// accurate — the other three are plausible-looking but wrong (a swapped
	// fact, a flipped negation, a wrong entity) rather than obviously
	// unrelated, so guessing without having actually read Summary is a real
	// gamble. Never sent to the learner before they ask to see the quiz, and
	// CorrectIndex/Explanation are never sent before they answer — see
	// httpserver's handlers.
	Choices      []string
	CorrectIndex int
	// Explanation is a Korean-language note on why Choices[CorrectIndex] is
	// the accurate one — shown only after the learner answers.
	Explanation string
	CreatedAt   time.Time
}

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
	// FindArticleByURL returns the cached Article for url, if any learner has
	// ever drawn it before — (Article{}, false, nil) if not. Callers use this
	// to skip a redundant LLM call (see pipeline.GenerateArticleStudy) when a
	// freshly fetched newsfeed.Candidate's URL is already cached.
	FindArticleByURL(ctx context.Context, url string) (Article, bool, error)
	// SaveArticle caches a freshly generated Article, keyed by URL. A no-op
	// that returns the existing row if this exact URL was already cached by
	// a race with another learner's simultaneous draw (same
	// insert-ignore-then-reselect shape as wordreview.Store.Save).
	SaveArticle(ctx context.Context, a Article) (Article, error)

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
