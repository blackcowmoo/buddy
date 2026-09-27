package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"github.com/redis/go-redis/v9"
)

func TestArticleActionsRequireOwnedReadyInstance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owner  string
		id     string
		status string
		err    error
		want   int
	}{
		{name: "missing", owner: "alex", id: "missing", status: newsarticle.StatusDone, want: http.StatusNotFound},
		{name: "other learner", owner: "other", id: "draw", status: newsarticle.StatusDone, want: http.StatusNotFound},
		{name: "shared article ID", owner: "alex", id: "article", status: newsarticle.StatusDone, want: http.StatusNotFound},
		{name: "store error", owner: "alex", id: "draw", err: errors.New("private database detail"), want: http.StatusInternalServerError},
		{name: "pending", owner: "alex", id: "draw", status: newsarticle.StatusPending, want: http.StatusConflict},
		{name: "unknown state", owner: "alex", id: "draw", status: "", want: http.StatusConflict},
		{name: "failed", owner: "alex", id: "draw", status: newsarticle.StatusFailed, want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeArticleStore{err: tc.err, byUser: map[string][]newsarticle.Instance{
				tc.owner: {{ID: "draw", UserID: tc.owner, Article: newsarticle.Article{ID: "article", Status: tc.status}}},
			}}
			ident := fakeIdentifier{id: "alex", ok: true}
			// These requests must stop at the article guard before touching Redis
			// or audio generation; no external service participates in the test.
			rdb := redis.NewClient(&redis.Options{MaxRetries: -1, Dialer: func(context.Context, string, string) (net.Conn, error) {
				t.Error("article guard allowed a Redis call")
				return nil, errors.New("unexpected Redis call")
			}})
			t.Cleanup(func() { _ = rdb.Close() })
			for _, action := range []struct {
				name    string
				handler http.HandlerFunc
			}{
				{"answer", articleAnswerHandler(ident, st)},
				{"audio", articleAudioHandler(ident, st, &transport.ArticleAudio{})},
				{"word lookup", articleWordDefineHandler(ident, st, nil, rdb, asyncjob.NewQueue(rdb))},
			} {
				t.Run(action.name, func(t *testing.T) {
					r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"word":"learn","selectedOptions":[]}`))
					r.SetPathValue("id", tc.id)
					w := httptest.NewRecorder()
					action.handler(w, r)
					requireStatus(t, w, tc.want)
					if tc.err != nil && strings.Contains(w.Body.String(), tc.err.Error()) {
						t.Fatal("response leaked the store error")
					}
				})
			}
		})
	}
}

func TestArticlePollingStillReturnsUnfinishedInstances(t *testing.T) {
	for _, status := range []string{newsarticle.StatusPending, newsarticle.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
				"alex": {{ID: "draw", UserID: "alex", Article: newsarticle.Article{ID: "article", Status: status}}},
			}}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.SetPathValue("id", "draw")
			w := httptest.NewRecorder()
			articleInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil, nil)(w, r)
			requireStatus(t, w, http.StatusOK)
			if !strings.Contains(w.Body.String(), `"status":"`+status+`"`) {
				t.Fatalf("poll response lost status %q: %s", status, w.Body.String())
			}
		})
	}
}
