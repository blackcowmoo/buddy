package httpserver

import (
	"net/http"

	"buddy/server/internal/newsarticle"
)

// Every article action resolves the learner-owned instance first; shared
// article IDs alone must never authorize access to another learner's draw.
func requireArticleInstance(w http.ResponseWriter, r *http.Request, articles newsarticle.Store, userID, errLabel string) (newsarticle.Instance, bool) {
	inst, err := articles.Get(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		serverError(w, errLabel+" "+userID, err)
		return newsarticle.Instance{}, false
	}
	if inst.ID == "" {
		http.NotFound(w, r)
		return newsarticle.Instance{}, false
	}
	return inst, true
}

func requireReadyArticleInstance(w http.ResponseWriter, r *http.Request, articles newsarticle.Store, userID, errLabel string) (newsarticle.Instance, bool) {
	inst, ok := requireArticleInstance(w, r, articles, userID, errLabel)
	if !ok {
		return newsarticle.Instance{}, false
	}
	if inst.Article.Status != newsarticle.StatusDone {
		http.Error(w, "article study still generating", http.StatusConflict)
		return newsarticle.Instance{}, false
	}
	return inst, true
}
