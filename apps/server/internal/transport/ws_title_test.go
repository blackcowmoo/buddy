package transport

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/identity"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/stt"

	"github.com/coder/websocket"
)

// newTestServerWithTitleLLM is newTestServerFull, but with a completeFn so
// title-generation tests can control GenerateTitle's result — the other
// helpers all wire a bare fakeLLM{}, which always fails Complete too.
func newTestServerWithTitleLLM(t *testing.T, st store.Store, completeFn func(msgs []llm.Message) (string, error)) *httptest.Server {
	t.Helper()
	pipe := &pipeline.Pipeline{
		STT:                []stt.Recognizer{fakeSTT{text: "hello there"}},
		LLM:                fakeLLM{completeFn: completeFn},
		MaxHistoryMessages: 20,
	}
	h := NewHandler(pipe, identity.NewCookieIdentifier(), st, nil, nil, nil, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// waitForTitle polls st for sessionID's title until it stops matching want,
// or times out (backgroundWorkTimeout — see its doc for why this doesn't
// need to track titleTimeout) — title generation is fired off `go` from
// emit (see Handler.generateTitle), so tests can't observe it synchronously.
func waitForTitle(t *testing.T, st store.Store, userID, sessionID, notWant string) string {
	t.Helper()
	var title string
	ok := pollUntil(t, func() bool {
		meta, _, err := st.SessionDetail(context.Background(), userID, sessionID)
		if err == nil && meta.Title != notWant {
			title = meta.Title
			return true
		}
		return false
	})
	if !ok {
		t.Fatalf("timed out waiting for title to change from %q", notWant)
	}
	return title
}

// TestWSFirstReplyGeneratesTitle checks the trigger wired into emit: once
// the room's first exchange (turn 1) finishes, the pipeline's LLM is asked
// for a title and it's saved over the raw-text placeholder SaveTurn set on
// turn 1 (see store.MySQLStore.SaveTurn).
func TestWSFirstReplyGeneratesTitle(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServerWithTitleLLM(t, st, func(msgs []llm.Message) (string, error) {
		return "Hiking Trip Plans", nil
	})
	c, _ := dial(t, srv, "title-user", "")
	ready := readEvent(t, c)

	sendText(t, c, "I went hiking last weekend.")
	readUntilTurn(t, c, protocol.EvAssistantDone, 1)

	got := waitForTitle(t, st, "title-user", ready.Session, "I went hiking last weekend.")
	if got != "Hiking Trip Plans" {
		t.Fatalf("title = %q, want the LLM-generated title", got)
	}
}

// TestWSReconnectDoesNotRegenerateTitle guards that an ordinary reconnect
// doesn't re-roll the title: session.Session's turn counter resumes from
// store.Store.LastTurn on every connection (see session.Session.Seed), so a
// reconnect's own first send lands on turn 2, not turn 1 again — and 2 isn't
// a multiple of TitleRegenerateEveryNTurns either, so neither of the title
// trigger's two conditions (see ws.go's emit) fires. Unlike before
// store.SaveGeneratedTitle stopped gating on title_generated, there's no
// second guard behind this one: a trigger that wrongly fired here would now
// actually overwrite the title, so this test is the real safety net.
func TestWSReconnectDoesNotRegenerateTitle(t *testing.T) {
	st := newTestStore(t)
	titleN := 0
	srv := newTestServerWithTitleLLM(t, st, func(msgs []llm.Message) (string, error) {
		// pipeline.Pipeline.LLM/ChatModel is now also used for the
		// FAST-track grammar-correction/translation pre-pass (see
		// correct()/translateAssistant()), which shares this same fake and
		// would otherwise steal a "Title N" slot meant for an actual title
		// request — only count calls whose system prompt is really the
		// title prompt.
		if len(msgs) == 0 || !strings.Contains(msgs[0].Content, "descriptive title") {
			return "", nil
		}
		titleN++
		return fmt.Sprintf("Title %d", titleN), nil
	})
	cookie := "reconnect-title-user"

	c1, _ := dial(t, srv, cookie, "")
	ready1 := readEvent(t, c1)
	sendText(t, c1, "first message")
	readUntilTurn(t, c1, protocol.EvAssistantDone, 1)
	c1.Close(websocket.StatusNormalClosure, "")

	first := waitForTitle(t, st, cookie, ready1.Session, "first message")
	if first != "Title 1" {
		t.Fatalf("title after first connection = %q, want %q", first, "Title 1")
	}

	c2, _ := dial(t, srv, cookie, ready1.Session)
	readEvent(t, c2) // ready
	sendText(t, c2, "second message, different connection")
	readUntilTurn(t, c2, protocol.EvAssistantDone, 2) // resumes numbering: turn 2, not turn 1 again
	c2.Close(websocket.StatusNormalClosure, "")

	time.Sleep(200 * time.Millisecond) // give a wrongly-firing regeneration time to land
	meta, _, err := st.SessionDetail(context.Background(), cookie, ready1.Session)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Title 1" {
		t.Fatalf("title = %q after reconnect, want it to stay pinned to %q", meta.Title, "Title 1")
	}
}

// TestWSTitleRegeneratesEveryFiveTurns checks the periodic half of the title
// trigger in ws.go's emit: after turn 1's placeholder-replacing title, the
// title is left untouched through turns 2-4, then regenerated once turn 5's
// reply lands (see TitleRegenerateEveryNTurns).
func TestWSTitleRegeneratesEveryFiveTurns(t *testing.T) {
	st := newTestStore(t)
	titleN := 0
	srv := newTestServerWithTitleLLM(t, st, func(msgs []llm.Message) (string, error) {
		// Same filter as TestWSReconnectDoesNotRegenerateTitle: only count
		// calls that are really the title prompt, not the FAST-track
		// grammar-correction/translation pre-pass sharing this fake.
		if len(msgs) == 0 || !strings.Contains(msgs[0].Content, "descriptive title") {
			return "", nil
		}
		titleN++
		return fmt.Sprintf("Title %d", titleN), nil
	})
	cookie := "regen-title-user"

	c, _ := dial(t, srv, cookie, "")
	ready := readEvent(t, c)

	sendText(t, c, "message 1")
	readUntilTurn(t, c, protocol.EvAssistantDone, 1)
	first := waitForTitle(t, st, cookie, ready.Session, "message 1")
	if first != "Title 1" {
		t.Fatalf("title after turn 1 = %q, want %q", first, "Title 1")
	}

	for i, text := range []string{"message 2", "message 3", "message 4"} {
		sendText(t, c, text)
		readUntilTurn(t, c, protocol.EvAssistantDone, i+2)
	}
	time.Sleep(200 * time.Millisecond) // give a wrongly-firing regeneration time to land
	meta, _, err := st.SessionDetail(context.Background(), cookie, ready.Session)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Title 1" {
		t.Fatalf("title = %q after turns 2-4, want it unchanged at %q", meta.Title, "Title 1")
	}

	sendText(t, c, "message 5")
	readUntilTurn(t, c, protocol.EvAssistantDone, 5)

	second := waitForTitle(t, st, cookie, ready.Session, "Title 1")
	if second != "Title 2" {
		t.Fatalf("title after turn 5 = %q, want %q", second, "Title 2")
	}
}
