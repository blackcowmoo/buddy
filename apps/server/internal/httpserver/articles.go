package httpserver

import (
	"context"
	"math/rand"
	"net/http"

	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/newsfeed"
	"buddy/server/internal/pipeline"
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
}

func toArticleDraw(inst newsarticle.Instance) articleDraw {
	return articleDraw{
		ID:      inst.ID,
		Source:  inst.Article.Source,
		Title:   inst.Article.Title,
		Summary: inst.Article.Summary,
		Choices: inst.Article.Choices,
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
// before, generating its English summary + quiz on first-ever draw across
// every learner (see newsarticle.Store.SaveArticle's URL-keyed cache) or
// reusing an already-cached result. Responds 204 with no body if every
// candidate from internal/newsfeed has already been drawn by this learner —
// the frontend's cue that there's nothing new to offer right now, distinct
// from an actual fetch/generation failure (a 5xx).
//
// The LLM call on a cache miss runs synchronously in the request path, same
// "quick, on-demand, no per-keystroke pressure" tolerance as
// wordSuggestHandler's SuggestWords call — this only ever runs once per
// unique article URL, not on every draw, so its cost is amortized across
// every learner who later draws the same story.
//
// fetchCandidates is newsfeed.FetchCandidates in production (see server.go);
// taking it as a parameter, rather than calling the package function
// directly, is what lets handler tests substitute a fake feed without
// reaching the real internet.
func articleDrawHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, fetchCandidates func(context.Context) ([]newsfeed.Candidate, error)) http.HandlerFunc {
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

		article, cached, err := articles.FindArticleByURL(r.Context(), pick.URL)
		if err != nil {
			serverError(w, "articles: find by url", err)
			return
		}
		if !cached {
			study, err := pipe.GenerateArticleStudy(r.Context(), pick.Source, pick.Title, pick.Description)
			if err != nil {
				serverError(w, "articles: generate study", err)
				return
			}
			article, err = articles.SaveArticle(r.Context(), newsarticle.Article{
				Source:       pick.Source,
				Title:        pick.Title,
				URL:          pick.URL,
				Summary:      study.Summary,
				Choices:      study.Choices,
				CorrectIndex: study.CorrectIndex,
				Explanation:  study.Explanation,
			})
			if err != nil {
				serverError(w, "articles: save article", err)
				return
			}
		}

		inst, err := articles.CreateInstance(r.Context(), userID, article.ID)
		if err != nil {
			serverError(w, "articles: create instance "+userID, err)
			return
		}
		writeJSON(w, toArticleDraw(inst))
	}
}

// articleAnswerHandler records the caller's choice for one of their own
// article-quiz instances and returns the reveal — correctness is always
// computed server-side against the stored answer key (see
// newsarticle.Store.Answer), never trusting a client-supplied verdict, same
// reasoning as pipeline.Pipeline.CheckQuizAnswer's caller.
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
		inst, err := articles.Answer(r.Context(), userID, r.PathValue("id"), body.SelectedIndex)
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
