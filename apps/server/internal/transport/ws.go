package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/llm"
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

	// titleTimeout bounds Handler.generateTitle's LLM call — its own budget,
	// not the connection's ctx, since a barge-in or disconnect right after
	// the first reply must not cut short the one-shot title generation for
	// that room (mirrors audioSaveTimeout below).
	titleTimeout = 15 * time.Second
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
	titleQueue *asyncjob.Queue // nil disables durable title generation — see SetTitleQueue
}

func NewHandler(p *pipeline.Pipeline, ident identity.Identifier, st store.Store, audio AudioSaver, recordings recording.Store) *Handler {
	return &Handler{pipe: p, ident: ident, store: st, audio: audio, recordings: recordings}
}

// SetTitleQueue wires durable, queue-backed title generation (see
// TitleJobHandler) — a separate setter, not a NewHandler parameter, so
// every existing call site (production and tests) keeps working unchanged
// when title generation stays on its original direct-call path (queue nil,
// i.e. Redis unconfigured — see cmd/server/main.go).
func (h *Handler) SetTitleQueue(q *asyncjob.Queue) {
	h.titleQueue = q
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
	// A reconnect to an existing session must not restart turn numbering at
	// 0 — that would collide with, and silently overwrite, turns the earlier
	// connection already saved (see store.MySQLStore.SaveTurn's ON DUPLICATE
	// KEY UPDATE). LastTurn resumes numbering from the persisted transcript.
	lastTurn, err := h.store.LastTurn(ctx, userID, sessionID)
	if err != nil {
		log.Printf("store: last turn %s/%s: %v", userID, sessionID, err)
	}
	style, err := h.store.GetInterlocutorStyle(ctx, userID)
	if err != nil {
		log.Printf("store: get interlocutor style %s: %v", userID, err)
	}
	sess := session.New(pipeline.BuildSystemPrompt(style))
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
		persistEvent(h.store, userID, sessionID, ev)
		// Turn 1's assistant reply is the first full exchange this room has
		// — enough context to title it. Fired here (not off persistEvent,
		// which has no pipeline access) so it never delays the reply the
		// learner is watching; see Handler.generateTitle for why a
		// reconnect firing this again is still safe.
		if ev.Type == protocol.EvAssistantDone && ev.Turn == 1 {
			go h.generateTitle(userID, sessionID, sess, ev.Text)
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
				go h.pipe.HandleText(tctx, userID, sessionID, sess, m.Text, emit)
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
//
// Turn 0 (the opening greeting, see pipeline.StartConversation) is written
// here like any other turn: store.MySQLStore.SaveTurn inserts into
// buddy_turns unconditionally and also creates the *session row* right away
// (title placeholder'd from the greeting text), so a greeting-only room is
// already visible to ListSessions/SessionDetail — the learner can find it
// and delete it even if they never reply. If the learner's turn 1 does land,
// SaveTurn replaces that placeholder title with their own first message.
func persistEvent(st store.Store, userID, sessionID string, ev protocol.ServerEvent) {
	switch ev.Type {
	case protocol.EvFinal:
		go saveTurn(st, userID, sessionID, ev.Turn, "user", ev.Text, false, ev.Source)
	case protocol.EvRefined:
		go saveTurn(st, userID, sessionID, ev.Turn, "user", ev.Text, true, ev.Source)
	case protocol.EvAssistantDone:
		go saveTurn(st, userID, sessionID, ev.Turn, "assistant", ev.Text, false, "")
	case protocol.EvCorrection:
		if ev.Failed {
			// Only durable when a job was actually reserved for this turn
			// (see store.ReserveCorrectionJob) — a plain UPDATE with no
			// matching row is a harmless no-op, same as saveCorrection below
			// when the queue-backed path isn't configured.
			go failCorrectionJob(st, userID, sessionID, ev.Turn)
			return
		}
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

func saveTurn(st store.Store, userID, sessionID string, turn int, role, text string, refined bool, source string) {
	if err := st.SaveTurn(context.Background(), userID, sessionID, turn, role, text, refined, source); err != nil {
		log.Printf("store: save turn %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

func saveCorrection(st store.Store, userID, sessionID string, turn int, c protocol.Correction) {
	if err := st.SaveCorrection(context.Background(), userID, sessionID, turn, c); err != nil {
		log.Printf("store: save correction %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

// failCorrectionJob mirrors saveCorrection for the failure path — a second,
// less-detailed FailJob write on top of CorrectionJobHandler's own (see
// analysis_jobs.go) when the queue-backed path is configured, and the only
// one at all when it isn't (a harmless no-op there, same as saveCorrection
// above with no reservation to match).
func failCorrectionJob(st store.Store, userID, sessionID string, turn int) {
	if err := st.FailJob(context.Background(), userID, sessionID, turn, "correction", "analysis failed"); err != nil {
		log.Printf("store: fail correction job %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

func saveTranslation(st store.Store, userID, sessionID string, turn int, role, translation string) {
	if err := st.SaveTranslation(context.Background(), userID, sessionID, turn, role, translation); err != nil {
		log.Printf("store: save translation %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

// generateTitle asks the pipeline's LLM for a proper chat-room title from
// the room's first exchange, replacing the raw-text-truncation placeholder
// store.MySQLStore.SaveTurn sets on turn 1. Runs entirely off the live
// turn: emit fires this via `go` so the learner's reply is never delayed,
// and it uses context.Background() (bounded by titleTimeout, not the
// connection's ctx) so a disconnect right after the first reply doesn't cut
// it short — same reasoning as backupAudio below.
//
// ev.Turn == 1 normally happens once per session's lifetime — session.New's
// turn counter is seeded from store.Store.LastTurn on every connect (see
// session.Session.Seed), so a reconnect resumes numbering rather than
// restarting at 0. It can still recur (e.g. a race between two connections
// both seeing the same LastTurn before either has saved turn 1), so this
// isn't relied on for correctness: store.SaveGeneratedTitle only ever applies
// the first successful write for a given session, making a repeat call a
// harmless no-op rather than a flapping title.
func (h *Handler) generateTitle(userID, sessionID string, sess *session.Session, assistantText string) {
	_, recent := sess.Export()
	var userText string
	for _, m := range recent {
		if m.Role == llm.RoleUser {
			userText = m.Content
			break
		}
	}
	if userText == "" {
		return
	}
	if h.titleQueue == nil {
		h.generateTitleDirect(userID, sessionID, userText, assistantText)
		return
	}
	// Durable path: queued in Redis and persisted independent of this
	// connection or replica — see TitleJobHandler. No poll-fallback is
	// needed here (unlike reply generation): a title landing after the
	// fact has no live-connection UX to serve, it just shows up next time
	// the room list is fetched.
	payload := titleJobPayload{UserID: userID, SessionID: sessionID, UserText: userText, AssistantText: assistantText}
	job, ok, err := h.titleQueue.Enqueue(context.Background(), asyncjob.KindTitle, titleDedupeKey(userID, sessionID), payload)
	if err != nil {
		log.Printf("title: enqueue %s/%s: %v", userID, sessionID, err)
		return
	}
	if !ok {
		return // already queued/in flight
	}
	claimed, err := h.titleQueue.TryClaimByID(context.Background(), job, TitleClaimTTL)
	if err != nil {
		log.Printf("title: inline claim %s/%s: %v", userID, sessionID, err)
		return
	}
	if !claimed {
		return // a pooled Worker already has it
	}
	if err := h.titleQueue.Execute(context.Background(), job, TitleJobHandler(h.pipe, h.store)); err != nil {
		log.Printf("title: inline execute %s/%s: %v", userID, sessionID, err)
	}
}

// generateTitleDirect is generateTitle's original direct-call behavior,
// used when titleQueue is nil (Redis unconfigured) — same "optional
// feature, zero setup by default" convention as every other Redis-backed
// feature in this codebase.
func (h *Handler) generateTitleDirect(userID, sessionID, userText, assistantText string) {
	ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
	defer cancel()
	title, err := h.pipe.GenerateTitle(ctx, userText, assistantText)
	if err != nil {
		log.Printf("title: generate %s/%s: %v", userID, sessionID, err)
		return
	}
	if title = strings.TrimSpace(title); title == "" {
		return
	}
	if err := h.store.SaveGeneratedTitle(context.Background(), userID, sessionID, title); err != nil {
		log.Printf("title: save %s/%s: %v", userID, sessionID, err)
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
