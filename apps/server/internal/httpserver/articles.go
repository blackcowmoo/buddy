package httpserver

import (
	"context"
	"math/rand"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/newsfeed"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
)

// articleListItem mirrors one newsarticle.Instance for the "오늘의 아티클" list
// page — the past-attempts view, same role as InstantSessions.tsx's list.
// Deliberately excludes Choices/CorrectIndex/Explanation: the list only
// shows what a learner already saw (source/title/summary) plus their own
// outcome, never re-exposing the quiz's answer key.
type articleListItem struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Answered  bool   `json:"answered"`
	Correct   bool   `json:"correct"`
	CreatedAt int64  `json:"createdAt"`
	// Status is "pending" (still being generated in the background), "done",
	// or "failed" (see newsarticle.Article.Status) — lets a reopened list
	// show a draw that's still generating instead of an empty summary, and
	// resume watching it via articleInstanceHandler (see lib/articles.ts's
	// fetchArticleInstance).
	Status string `json:"status"`
}

func toArticleListItem(inst newsarticle.Instance) articleListItem {
	return articleListItem{
		ID:        inst.ID,
		Source:    inst.Article.Source,
		Title:     inst.Article.Title,
		Summary:   inst.Article.Summary,
		Answered:  inst.Answered,
		Correct:   inst.Correct,
		CreatedAt: inst.CreatedAt.Unix(),
		Status:    inst.Article.Status,
	}
}

// articleDraw is what articleDrawHandler returns right after a fresh draw:
// enough to render the reading view (source/title/summary) and, once the
// learner asks to see it, the quiz's Choices — but never CorrectIndex/
// Explanation, which only articleAnswerHandler's response reveals, after
// the learner has actually picked one. Never trust a client to keep the
// answer key hidden on its own; the server just never sends it yet.
type articleDraw struct {
	ID      string   `json:"id"`
	Source  string   `json:"source"`
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Choices []string `json:"choices"`
	// Status is "pending" (the study content is still generating in the
	// background — Summary/Choices are empty) or "done"/"failed" — see
	// newsarticle.Article.Status. The frontend polls articleInstanceHandler
	// until this leaves "pending", the same way it would if the learner had
	// stayed on the page the whole time; nothing about generation itself
	// depends on this poll continuing (see asyncjob.KindArticleStudy).
	Status string `json:"status"`
}

func toArticleDraw(inst newsarticle.Instance) articleDraw {
	return articleDraw{
		ID:      inst.ID,
		Source:  inst.Article.Source,
		Title:   inst.Article.Title,
		Summary: inst.Article.Summary,
		Choices: inst.Article.Choices,
		Status:  inst.Article.Status,
	}
}

// articleResult is what articleAnswerHandler returns — the reveal a learner
// sees right after picking a choice: whether they got it right, which
// choice was actually accurate, and why, in their native language.
type articleResult struct {
	Correct      bool   `json:"correct"`
	CorrectIndex int    `json:"correctIndex"`
	Translation  string `json:"translation"` // the accurate choice's own text
	Explanation  string `json:"explanation"`
}

func toArticleResult(inst newsarticle.Instance) articleResult {
	return articleResult{
		Correct:      inst.Correct,
		CorrectIndex: inst.Article.CorrectIndex,
		Translation:  inst.Article.Choices[inst.Article.CorrectIndex],
		Explanation:  inst.Article.Explanation,
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

// articleDrawHandler draws a fresh news article the caller hasn't drawn
// before, reserving a StatusPending row (see newsarticle.Store.
// ReserveArticle's URL-keyed cache) and kicking off its English summary +
// quiz generation in the background on first-ever draw across every
// learner, or reusing an already-cached/in-flight result. Responds 204 with
// no body if every candidate from internal/newsfeed has already been drawn
// by this learner — the frontend's cue that there's nothing new to offer
// right now, distinct from an actual fetch/generation failure (a 5xx).
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
func articleDrawHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, fetchCandidates func(context.Context) ([]newsfeed.Candidate, error), articleStudyQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}

		used, err := articles.UsedURLs(r.Context(), userID)
		if err != nil {
			serverError(w, "articles: used urls "+userID, err)
			return
		}
		candidates, err := fetchCandidates(r.Context())
		if err != nil {
			serverError(w, "articles: fetch candidates", err)
			return
		}
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
		pick := fresh[rand.Intn(len(fresh))]

		article, err := articles.ReserveArticle(r.Context(), pick.Source, pick.Title, pick.URL)
		if err != nil {
			serverError(w, "articles: reserve "+userID, err)
			return
		}
		if article.Status == newsarticle.StatusPending {
			enqueueOrRunInline(articleStudyQueue, r.Context(),
				"articles: enqueue study "+article.ID,
				func(ctx context.Context) error {
					return transport.EnqueueArticleStudyJob(ctx, articleStudyQueue, pipe, articles, article.ID, pick.Source, pick.Title, pick.Description)
				},
				"articles: study "+article.ID,
				func(ctx context.Context) error {
					return transport.RunArticleStudyInline(ctx, pipe, articles, article.ID, pick.Source, pick.Title, pick.Description)
				},
			)
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
func articleInstanceHandler(ident identity.Identifier, articles newsarticle.Store) http.HandlerFunc {
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
		writeJSON(w, toArticleDraw(inst))
	}
}

// articleAnswerHandler records the caller's choice for one of their own
// article-quiz instances and returns the reveal — correctness is always
// computed server-side against the stored answer key (see
// newsarticle.Store.Answer), never trusting a client-supplied verdict, same
// reasoning as pipeline.Pipeline.CheckQuizAnswer's caller. Rejects answering
// an instance whose Article is still StatusPending/StatusFailed with 409:
// there's no answer key yet to score against (Choices is empty), and the
// frontend never offers the quiz UI before articleInstanceHandler's poll
// reports "done" — reaching this branch means a stale/racing client request,
// not a normal user action.
func articleAnswerHandler(ident identity.Identifier, articles newsarticle.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			SelectedIndex int `json:"selectedIndex"`
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
		inst, err := articles.Answer(r.Context(), userID, id, body.SelectedIndex)
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
