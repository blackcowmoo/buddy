package transport

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"

	"github.com/coder/websocket"
)

const maxAudioBytes = 16 << 20 // 16 MiB per utterance frame

// Handler upgrades HTTP to WebSocket and runs one conversation per connection.
type Handler struct {
	pipe *pipeline.Pipeline
}

func NewHandler(p *pipeline.Pipeline) *Handler { return &Handler{pipe: p} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	sess := session.New(pipeline.DefaultSystemPrompt)

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
