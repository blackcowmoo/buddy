package transport

import (
	"context"
	"testing"

	"buddy/server/internal/protocol"
	"buddy/server/internal/store"

	"github.com/coder/websocket"
)

func TestWSFinalAndAssistantTurnsArePersisted(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	c, _ := dial(t, srv, "turn-user", "")
	ready := readEvent(t, c)

	sendText(t, c, "Hello Buddy")
	readUntilTurn(t, c, protocol.EvAssistantDone, 1)

	var turns []store.Turn
	pollUntil(t, func() bool {
		_, ts, err := st.SessionDetail(context.Background(), "turn-user", ready.Session)
		if err == nil && len(ts) == 3 {
			turns = ts
			return true
		}
		return false
	})
	if len(turns) != 3 {
		t.Fatalf("expected 3 persisted turns (greeting+user+assistant), got %+v", turns)
	}
	if turns[0].Turn != 0 || turns[0].Role != "assistant" || turns[0].Text == "" || turns[0].Source != "" {
		t.Fatalf("turn[0] = %+v, want a non-empty opening greeting with no source", turns[0])
	}
	if turns[1].Role != "user" || turns[1].Text != "Hello Buddy" || turns[1].Source != protocol.SourceText {
		t.Fatalf("turn[1] = %+v, want user/\"Hello Buddy\"/source=text", turns[1])
	}
	if turns[2].Role != "assistant" || turns[2].Text == "" || turns[2].Source != "" {
		t.Fatalf("turn[2] = %+v, want a non-empty assistant reply with no source", turns[2])
	}
}

// TestWSBinaryFramePersistsVoiceSource verifies a spoken (binary-frame)
// utterance is persisted with Source == protocol.SourceVoice, distinguishing
// it from TestWSFinalAndAssistantTurnsArePersisted's typed path — this is
// what lets the frontend show which input method produced each message.
// Looks up the user turn by role rather than assuming index 0, since this is
// also a brand-new session: turn 0 is the opening greeting (see
// TestWSFinalAndAssistantTurnsArePersisted), and the voice turn lands at
// turn 1 alongside it.
func TestWSBinaryFramePersistsVoiceSource(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st)
	c, _ := dial(t, srv, "voice-user", "")
	ready := readEvent(t, c)

	if err := c.Write(context.Background(), websocket.MessageBinary, []byte("fake pcm bytes")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	draft := readUntil(t, c, protocol.EvPendingTranscript)
	sendVoiceConfirm(t, c, draft.Text) // learner reviews the draft and sends it

	var userTurn store.Turn
	found := pollUntil(t, func() bool {
		_, ts, err := st.SessionDetail(context.Background(), "voice-user", ready.Session)
		if err != nil {
			return false
		}
		for _, turn := range ts {
			if turn.Role == "user" {
				userTurn = turn
				return true
			}
		}
		return false
	})
	if !found {
		t.Fatalf("no user turn persisted in time")
	}
	if userTurn.Source != protocol.SourceVoice {
		t.Fatalf("user turn = %+v, want source=voice", userTurn)
	}
}

// TestPersistEventSavesTranslationsByRole checks the persistEvent cases
// added for EvUserTranslation/EvAssistantTranslation: each must reach
// SaveTranslation with the role that disambiguates it from the turn's other
// role, since persistEvent runs its saves off the hot path in goroutines
// where a mixed-up role would silently overwrite the wrong row.
func TestPersistEventSavesTranslationsByRole(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-1", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", "sess-1", 1, "assistant", "Nice!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}

	persistEvent(st, "alex", "sess-1", protocol.ServerEvent{Type: protocol.EvUserTranslation, Turn: 1, Text: "그는 학교에 간다"})
	persistEvent(st, "alex", "sess-1", protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: 1, Text: "좋아요!"})

	var turns []store.Turn
	pollUntil(t, func() bool {
		_, ts, err := st.SessionDetail(ctx, "alex", "sess-1")
		if err == nil && len(ts) == 2 && ts[0].Translation != "" && ts[1].Translation != "" {
			turns = ts
			return true
		}
		return false
	})
	if len(turns) != 2 {
		t.Fatalf("translations did not persist in time: %+v", turns)
	}
	if turns[0].Role != "user" || turns[0].Translation != "그는 학교에 간다" {
		t.Fatalf("user turn = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Translation != "좋아요!" {
		t.Fatalf("assistant turn = %+v", turns[1])
	}
}
