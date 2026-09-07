package transport

import (
	"context"
	"testing"

	"buddy/server/internal/pipeline"
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

	persistEvent(nil, st, nil, nil, "alex", "sess-1", protocol.ServerEvent{Type: protocol.EvUserTranslation, Turn: 1, Text: "그는 학교에 간다"})
	persistEvent(nil, st, nil, nil, "alex", "sess-1", protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: 1, Text: "좋아요!"})

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

func TestPersistEventFinalBeforePreviewKeepsExplicitUnreadDecision(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-race", 1, "user", "I like pizza.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	correction := protocol.Correction{Original: "I like pizza.", Corrected: "I like pizza."}

	// Deliberately persist Judge before Chat. This is the opposite of the
	// event order, but is legal because persistEvent detaches both writes.
	persistEvent(nil, st, nil, nil, "alex", "sess-race", protocol.ServerEvent{
		Type: protocol.EvCorrection, Turn: 1, Final: true, Changed: false, Correction: &correction,
	})
	pollUntil(t, func() bool {
		_, turns, err := st.SessionDetail(ctx, "alex", "sess-race")
		return err == nil && len(turns) == 1 && turns[0].CorrectionStage == "judge"
	})
	// Invoke the same persistence helper synchronously so the assertion cannot
	// accidentally run before persistEvent's late-preview goroutine starts.
	saveCorrectionPreview(st, "alex", "sess-race", 1, correction)

	_, turns, err := st.SessionDetail(ctx, "alex", "sess-race")
	if err != nil || len(turns) != 1 {
		t.Fatalf("SessionDetail() turns=%+v err=%v", turns, err)
	}
	if turns[0].CorrectionStage != "judge" || turns[0].CorrectionUnread {
		t.Fatalf("late Chat preview changed terminal state: %+v", turns[0])
	}
}

// TestPersistEventCapturesVocabularyWordFromCorrection guards the live
// (no-durable-queue) path's wiring of captureCorrectionWords: in a
// deployment with no Redis-backed correction queue, persistEvent's
// EvCorrection case is the *only* place a correction (and any word captured
// from it) ever gets persisted, so it must not be skipped here.
func TestPersistEventCapturesVocabularyWordFromCorrection(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-1", 1, "user", "I was very angry", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"valid":true,"reason":""}`}}}}
	words := newFakeWordReviewStore()

	persistEvent(pipe, st, words, nil, "alex", "sess-1", protocol.ServerEvent{
		Type:  protocol.EvCorrection,
		Turn:  1,
		Final: true,
		Correction: &protocol.Correction{
			Original:  "I was very angry",
			Corrected: "I was furious.",
			Issues:    []protocol.Issue{{Type: "vocabulary", Span: "very angry", Suggestion: "furious", ExplanationTranslation: "몹시 화난"}},
		},
	})

	pollUntil(t, func() bool {
		list, err := words.List(ctx, "alex")
		return err == nil && len(list) == 1
	})
	list, err := words.List(ctx, "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Word != "furious" {
		t.Fatalf("captured words = %+v, want exactly one 'furious'", list)
	}
}
