package httpserver

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"buddy/server/internal/identity"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
	"buddy/server/internal/ttsstore"
)

// messageAudioHandler serves one chat turn's read-aloud audio — the
// learner's own line or the assistant's reply (see StudyControl.tsx, which
// offers 🔊 on both, e.g. so a learner can hear their own sentence spoken
// back for pronunciation practice). Unlike articleAudioHandler, this is
// generated on demand (the first time a learner actually asks to hear this
// specific message, not eagerly for every reply — see
// transport.MessageAudioKey's doc comment) and streamed to the client as
// it's produced rather than buffered first, since the learner is waiting
// live and a reply can be long enough that "wait for the whole thing"
// would be a noticeable delay (kokoro-js's own internal sentence-boundary
// chunking is what actually makes this arrive progressively — see
// internal/tts.Kokoro.Stream). The stream is simultaneously teed into an
// in-memory buffer and cached in S3 once fully received, so replaying the
// same message is instant and never regenerates. A cache hit skips
// generation entirely, served the same buffered way as
// articleAudioHandler.
func messageAudioHandler(ident identity.Identifier, st store.Store, audio *transport.ArticleAudio) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if audio == nil {
			http.Error(w, "read-aloud is not configured", http.StatusServiceUnavailable)
			return
		}
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		turn, err := strconv.Atoi(r.PathValue("turn"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		role := r.URL.Query().Get("role")

		_, turns, err := st.SessionDetail(r.Context(), userID, sessionID)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			serverError(w, "messages: audio session "+userID, err)
			return
		}
		text := ""
		found := false
		for _, t := range turns {
			if t.Turn == turn && t.Role == role {
				text = t.Text
				found = true
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}

		key := transport.MessageAudioKey(sessionID, turn, role)
		body, err := audio.Cache.Open(r.Context(), key)
		if err == nil {
			defer body.Close()
			w.Header().Set("Content-Type", "audio/mpeg")
			if _, err := io.Copy(w, body); err != nil {
				log.Printf("messages: audio stream cached %s: %v", key, err)
			}
			return
		}
		if !errors.Is(err, ttsstore.ErrNotFound) {
			serverError(w, "messages: audio open "+key, err)
			return
		}

		stream, err := audio.Client.Stream(r.Context(), text)
		if err != nil {
			serverError(w, "messages: audio generate "+key, err)
			return
		}
		defer stream.Close()

		var buf bytes.Buffer
		w.Header().Set("Content-Type", "audio/mpeg")
		if _, err := io.Copy(w, io.TeeReader(stream, &buf)); err != nil {
			// A truncated/failed stream (client disconnect, upstream error
			// mid-generation) must never be cached as if it were the
			// complete audio — the next request would just serve the same
			// truncated bytes forever instead of retrying generation.
			log.Printf("messages: audio stream %s: %v", key, err)
			return
		}
		if err := audio.Cache.Put(r.Context(), key, buf.Bytes()); err != nil {
			log.Printf("messages: audio cache %s: %v", key, err)
		}
	}
}
