package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
	"buddy/server/internal/store"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	maxAudioBytes    = 16 << 20 // 16 MiB per utterance frame
	saveInterval     = 30 * time.Second
	audioSaveTimeout = 30 * time.Second
)

// AudioSaver persists one utterance's raw audio bytes to a temporary backing
// store, e.g. an S3-compatible bucket (see internal/audiostore.Store). A nil
// AudioSaver disables backup entirely.
type AudioSaver interface {
	SaveStream(ctx context.Context, key string, r io.Reader) error
}

// Handler upgrades HTTP to WebSocket and runs one conversation per connection.
type Handler struct {
	pipe  *pipeline.Pipeline
	ident identity.Identifier
	store store.Store
	audio AudioSaver
}

func NewHandler(p *pipeline.Pipeline, ident identity.Identifier, st store.Store, audio AudioSaver) *Handler {
	return &Handler{pipe: p, ident: ident, store: st, audio: audio}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Resolve (and, on first visit, set) the ID before the upgrade, since
	// Set-Cookie must go out on the HTTP response, not the WS frames. ok is
	// false when identity couldn't be established (e.g. OIDCIdentifier found
	// no valid Dex JWT) — refuse rather than fall back to a shared/empty key
	// that would mix up unrelated users' memory.
	userID, ok := h.ident.Identify(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The frontend only ever supplies ?session=<id> when the learner picked
	// an existing chat room from the list (or is resuming one); omitting it
	// always starts a brand-new room, on purpose — the home screen shows the
	// room list rather than silently reconnecting to whatever was last open.
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		// Uniqueness (not unguessability of someone else's) is all that's
		// required here: every store lookup is scoped by (userID, sessionID)
		// together, so a collision or a guessed ID from another user still
		// can't reach that user's data (see the composite primary keys in
		// internal/store/mysql.go).
		sessionID = identity.NewOpaqueID()
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Single entry point means same-origin, but be permissive in study/dev.
		// Tighten OriginPatterns before exposing this publicly.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	c.SetReadLimit(maxAudioBytes)
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	profile, err := h.store.Load(ctx, userID, sessionID)
	if err != nil {
		log.Printf("store: load %s/%s: %v", userID, sessionID, err)
	}
	sess := session.New(pipeline.DefaultSystemPrompt)
	sess.Seed(profile.Summary, profile.Recent)

	save := func() {
		summary, recent := sess.Export()
		if err := h.store.Save(context.Background(), userID, sessionID, store.Profile{Summary: summary, Recent: recent}); err != nil {
			log.Printf("store: save %s/%s: %v", userID, sessionID, err)
		}
	}
	defer save() // final save on disconnect

	ticker := time.NewTicker(saveInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				save()
			}
		}
	}()

	// One writer goroutine owns the socket for writes; emit() is the only way
	// events reach it, so pipeline goroutines can emit concurrently.
	events := make(chan protocol.ServerEvent, 128)
	emit := func(ev protocol.ServerEvent) {
		persistEvent(h.store, userID, sessionID, ev)
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-events:
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if err := c.Write(ctx, websocket.MessageText, b); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	emit(protocol.ServerEvent{Type: protocol.EvReady, Session: sessionID})

	// turnCancel implements barge-in: a new input cancels the previous turn.
	turnCancel := func() {}
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		turnCancel()
		var tctx context.Context
		tctx, turnCancel = context.WithCancel(ctx)

		switch typ {
		case websocket.MessageBinary:
			pcm := append([]byte(nil), data...) // copy: Read may reuse the buffer
			if h.audio != nil {
				go h.backupAudio(userID, pcm)
			}
			go h.pipe.HandleUtterance(tctx, sess, pcm, emit)
		case websocket.MessageText:
			var m protocol.ClientMsg
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			switch m.Type {
			case "text":
				go h.pipe.HandleText(tctx, sess, m.Text, emit)
			case "reset":
				sess.Reset(pipeline.DefaultSystemPrompt)
				// Turn numbering restarts at 1 after Reset, which would
				// collide with (and silently overwrite, via SaveTurn's
				// upsert) this room's original turn 1 if its old transcript
				// were left in place. Clearing it keeps "reset" meaning the
				// same thing for the persisted history as it does on
				// screen: this room's messages are gone, its long-term
				// summary and title are not.
				go func() {
					if err := h.store.DeleteTurns(context.Background(), userID, sessionID); err != nil {
						log.Printf("store: delete turns %s/%s: %v", userID, sessionID, err)
					}
				}()
			}
		}
	}

	turnCancel()
	cancel()
	log.Printf("ws: connection closed")
	_ = c.Close(websocket.StatusNormalClosure, "bye")
}

// persistEvent writes a copy of ev's payload to durable per-session
// transcript storage. It's deliberately narrow and off the hot path:
// EvAssistantDelta fires many times per turn as tokens stream, so only
// events that represent a finished piece of state trigger a write, and each
// write runs in its own goroutine so a slow database never adds latency to
// the live conversation the learner is watching. Writes use
// context.Background(), not the connection's context, so a turn's result
// still lands even if the client disconnects or barges in right as it
// completes (same reasoning as pipeline.compact's use of Background).
func persistEvent(st store.Store, userID, sessionID string, ev protocol.ServerEvent) {
	switch ev.Type {
	case protocol.EvFinal:
		go saveTurn(st, userID, sessionID, ev.Turn, "user", ev.Text, false)
	case protocol.EvRefined:
		go saveTurn(st, userID, sessionID, ev.Turn, "user", ev.Text, true)
	case protocol.EvAssistantDone:
		go saveTurn(st, userID, sessionID, ev.Turn, "assistant", ev.Text, false)
	case protocol.EvCorrection:
		if ev.Correction == nil {
			return
		}
		go saveCorrection(st, userID, sessionID, ev.Turn, *ev.Correction)
	}
}

func saveTurn(st store.Store, userID, sessionID string, turn int, role, text string, refined bool) {
	if err := st.SaveTurn(context.Background(), userID, sessionID, turn, role, text, refined); err != nil {
		log.Printf("store: save turn %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

func saveCorrection(st store.Store, userID, sessionID string, turn int, c protocol.Correction) {
	if err := st.SaveCorrection(context.Background(), userID, sessionID, turn, c); err != nil {
		log.Printf("store: save correction %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

// backupAudio streams one utterance's raw PCM to the configured temporary
// store (see internal/audiostore.Store) as a best-effort disposable backup.
// It runs on its own context.Background() timeout rather than the turn's
// context, so a barge-in that cancels the turn doesn't truncate the upload —
// and it only logs on failure, since losing this backup must never affect
// the live conversation.
func (h *Handler) backupAudio(userID string, pcm []byte) {
	key := userID + "/" + uuid.New().String() + ".pcm"
	ctx, cancel := context.WithTimeout(context.Background(), audioSaveTimeout)
	defer cancel()
	if err := h.audio.SaveStream(ctx, key, bytes.NewReader(pcm)); err != nil {
		log.Printf("audiostore: backup %s: %v", key, err)
	}
}
