package httpserver

import (
	"io"
	"log"
	"net/http"
	"strconv"

	"buddy/server/internal/identity"
	"buddy/server/internal/recording"
)

// recordingsListHandler returns the caller's own archived recordings
// (internal/recording), most recent first. Scoped to userID the same way
// meHandler resolves identity — a recording can only ever be listed or
// played back by the user who made it.
func recordingsListHandler(ident identity.Identifier, recordings recording.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if recordings == nil {
			http.Error(w, "recording storage is not configured", http.StatusServiceUnavailable)
			return
		}
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		list, err := recordings.List(r.Context(), userID)
		if err != nil {
			log.Printf("recordings: list %s: %v", userID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		type item struct {
			ID         string `json:"id"`
			CreatedAt  int64  `json:"createdAt"` // unix seconds
			DurationMS int    `json:"durationMs"`
			SizeBytes  int64  `json:"sizeBytes"`
		}
		out := make([]item, len(list))
		for i, rec := range list {
			out[i] = item{ID: rec.ID, CreatedAt: rec.CreatedAt.Unix(), DurationMS: rec.DurationMS, SizeBytes: rec.SizeBytes}
		}
		writeJSON(w, out)
	}
}

// recordingAudioHandler streams one recording's stored bytes back to the
// browser. The stored S3 object is already gzip-compressed WAV (see
// internal/recording), so this is a byte-for-byte proxy: Content-Encoding:
// gzip lets the browser's own HTTP stack decompress it on the way in, so no
// server CPU is spent decoding just to serve playback.
func recordingAudioHandler(ident identity.Identifier, recordings recording.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if recordings == nil {
			http.Error(w, "recording storage is not configured", http.StatusServiceUnavailable)
			return
		}
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rec, body, err := recordings.Open(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer body.Close()

		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.FormatInt(rec.SizeBytes, 10))
		if _, err := io.Copy(w, body); err != nil {
			log.Printf("recordings: stream %s: %v", rec.ID, err)
		}
	}
}

// recordingDeleteHandler removes one archived recording. Scoped to the
// caller's own userID the same way recordingsListHandler/recordingAudioHandler
// are — a recording can only ever be deleted by the user who made it.
func recordingDeleteHandler(ident identity.Identifier, recordings recording.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if recordings == nil {
			http.Error(w, "recording storage is not configured", http.StatusServiceUnavailable)
			return
		}
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := recordings.Delete(r.Context(), userID, r.PathValue("id")); err != nil {
			log.Printf("recordings: delete %s: %v", userID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
