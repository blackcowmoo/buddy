package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
)

func TestReplyEmitsDeltaThenDoneAndAppends(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("hello")
	p := &Pipeline{LLM: &fakeLLM{chatReply: "hi there"}, ChatModel: "m"}

	events := make(chan protocol.ServerEvent, 8)
	p.reply(context.Background(), "alex", "sess-1", sess, 1, func(ev protocol.ServerEvent) { events <- ev })
	close(events)

	var got []protocol.ServerEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].Type != protocol.EvAssistantDelta || got[1].Type != protocol.EvAssistantDone {
		t.Fatalf("unexpected events: %+v", got)
	}
	if got[1].Text != "hi there" {
		t.Fatalf("assistant_done text = %q", got[1].Text)
	}
	_, recent := sess.Export()
	if len(recent) != 2 || recent[1].Role != llm.RoleAssistant || recent[1].Content != "hi there" {
		t.Fatalf("assistant reply not appended: %+v", recent)
	}
}

func TestReplyEmitsAssistantTranslation(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("hello")
	p := &Pipeline{
		LLM:       &fakeLLM{chatReply: "hi there"},
		ChatModel: "m",
		Analysis: []Candidate{{Model: "t", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "안녕하세요", nil
		}}}},
	}

	events := make(chan protocol.ServerEvent, 8)
	p.reply(context.Background(), "alex", "sess-1", sess, 1, func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	var translations []protocol.ServerEvent
	for _, ev := range got {
		if ev.Type == protocol.EvAssistantTranslation {
			translations = append(translations, ev)
		}
	}
	if len(translations) != 1 || translations[0].Text != "안녕하세요" || translations[0].Turn != 1 {
		t.Fatalf("expected one assistant_translation event, got %+v (all events: %+v)", translations, got)
	}
}

// TestReplyAssistantTranslationSurvivesBargeInAfterReplyLands guards the fix
// for a real bug: assistant-reply translation almost never showed up in live
// conversations. reply() only kicks off translateAssistant AFTER the full
// reply has streamed, so by the time that background goroutine actually runs
// its LLM call, the learner has often already sent their next message —
// which ws.go's read loop answers by cancelling the very context reply() was
// called with (barge-in). translateAssistant now runs on
// context.Background() instead of that ctx (mirroring compact()), so it must
// still emit its result even when the caller's ctx is cancelled the instant
// the reply finishes, exactly as it would be in production.
func TestReplyAssistantTranslationSurvivesBargeInAfterReplyLands(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("hello")
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pipeline{
		LLM:       &fakeLLM{chatReply: "hi there"},
		ChatModel: "m",
		Analysis: []Candidate{{Model: "t", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "안녕하세요", nil
		}}}},
	}

	events := make(chan protocol.ServerEvent, 8)
	p.reply(ctx, "alex", "sess-1", sess, 1, func(ev protocol.ServerEvent) { events <- ev })
	// Simulate the barge-in landing the instant the visible reply finishes
	// streaming, before the background translation goroutine has run — the
	// exact race ws.go's turnCancel() wins against translateAssistant today.
	cancel()

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	var translations []protocol.ServerEvent
	for _, ev := range got {
		if ev.Type == protocol.EvAssistantTranslation {
			translations = append(translations, ev)
		}
	}
	if len(translations) != 1 || translations[0].Text != "안녕하세요" {
		t.Fatalf("assistant translation should survive a barge-in landing right after the reply streamed, got %+v (all events: %+v)", translations, got)
	}
}

func TestReplyBargeInSkipsDoneAndAppend(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("hello")
	p := &Pipeline{LLM: &fakeLLM{chatReply: "should not be used"}, ChatModel: "m"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: simulates a barge-in landing before the reply lands

	var got []protocol.ServerEvent
	p.reply(ctx, "alex", "sess-1", sess, 1, func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 0 {
		t.Fatalf("expected no events emitted on barge-in, got %+v", got)
	}
	_, recent := sess.Export()
	if len(recent) != 1 {
		t.Fatalf("assistant reply must not be appended on barge-in, got %+v", recent)
	}
}

func TestReplyFallbackOnLLMError(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("what is the capital of France")
	p := &Pipeline{LLM: &fakeLLM{chatErr: errors.New("connection refused")}, ChatModel: "m"}

	var got []protocol.ServerEvent
	p.reply(context.Background(), "alex", "sess-1", sess, 1, func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 {
		t.Fatalf("expected fallback delta + done, got %+v", got)
	}
	for _, ev := range got {
		if !strings.Contains(ev.Text, "LLM offline") {
			t.Fatalf("expected fallback text mentioning an offline LLM, got %q", ev.Text)
		}
		if !strings.Contains(ev.Text, "what is the capital of France") {
			t.Fatalf("fallback should echo the user's message, got %q", ev.Text)
		}
	}
	_, recent := sess.Export()
	if len(recent) != 2 || !strings.Contains(recent[1].Content, "LLM offline") {
		t.Fatalf("fallback reply not appended: %+v", recent)
	}
}

// ---- StartConversation() --------------------------------------------------

func TestFallbackReplyEchoesLastUserMessage(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "first"},
		{Role: llm.RoleAssistant, Content: "reply"},
		{Role: llm.RoleUser, Content: "second"},
	}
	got := fallbackReply(msgs)
	if !strings.Contains(got, "second") || strings.Contains(got, "first") {
		t.Fatalf("fallbackReply should echo the LAST user message, got %q", got)
	}
}

// ---- HandleText / HandleUtterance (integration) --------------------------------
