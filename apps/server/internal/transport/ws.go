package transport

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/recording"
	"buddy/server/internal/session"
	"buddy/server/internal/store"
	"buddy/server/internal/wordreview"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	maxAudioBytes = 16 << 20 // 16 MiB per utterance frame
	saveInterval  = 30 * time.Second
	// Audio persistence is deliberately detached from the WebSocket and may
	// target a slow local or remote object store. Keep the same long-running
	// policy as local LLM/STT inference so a temporary timeout cannot trigger
	// duplicate background uploads.
	audioSaveTimeout = 24 * time.Hour

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
	pipe            *pipeline.Pipeline
	ident           identity.Identifier
	store           store.Store
	audio           AudioSaver
	recordings      recording.Store // nil disables recording archival (see config.Config's S3Bucket)
	words           wordreview.Store
	wordVerifyQueue *asyncjob.Queue
}

func NewHandler(p *pipeline.Pipeline, ident identity.Identifier, st store.Store, audio AudioSaver, recordings recording.Store, words wordreview.Store, wordVerifyQueue *asyncjob.Queue) *Handler {
	return &Handler{pipe: p, ident: ident, store: st, audio: audio, recordings: recordings, words: words, wordVerifyQueue: wordVerifyQueue}
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

	// Load, LastTurn, GetInterlocutorStyle, and GetLearnerProfile are four
	// independent reads — fan them out concurrently (same reasoning as
	// store.MySQLStore.SessionDetail) rather than paying four sequential
	// round trips to what may be a network-hop-away replica before the
	// handshake can complete.
	var profile store.Profile
	var lastTurn int
	var style string
	var learnerProfile string
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		var err error
		if profile, err = h.store.Load(ctx, userID, sessionID); err != nil {
			log.Printf("store: load %s/%s: %v", userID, sessionID, err)
		}
	}()
	go func() {
		defer wg.Done()
		// A reconnect to an existing session must not restart turn
		// numbering at 0 — that would collide with, and silently
		// overwrite, turns the earlier connection already saved (see
		// store.MySQLStore.SaveTurn's ON DUPLICATE KEY UPDATE). LastTurn
		// resumes numbering from the persisted transcript.
		var err error
		if lastTurn, err = h.store.LastTurn(ctx, userID, sessionID); err != nil {
			log.Printf("store: last turn %s/%s: %v", userID, sessionID, err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if style, err = h.store.GetInterlocutorStyle(ctx, userID); err != nil {
			log.Printf("store: get interlocutor style %s: %v", userID, err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if learnerProfile, err = h.store.GetLearnerProfile(ctx, userID); err != nil {
			log.Printf("store: get learner profile %s: %v", userID, err)
		}
	}()
	wg.Wait()
	sess := session.New(pipeline.BuildSystemPrompt(style, learnerProfile))
	sess.Seed(profile.Summary, profile.Recent, lastTurn)

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
		persistEvent(h.pipe, h.store, h.words, h.wordVerifyQueue, userID, sessionID, ev)
		if ev.Type == protocol.EvAssistantDone && shouldGenerateTitle(ev.Turn) {
			go h.generateTitle(userID, sessionID, sess, ev.Turn, ev.Text)
		}
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
		h.pipe.StartConversation(ctx, userID, sessionID, sess, emit)
	}

	// turnCancel implements barge-in: a new input cancels the previous turn.
	turnCancel := func() {}
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}

		// A room can be finalized out from under a still-open connection —
		// e.g. maybeFinalizeInstantSession, triggered server-side by a
		// background correction job rather than anything on this socket —
		// so re-check on every message rather than trusting whatever was
		// true at connect time. Skipping here, before HandleText/
		// HandleUtterance ever run, is what actually saves the LLM call;
		// the client finding out the room is read-only is a separate,
		// already-handled concern (EndConversationControl).
		if ended, err := h.store.SessionEnded(ctx, userID, sessionID); err != nil {
			log.Printf("store: session ended %s/%s: %v", userID, sessionID, err)
		} else if ended {
			continue
		}

		turnCancel()
		var tctx context.Context
		tctx, turnCancel = context.WithCancel(ctx)

		switch typ {
		case websocket.MessageBinary:
			pcm := append([]byte(nil), data...) // copy: Read may reuse the buffer
			go h.pipe.HandleUtterance(tctx, userID, sessionID, sess, pcm, emit)
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
				go h.pipe.HandleText(tctx, userID, sessionID, sess, m.Text, m.Source, emit)
			}
		}
	}

	turnCancel()
	cancel()
	log.Printf("ws: connection closed")
	_ = c.Close(websocket.StatusNormalClosure, "bye")
}
