package httpserver

import (
	"context"
	"errors"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/nuance"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
)

func nuanceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, nuance.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, nuance.ErrConflict):
		http.Error(w, "progress changed; reload", http.StatusConflict)
	case errors.Is(err, nuance.ErrInvalid):
		http.Error(w, "invalid practice action", http.StatusBadRequest)
	default:
		serverError(w, "nuance", err)
	}
}

func registerNuance(mux *http.ServeMux, ident identity.Identifier, st nuance.Store, pipe *pipeline.Pipeline, profile storeProfile, q *asyncjob.Queue) {
	enqueue := func(ctx context.Context, l nuance.Lesson) error {
		if err := transport.EnqueueNuance(ctx, q, pipe, st, profile, l.UserID, l.ID); err != nil {
			_ = st.SetStatus(context.WithoutCancel(ctx), l.ID, nuance.StatusFailed)
			return err
		}
		return nil
	}
	// Reopening a pending lesson repairs the DB/queue gap and the no-Redis
	// process-restart case. Queue dedupe and inline dedupe keep polling cheap.
	recoverLesson := func(ctx context.Context, l nuance.Lesson) nuance.Lesson {
		if l.Status == nuance.StatusPending || l.Status == nuance.StatusProcessing {
			if enqueue(ctx, l) != nil {
				l.Status = nuance.StatusFailed
			}
		}
		return l
	}
	mux.HandleFunc("GET /api/nuance", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		list, err := st.List(r.Context(), user)
		if err != nil {
			nuanceError(w, r, err)
			return
		}
		if list == nil {
			list = []nuance.Lesson{}
		}
		for i := range list {
			list[i] = recoverLesson(r.Context(), list[i])
		}
		writeJSON(w, list)
	})
	mux.HandleFunc("POST /api/nuance/review", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		batch, err := st.StartReview(r.Context(), user)
		if err != nil {
			nuanceError(w, r, err)
			return
		}
		if batch.Items == nil {
			batch.Items = []nuance.ReviewItem{}
		}
		if batch.Lessons == nil {
			batch.Lessons = []nuance.Lesson{}
		}
		writeJSON(w, batch)
	})
	mux.HandleFunc("POST /api/nuance/draw", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		l, err := st.Create(r.Context(), user)
		if err != nil {
			nuanceError(w, r, err)
			return
		}
		if err = enqueue(r.Context(), l); err != nil {
			l.Status = nuance.StatusFailed
		}
		writeJSON(w, l)
	})
	mux.HandleFunc("GET /api/nuance/{id}", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		l, err := st.Get(r.Context(), user, r.PathValue("id"))
		if err != nil {
			nuanceError(w, r, err)
			return
		}
		writeJSON(w, recoverLesson(r.Context(), l))
	})
	mux.HandleFunc("POST /api/nuance/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		l, err := st.Get(r.Context(), user, r.PathValue("id"))
		if err != nil {
			nuanceError(w, r, err)
			return
		}
		if l.Status != nuance.StatusDone {
			if err = st.SetStatus(r.Context(), l.ID, nuance.StatusPending); err != nil {
				nuanceError(w, r, err)
				return
			}
			l.Status = nuance.StatusPending
			if err = enqueue(r.Context(), l); err != nil {
				l.Status = nuance.StatusFailed
			}
			l, err = st.Get(r.Context(), user, l.ID)
			if err != nil {
				nuanceError(w, r, err)
				return
			}
		}
		writeJSON(w, l)
	})
	mux.HandleFunc("POST /api/nuance/{id}/practice", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var a nuance.Action
		if !decodeJSON(w, r, &a) {
			return
		}
		l, err := st.Act(r.Context(), user, r.PathValue("id"), a)
		if err != nil {
			nuanceError(w, r, err)
			return
		}
		writeJSON(w, l)
	})
	mux.HandleFunc("DELETE /api/nuance/{id}", func(w http.ResponseWriter, r *http.Request) {
		user, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if err := st.Delete(r.Context(), user, r.PathValue("id")); err != nil {
			nuanceError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
