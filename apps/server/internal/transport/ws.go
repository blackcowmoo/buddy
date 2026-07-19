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
	"buddy/server/internal/recording"
	"buddy/server/internal/session"
	"buddy/server/internal/store"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	maxAudioBytes    = 16 << 20 // 16 MiB per utterance frame
	saveInterval     = 30 * time.Second
	audioSaveTimeout = 30 * time.Second

	// pcmSampleRate matches the wire format documented in internal/protocol:
	// mono, 16 kHz, signed 16-bit little-endian PCM.
	pcmSampleRate = 16000
)

// AudioSaver persists one utterance's raw audio bytes to a temporary backing
// store, e.g. an S3-compatible bucket (see internal/audiostore.Store). A nil
// AudioSaver disables backup entirely.
type AudioSaver interface {
	SaveStream(ctx context.Context, key string, r io.Reader) error

	// Delete removes one backup, keyed the same way Handler.backupAudio wrote
	// it: userID/sessionID/id.pcm. id is the same one internal/recording.Save
	// was given for this utterance, so httpserver.recordingDeleteHandler can
	// cascade a single recording's delete to its matching backup. A no-op if
	// id has no backup.
	Delete(ctx context.Context, userID, sessionID, id string) error

	// DeleteBySession removes every backup archived under sessionID — used
	// to cascade a chat room deletion to its temporary audio backups (see
	// httpserver.sessionDeleteHandler). A no-op if userID has none.
	DeleteBySession(ctx context.Context, userID, sessionID string) error
}

// Handler upgrades HTTP to WebSocket and runs one conversation per connection.
type Handler struct {
	pipe       *pipeline.Pipeline
	ident      identity.Identifier
	store      store.Store
	audio      AudioSaver
	recordings recording.Store // nil disables recording archival (see config.Config's S3Bucket)
}

func NewHandler(p *pipeline.Pipeline, ident identity.Identifier, st store.Store, audio AudioSaver, recordings recording.Store) *Handler {
	return &Handler{pipe: p, ident: ident, store: st, audio: audio, recordings: recordings}
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
	// A missing ?session= is what triggers the opening greeting below — an
	// existing ID means the learner is resuming a room that (by definition)
	// has already been talked in, so it never gets a second greeting.
	isNewSession := sessionID == ""
	if isNewSession {
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

	if isNewSession {
		// Deliberately NOT `go`: the read loop below is what appends the
		// learner's own first turn to sess.history, so this must fully finish
		// (or be cut short by ctx cancelling on disconnect) before that loop
		// starts — otherwise a fast client could get its own first message
		// appended before the greeting, racing sess's in-memory ordering. A
		// client that sends something while this blocks doesn't lose it: WS
		// frames queue until c.Read below actually consumes them.
		h.pipe.StartConversation(ctx, sess, emit)
	}

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
			go h.pipe.HandleUtterance(tctx, sess, pcm, emit)
			// Both backups below are side-effects independent of the
			// conversation pipeline, so they use context.Background() (like
			// save() above) rather than ctx/tctx: a barge-in or the user
			// closing the tab right after speaking must not abort an upload
			// already in flight. A save failure never disrupts the live
			// conversation — just logged. These are two separate, unrelated
			// features (see internal/recording's package doc for why), but
			// they share one id for this utterance so a single recording's
			// delete can cascade to its matching temporary backup (see
			// httpserver.recordingDeleteHandler) the same way session delete
			// already cascades to every backup in bulk.
			utteranceID := uuid.New().String()
			if h.audio != nil {
				go h.backupAudio(userID, sessionID, utteranceID, pcm)
			}
			if h.recordings != nil {
				go func() {
					sctx, cancel := context.WithTimeout(context.Background(), audioSaveTimeout)
					defer cancel()
					if _, err := h.recordings.Save(sctx, userID, sessionID, utteranceID, pcm, pcmSampleRate); err != nil {
						log.Printf("recording: save %s: %v", userID, err)
					}
				}()
			}
		case websocket.MessageText:
			var m protocol.ClientMsg
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			switch m.Type {
			case "text":
				go h.pipe.HandleText(tctx, sess, m.Text, emit)
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
	if ev.Turn == 0 {
		// Turn 0 is the reserved sentinel for the opening greeting (see
		// pipeline.StartConversation) — deliberately never written to the
		// per-turn transcript, so a room the learner opens and never replies
		// to leaves no durable row behind, same as if it had never been
		// visited (matches SaveTurn's own turn==1-from-user gate on the
		// session row). It still reaches the LLM via the in-memory session
		// history, and rides along in Profile.Recent once a real turn
		// persists.
		return
	}
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
	case protocol.EvUserTranslation:
		go saveTranslation(st, userID, sessionID, ev.Turn, "user", ev.Text)
	case protocol.EvAssistantTranslation:
		go saveTranslation(st, userID, sessionID, ev.Turn, "assistant", ev.Text)
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

func saveTranslation(st store.Store, userID, sessionID string, turn int, role, translation string) {
	if err := st.SaveTranslation(context.Background(), userID, sessionID, turn, role, translation); err != nil {
		log.Printf("store: save translation %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

// backupAudio streams one utterance's raw PCM to the configured temporary
// store (see internal/audiostore.Store) as a best-effort disposable backup.
// It runs on its own context.Background() timeout rather than the turn's
// context, so a barge-in that cancels the turn doesn't truncate the upload —
// and it only logs on failure, since losing this backup must never affect
// the live conversation.
//
// The key is prefixed with sessionID (not just userID) so
// AudioSaver.DeleteBySession can find and remove every backup belonging to a
// room once its chat session is deleted — otherwise these temporary backups
// would outlive the conversation they belong to indefinitely. id is the same
// one passed to internal/recording.Store.Save for this utterance (see the
// ServeHTTP call site), so AudioSaver.Delete can also remove this one backup
// when just its matching recording — not the whole session — is deleted.
func (h *Handler) backupAudio(userID, sessionID, id string, pcm []byte) {
	key := userID + "/" + sessionID + "/" + id + ".pcm"
	ctx, cancel := context.WithTimeout(context.Background(), audioSaveTimeout)
	defer cancel()
	if err := h.audio.SaveStream(ctx, key, bytes.NewReader(pcm)); err != nil {
		log.Printf("audiostore: backup %s: %v", key, err)
	}
}
