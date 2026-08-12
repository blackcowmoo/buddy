package httpserver

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/concurrent"
	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/newsfeed"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"buddy/server/internal/ttsstore"
)

// articleListItem mirrors one newsarticle.Instance for the "오늘의 아티클" list
// page — the past-attempts view, same role as InstantSessions.tsx's list.
// Deliberately excludes SubQuestions' answer keys: the list only shows what
// a learner already saw (source/title/summary) plus their own outcome,
// never re-exposing the quiz's answer key.
type articleListItem struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Answered  bool   `json:"answered"`
	Correct   bool   `json:"correct"`
	CreatedAt int64  `json:"createdAt"`
	// PublishedAt is the source feed's own publish time (newsarticle.Article.
	// PublishedAt), unix seconds, 0 if unknown — distinct from CreatedAt,
	// which is when this learner drew the story.
	PublishedAt int64 `json:"publishedAt"`
	// Status is "pending" (still being generated in the background), "done",
	// or "failed" (see newsarticle.Article.Status) — lets a reopened list
	// show a draw that's still generating instead of an empty summary, and
	// resume watching it via articleInstanceHandler (see lib/articles.ts's
	// fetchArticleInstance).
	Status string `json:"status"`
}

func toArticleListItem(inst newsarticle.Instance) articleListItem {
	return articleListItem{
		ID:          inst.ID,
		Source:      inst.Article.Source,
		Title:       inst.Article.Title,
		Summary:     inst.Article.Summary,
		Answered:    inst.Answered,
		Correct:     inst.Correct,
		CreatedAt:   inst.CreatedAt.Unix(),
		PublishedAt: articlePublishedAtUnix(inst.Article),
		Status:      inst.Article.Status,
	}
}

// draftSubQuestion is one sub-question as sent before the learner answers:
// the prompt and its 2 options, never CorrectOptionIndex/Explanation — see
// articleDraw's doc comment.
type draftSubQuestion struct {
	Prompt  string   `json:"prompt"`
	Options []string `json:"options"`
}

func toDraftSubQuestions(qs []newsarticle.SubQuestion) []draftSubQuestion {
	out := make([]draftSubQuestion, len(qs))
	for i, q := range qs {
		out[i] = draftSubQuestion{Prompt: q.Prompt, Options: q.Options}
	}
	return out
}

// articleDraw is what articleDrawHandler returns right after a fresh draw:
// enough to render the reading view (source/title/summary) and, once the
// learner asks to see it, the quiz's SubQuestions — but never any entry's
// CorrectOptionIndex/Explanation, which only articleAnswerHandler's
// response reveals, after the learner has actually answered. Never trust a
// client to keep the answer key hidden on its own; the server just never
// sends it yet.
type articleDraw struct {
	ID           string             `json:"id"`
	Source       string             `json:"source"`
	Title        string             `json:"title"`
	Summary      string             `json:"summary"`
	Translation  string             `json:"translation"`
	SubQuestions []draftSubQuestion `json:"subQuestions"`
	// PublishedAt is the source feed's own publish time, unix seconds, 0 if
	// unknown — see articleListItem.PublishedAt's doc comment. Present from
	// the very first draw response, even while Status is still "pending":
	// it's captured at ReserveArticle time, not once generation finishes.
	PublishedAt int64 `json:"publishedAt"`
	// Status is "pending" (the study content is still generating in the
	// background — Summary/SubQuestions are empty) or "done"/"failed" — see
	// newsarticle.Article.Status. The frontend polls articleInstanceHandler
	// until this leaves "pending", the same way it would if the learner had
	// stayed on the page the whole time; nothing about generation itself
	// depends on this poll continuing (see asyncjob.KindArticleStudy).
	Status string `json:"status"`
}

func toArticleDraw(inst newsarticle.Instance) articleDraw {
	return articleDraw{
		ID:           inst.ID,
		Source:       inst.Article.Source,
		Title:        inst.Article.Title,
		Summary:      inst.Article.Summary,
		Translation:  inst.Article.Translation,
		SubQuestions: toDraftSubQuestions(inst.Article.SubQuestions),
		PublishedAt:  articlePublishedAtUnix(inst.Article),
		Status:       inst.Article.Status,
	}
}

// articlePublishedAtUnix reports a.PublishedAt as unix seconds, or 0 for the
// zero time.Time (an unknown/unparsed feed pubDate — see
// newsarticle.Article.PublishedAt's doc comment) rather than the large
// negative number time.Time{}.Unix() would otherwise produce.
func articlePublishedAtUnix(a newsarticle.Article) int64 {
	if a.PublishedAt.IsZero() {
		return 0
	}
	return a.PublishedAt.Unix()
}

// articleSubQuestionResult is one sub-question's reveal — its full answer
// key plus what the learner actually picked, so the client can color each
// one correct/incorrect independently (see quizChoiceClass) rather than
// only knowing the aggregate verdict.
type articleSubQuestionResult struct {
	Prompt              string   `json:"prompt"`
	Options             []string `json:"options"`
	CorrectOptionIndex  int      `json:"correctOptionIndex"`
	SelectedOptionIndex int      `json:"selectedOptionIndex"`
	Correct             bool     `json:"correct"`
	Explanation         string   `json:"explanation"`
}

// articleResult is what articleAnswerHandler returns — the reveal a learner
// sees right after answering every sub-question: an aggregate verdict
// (Correct is true only if every sub-question was), a Score/Total for a
// partial-credit summary line, and each sub-question's own reveal.
type articleResult struct {
	Correct      bool                       `json:"correct"`
	Score        int                        `json:"score"`
	Total        int                        `json:"total"`
	SubQuestions []articleSubQuestionResult `json:"subQuestions"`
}

func toArticleResult(inst newsarticle.Instance) articleResult {
	subs := make([]articleSubQuestionResult, len(inst.Article.SubQuestions))
	score := 0
	for i, q := range inst.Article.SubQuestions {
		selected := -1
		if i < len(inst.SelectedOptions) {
			selected = inst.SelectedOptions[i]
		}
		correct := selected == q.CorrectOptionIndex
		if correct {
			score++
		}
		subs[i] = articleSubQuestionResult{
			Prompt:              q.Prompt,
			Options:             q.Options,
			CorrectOptionIndex:  q.CorrectOptionIndex,
			SelectedOptionIndex: selected,
			Correct:             correct,
			Explanation:         q.Explanation,
		}
	}
	return articleResult{
		Correct:      inst.Correct,
		Score:        score,
		Total:        len(inst.Article.SubQuestions),
		SubQuestions: subs,
	}
}

// articleInstancesListHandler returns the caller's own past article-quiz
// attempts, most recently drawn first — this feature's own dedicated list,
// the same "instant, unlimited, own list" shape as
// instantSessionsListHandler; only already-drawn articles are ever excluded
// (see articleDrawHandler), never a daily count.
func articleInstancesListHandler(ident identity.Identifier, articles newsarticle.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		list, err := articles.List(r.Context(), userID)
		if err != nil {
			serverError(w, "articles: list "+userID, err)
			return
		}
		out := make([]articleListItem, len(list))
		for i, inst := range list {
			out[i] = toArticleListItem(inst)
		}
		writeJSON(w, out)
	}
}

// selfHealIncompleteArticle detects a StatusDone Article left over from
// before the 4-choice -> N-sub-question quiz redesign — SubQuestions is
// empty because sub_questions_json predates that migration entirely — and
// atomically reopens it as StatusPending (see newsarticle.Store.
// ReopenIncompleteArticle) so it can be regenerated exactly like a fresh
// draw, instead of permanently showing a quiz with no questions. The bool
// result is true only when this call actually won the reopen — callers must
// gate re-dispatching generation on it, not on the returned Article's
// Status alone, or a still-legitimately-generating article would get
// re-enqueued on every poll tick (see articleInstanceHandler).
func selfHealIncompleteArticle(ctx context.Context, articles newsarticle.Store, a newsarticle.Article) (newsarticle.Article, bool) {
	if a.Status != newsarticle.StatusDone || len(a.SubQuestions) > 0 {
		return a, false
	}
	won, err := articles.ReopenIncompleteArticle(ctx, a.ID)
	if err != nil {
		log.Printf("articles: reopen incomplete %s: %v", a.ID, err)
		return a, false
	}
	if !won {
		return a, false
	}
	a.Status = newsarticle.StatusPending
	return a, true
}

// dispatchArticleStudy enqueues (or runs inline, if no queue is configured)
// article-study generation for a StatusPending article — the shared body of
// articleDrawHandler's first-draw dispatch and articleInstanceHandler's
// self-heal re-dispatch.
func dispatchArticleStudy(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, articles newsarticle.Store, audio *transport.ArticleAudio, a newsarticle.Article, logSuffix string) {
	asyncjob.EnqueueOrRunInline(queue, ctx,
		"articles: enqueue study"+logSuffix+" "+a.ID,
		func(ctx context.Context) error {
			return transport.EnqueueArticleStudyJob(ctx, queue, pipe, articles, audio, a.ID, a.Source, a.Title, a.Description)
		},
		"articles: study"+logSuffix+" "+a.ID,
		func(ctx context.Context) error {
			return transport.RunArticleStudyInline(ctx, pipe, articles, audio, a.ID, a.Source, a.Title, a.Description)
		},
	)
}

// articleDrawHandler draws the most recent news article the caller hasn't
// drawn before — internal/newsfeed.FetchCandidates already returns every
// outlet's items sorted newest-first, so this just takes the first one left
// after excluding already-drawn URLs — reserving a StatusPending row (see
// newsarticle.Store.ReserveArticle's URL-keyed cache) and kicking off its
// English summary + quiz generation in the background on first-ever draw
// across every learner, or reusing an already-cached/in-flight result.
// Responds 204 with no body if every candidate from internal/newsfeed has
// already been drawn by this learner — the frontend's cue that there's
// nothing new to offer right now, distinct from an actual fetch/generation
// failure (a 5xx).
//
// The response comes back the instant the row is reserved — it never waits
// on pipe.GenerateArticleStudy, which (like pipeline.VerifyWord) can be slow
// against a local model, and — unlike a request-scoped call — must survive
// the learner navigating away before it finishes (see
// asyncjob.KindArticleStudy, enqueued via the same "durable queue when Redis
// is configured, detached inline goroutine otherwise" enqueueOrRunInline
// pattern wordSaveHandler uses). Status "pending" in the response tells the
// frontend to poll articleInstanceHandler until generation lands.
//
// fetchCandidates is newsfeed.FetchCandidates in production (see server.go);
// taking it as a parameter, rather than calling the package function
// directly, is what lets handler tests substitute a fake feed without
// reaching the real internet.
func articleDrawHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, fetchCandidates func(context.Context) ([]newsfeed.Candidate, error), articleStudyQueue *asyncjob.Queue, audio *transport.ArticleAudio) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}

		var used map[string]bool
		var candidates []newsfeed.Candidate
		var usedErr, candErr error
		concurrent.Run(
			func() { used, usedErr = articles.UsedURLs(r.Context(), userID) },
			func() { candidates, candErr = fetchCandidates(r.Context()) },
		)
		if usedErr != nil {
			serverError(w, "articles: used urls "+userID, usedErr)
			return
		}
		if candErr != nil {
			serverError(w, "articles: fetch candidates", candErr)
			return
		}
		// candidates is already sorted newest-first (see
		// newsfeed.FetchCandidates), and filtering here preserves that order,
		// so fresh[0] is the most recent story this learner hasn't drawn yet.
		fresh := make([]newsfeed.Candidate, 0, len(candidates))
		for _, c := range candidates {
			if !used[c.URL] {
				fresh = append(fresh, c)
			}
		}
		if len(fresh) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		pick := fresh[0]

		article, err := articles.ReserveArticle(r.Context(), pick.Source, pick.Title, pick.URL, pick.Description, pick.PublishedAt)
		if err != nil {
			serverError(w, "articles: reserve "+userID, err)
			return
		}
		if healed, reopened := selfHealIncompleteArticle(r.Context(), articles, article); reopened {
			article = healed
		}
		if article.Status == newsarticle.StatusPending {
			dispatchArticleStudy(r.Context(), articleStudyQueue, pipe, articles, audio, article, "")
		}

		inst, err := articles.CreateInstance(r.Context(), userID, article.ID)
		if err != nil {
			serverError(w, "articles: create instance "+userID, err)
			return
		}
		writeJSON(w, toArticleDraw(inst))
	}
}

// articleInstanceHandler returns one of the caller's own article-quiz
// instances — the poll target for a draw still generating in the background
// (see articleDrawHandler), so a learner who navigates away mid-generation
// and comes back — whether by reopening this exact draw or tapping a
// still-pending row in their list (see articleInstancesListHandler) — can
// resume watching it finish instead of losing track of it. Same response
// shape and answer-key-hiding contract as articleDrawHandler; gated to the
// caller's own instance the same way as articleAnswerHandler.
//
// Also the self-heal entry point (see selfHealIncompleteArticle) for an
// Article completed before the 4-choice -> N-sub-question quiz redesign:
// reopening one of those here transitions it back to "pending" and kicks
// off real generation exactly once (gated on actually winning the reopen,
// not just on Status being "pending" — this handler is polled every few
// seconds by a normally-generating draw, and re-enqueuing on every one of
// those ticks would be wasteful/duplicative).
func articleInstanceHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, articleStudyQueue *asyncjob.Queue, audio *transport.ArticleAudio) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		inst, err := articles.Get(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "articles: get "+userID, err)
			return
		}
		if inst.ID == "" {
			http.NotFound(w, r)
			return
		}
		if healed, reopened := selfHealIncompleteArticle(r.Context(), articles, inst.Article); reopened {
			inst.Article = healed
			dispatchArticleStudy(r.Context(), articleStudyQueue, pipe, articles, audio, healed, " (reopen)")
		}
		writeJSON(w, toArticleDraw(inst))
	}
}

// articleAnswerHandler records the caller's choices for one of their own
// article-quiz instances and returns the reveal — correctness is always
// computed server-side against the stored answer key (see
// newsarticle.Store.Answer), never trusting a client-supplied verdict, same
// reasoning as pipeline.Pipeline.CheckQuizAnswer's caller. Rejects answering
// an instance whose Article is still StatusPending/StatusFailed with 409:
// there's no answer key yet to score against (SubQuestions is empty), and
// the frontend never offers the quiz UI before articleInstanceHandler's
// poll reports "done" — reaching this branch means a stale/racing client
// request, not a normal user action. Also rejects a selectedOptions whose
// length doesn't match the article's SubQuestions with 400 — the frontend
// always submits one pick per sub-question (see ArticleQuiz.tsx), so a
// mismatch means a stale draw client-side, not a value worth silently
// scoring as wrong.
func articleAnswerHandler(ident identity.Identifier, articles newsarticle.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			SelectedOptions []int `json:"selectedOptions"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		id := r.PathValue("id")
		existing, err := articles.Get(r.Context(), userID, id)
		if err != nil {
			serverError(w, "articles: get "+userID, err)
			return
		}
		if existing.ID == "" {
			http.NotFound(w, r)
			return
		}
		if existing.Article.Status != newsarticle.StatusDone {
			http.Error(w, "article study still generating", http.StatusConflict)
			return
		}
		if len(body.SelectedOptions) != len(existing.Article.SubQuestions) {
			http.Error(w, "selectedOptions must have one entry per sub-question", http.StatusBadRequest)
			return
		}
		inst, err := articles.Answer(r.Context(), userID, id, body.SelectedOptions)
		if err != nil {
			serverError(w, "articles: answer "+userID, err)
			return
		}
		if inst.ID == "" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, toArticleResult(inst))
	}
}

// articleAudioHandler serves this article's read-aloud audio — generated
// once per shared Article (see transport.ArticleAudio, hooked into the
// article study job right after its summary completes) and cached in S3
// (see internal/ttsstore), so every learner who's drawn this same story
// gets it from cache instead of it being regenerated per instance/learner.
// A cache miss — never pre-generated (e.g. the job's best-effort attempt
// failed), or swept past its TTL (see ttsstore.Store.SweepExpired) — falls
// back to generating on the spot instead of 404ing, buffering the whole
// thing before responding since Kokoro-FastAPI's non-streaming response
// doesn't report a size up front either way. Scoped to the caller's own
// instance the same way articleInstanceHandler is, even though the
// underlying audio is shared, so a learner can't probe another's article ID
// unless they've actually drawn it too.
func articleAudioHandler(ident identity.Identifier, articles newsarticle.Store, audio *transport.ArticleAudio) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if audio == nil {
			http.Error(w, "read-aloud is not configured", http.StatusServiceUnavailable)
			return
		}
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		inst, err := articles.Get(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "articles: audio get "+userID, err)
			return
		}
		if inst.ID == "" {
			http.NotFound(w, r)
			return
		}
		if inst.Article.Status != newsarticle.StatusDone {
			http.Error(w, "article study still generating", http.StatusConflict)
			return
		}

		key := transport.ArticleAudioKey(inst.Article.ID)
		body, err := audio.Cache.Open(r.Context(), key, audio.Client.Version())
		if errors.Is(err, ttsstore.ErrNotFound) {
			generated, genErr := audio.Generate(r.Context(), key, inst.Article.Summary)
			if genErr != nil {
				serverError(w, "articles: audio generate "+inst.Article.ID, genErr)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(generated)
			return
		}
		if err != nil {
			serverError(w, "articles: audio open "+inst.Article.ID, err)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", "audio/mpeg")
		if _, err := io.Copy(w, body); err != nil {
			log.Printf("articles: audio stream %s: %v", inst.Article.ID, err)
		}
	}
}

// articleDeleteHandler removes one of the caller's own article-quiz
// instances — mirrors sessionDeleteHandler/wordDeleteHandler's plain
// "just gone" contract; the shared Article cache row is untouched, since
// other learners' instances may still reference it.
func articleDeleteHandler(ident identity.Identifier, articles newsarticle.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if err := articles.Delete(r.Context(), userID, r.PathValue("id")); err != nil {
			serverError(w, "articles: delete "+userID, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
