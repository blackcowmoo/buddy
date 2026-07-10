package transport

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
	"buddy/server/internal/store"

	"github.com/coder/websocket"
)

const (
	maxAudioBytes = 16 << 20 // 16 MiB per utterance frame
	saveInterval  = 30 * time.Second
)

// Handler upgrades HTTP to WebSocket and runs one conversation per connection.
type Handler struct {
	pipe  *pipeline.Pipeline
	ident identity.Identifier
	store store.Store
}

func NewHandler(p *pipeline.Pipeline, ident identity.Identifier, st store.Store) *Handler {
	return &Handler{pipe: p, ident: ident, store: st}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Resolve (and, on first visit, set) the ID before the upgrade, since
	// Set-Cookie must go out on the HTTP response, not the WS frames. ok is
	// false when identity couldn't be established (e.g. HeaderIdentifier
	// found no auth-proxy header) — refuse rather than fall back to a
	// shared/empty key that would mix up unrelated users' memory.
	userID, ok := h.ident.Identify(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
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

	profile, err := h.store.Load(ctx, userID)
	if err != nil {
		log.Printf("store: load %s: %v", userID, err)
	}
	sess := session.New(pipeline.DefaultSystemPrompt)
	sess.Seed(profile.Summary, profile.Recent)

	save := func() {
		summary, recent := sess.Export()
		if err := h.store.Save(context.Background(), userID, store.Profile{Summary: summary, Recent: recent}); err != nil {
			log.Printf("store: save %s: %v", userID, err)
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

	emit(protocol.ServerEvent{Type: protocol.EvReady})

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
			}
		}
	}

	turnCancel()
	cancel()
	log.Printf("ws: connection closed")
	_ = c.Close(websocket.StatusNormalClosure, "bye")
}
