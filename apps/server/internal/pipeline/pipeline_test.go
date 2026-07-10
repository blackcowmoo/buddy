package pipeline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
	"buddy/server/internal/stt"
)

// ---- test doubles -----------------------------------------------------------

type fakeSTT struct {
	text string
	err  error
}

func (f fakeSTT) Name() string { return "fake" }
func (f fakeSTT) Transcribe(ctx context.Context, pcm []byte) (stt.Result, error) {
	if f.err != nil {
		return stt.Result{}, f.err
	}
	return stt.Result{Text: f.text, Confidence: 1}, nil
}

// fakeLLM is a deterministic llm.Client double. ChatStream checks ctx.Err()
// first, mirroring a real HTTP client failing immediately on an
// already-cancelled context — this is what makes the barge-in test exact.
type fakeLLM struct {
	mu sync.Mutex

	chatReply string
	chatErr   error
	complete  func(msgs []llm.Message) (string, error)
}

func (f *fakeLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatErr != nil {
		return "", f.chatErr
	}
	if onToken != nil {
		onToken(f.chatReply)
	}
	return f.chatReply, nil
}

func (f *fakeLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.complete != nil {
		return f.complete(msgs)
	}
	return "", nil
}

// collectUntilQuiet drains emitted events until none arrive for `quiet`, or
// `timeout` elapses overall. Needed for tests that exercise the background
// refine/correct/compact goroutines, whose interleaving with the synchronous
// reply is intentionally not guaranteed.
func collectUntilQuiet(t *testing.T, events <-chan protocol.ServerEvent, quiet, timeout time.Duration) []protocol.ServerEvent {
	t.Helper()
	var got []protocol.ServerEvent
	overall := time.After(timeout)
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
		case <-time.After(quiet):
			return got
		case <-overall:
			t.Fatalf("collectUntilQuiet: timed out; got so far: %+v", got)
			return got
		}
	}
}

// ---- reply() -----------------------------------------------------------------

func TestReplyEmitsDeltaThenDoneAndAppends(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("hello")
	p := &Pipeline{LLM: &fakeLLM{chatReply: "hi there"}, ChatModel: "m"}

	events := make(chan protocol.ServerEvent, 8)
	p.reply(context.Background(), sess, 1, func(ev protocol.ServerEvent) { events <- ev })
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

func TestReplyBargeInSkipsDoneAndAppend(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("hello")
	p := &Pipeline{LLM: &fakeLLM{chatReply: "should not be used"}, ChatModel: "m"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: simulates a barge-in landing before the reply lands

	var got []protocol.ServerEvent
	p.reply(ctx, sess, 1, func(ev protocol.ServerEvent) { got = append(got, ev) })

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
	p.reply(context.Background(), sess, 1, func(ev protocol.ServerEvent) { got = append(got, ev) })

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

// ---- compact() -----------------------------------------------------------------

func TestCompactNoopBelowThreshold(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("u1")
	sess.AppendAssistant("a1")
	calls := 0
	p := &Pipeline{
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls++
			return "should not be called", nil
		}},
		MaxHistoryMessages: 20,
	}

	p.compact(sess)

	if calls != 0 {
		t.Fatalf("expected no LLM call below threshold, got %d calls", calls)
	}
	summary, recent := sess.Export()
	if summary != "" || len(recent) != 2 {
		t.Fatalf("session mutated unexpectedly: summary=%q recent=%+v", summary, recent)
	}
}

func TestCompactFoldsOldestAtThreshold(t *testing.T) {
	sess := session.New("sys")
	for i := 0; i < 6; i++ { // 12 messages
		sess.AppendUser(fmt.Sprintf("u%d", i))
		sess.AppendAssistant(fmt.Sprintf("a%d", i))
	}
	p := &Pipeline{
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "rolled-up summary", nil
		}},
		MaxHistoryMessages: 4,
	}

	p.compact(sess)

	summary, recent := sess.Export()
	if summary != "rolled-up summary" {
		t.Fatalf("summary = %q", summary)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 remaining messages, got %d: %+v", len(recent), recent)
	}
}

func TestCompactLeavesHistoryOnLLMError(t *testing.T) {
	sess := session.New("sys")
	for i := 0; i < 6; i++ {
		sess.AppendUser("u")
		sess.AppendAssistant("a")
	}
	_, before := sess.Export()

	p := &Pipeline{
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("llm down")
		}},
		MaxHistoryMessages: 4,
	}
	p.compact(sess)

	summary, after := sess.Export()
	if summary != "" {
		t.Fatalf("summary should stay empty on LLM error, got %q", summary)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("history should be untouched on LLM error:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// ---- correct() -----------------------------------------------------------------

func TestCorrectEmitsEventWhenChanged(t *testing.T) {
	p := &Pipeline{
		CorrectModel: "m",
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I like pizza.","issues":[{"type":"grammar","span":"I likes","suggestion":"I like","explanation":"수 일치 오류"}]}`, nil
		}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "I likes pizza", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Type != protocol.EvCorrection {
		t.Fatalf("expected one correction event, got %+v", got)
	}
	if got[0].Correction.Corrected != "I like pizza." {
		t.Fatalf("Correction.Corrected = %q", got[0].Correction.Corrected)
	}
	if got[0].Correction.Issues[0].Explanation != "수 일치 오류" {
		t.Fatalf("explanation not passed through: %+v", got[0].Correction.Issues[0])
	}
}

func TestCorrectSkipsWhenAlreadyCorrect(t *testing.T) {
	p := &Pipeline{
		CorrectModel: "m",
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I like pizza.","issues":[]}`, nil
		}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "I like pizza.", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 0 {
		t.Fatalf("expected no event for an already-correct sentence, got %+v", got)
	}
}

func TestCorrectIgnoresMalformedJSON(t *testing.T) {
	p := &Pipeline{
		CorrectModel: "m",
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "not json", nil
		}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "whatever", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 0 {
		t.Fatalf("expected no event for malformed JSON, got %+v", got)
	}
}

func TestCorrectIgnoresLLMError(t *testing.T) {
	p := &Pipeline{
		CorrectModel: "m",
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "whatever", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 0 {
		t.Fatalf("expected no event when the LLM call fails, got %+v", got)
	}
}

// ---- prompt helpers -----------------------------------------------------------

func TestLanguageName(t *testing.T) {
	cases := map[string]string{
		"ko": "Korean", "KO-KR": "Korean", "en": "English", "ja": "Japanese",
		"zh": "Chinese", "es": "Spanish", "": "Korean", "auto": "Korean",
		"fr": "fr", // unknown code falls back to itself
	}
	for in, want := range cases {
		if got := languageName(in); got != want {
			t.Errorf("languageName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCorrectionSystemPromptNamesTargetLanguage(t *testing.T) {
	p := correctionSystemPrompt("ko")
	if !strings.Contains(p, "Korean") {
		t.Fatalf("prompt should mention Korean: %s", p)
	}
}

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

func TestHandleTextEndToEnd(t *testing.T) {
	p := &Pipeline{
		LLM: &fakeLLM{
			chatReply: "Nice to meet you!",
			complete: func(msgs []llm.Message) (string, error) {
				return `{"corrected":"Hello, my name is Alex.","issues":[{"type":"grammar","span":"name Alex","suggestion":"my name is Alex","explanation":"주어 누락"}]}`, nil
			},
		},
		ChatModel: "chat-model", CorrectModel: "correct-model", FeedbackLang: "ko",
	}
	sess := session.New("sys")
	events := make(chan protocol.ServerEvent, 16)
	p.HandleText(context.Background(), sess, "  Hello name Alex  ", func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	byType := map[protocol.EventType][]protocol.ServerEvent{}
	for _, ev := range got {
		byType[ev.Type] = append(byType[ev.Type], ev)
	}

	if len(byType[protocol.EvFinal]) != 1 || byType[protocol.EvFinal][0].Text != "Hello name Alex" {
		t.Fatalf("final_transcript wrong: %+v", byType[protocol.EvFinal])
	}
	if len(byType[protocol.EvAssistantDone]) != 1 || byType[protocol.EvAssistantDone][0].Text != "Nice to meet you!" {
		t.Fatalf("assistant_done wrong: %+v", byType[protocol.EvAssistantDone])
	}
	if len(byType[protocol.EvCorrection]) != 1 {
		t.Fatalf("expected one correction event, got %+v", byType[protocol.EvCorrection])
	}
	corr := byType[protocol.EvCorrection][0].Correction
	if corr == nil || corr.Corrected != "Hello, my name is Alex." || len(corr.Issues) != 1 {
		t.Fatalf("correction payload wrong: %+v", corr)
	}
	if corr.Issues[0].Explanation != "주어 누락" {
		t.Fatalf("correction explanation not in Korean: %+v", corr.Issues[0])
	}

	_, recent := sess.Export()
	if len(recent) != 2 || recent[0].Content != "Hello name Alex" || recent[1].Content != "Nice to meet you!" {
		t.Fatalf("session history wrong: %+v", recent)
	}
}

func TestHandleUtteranceEmptyTranscriptIsNoop(t *testing.T) {
	p := &Pipeline{FastSTT: fakeSTT{text: "   "}, SlowSTT: fakeSTT{text: "irrelevant"}, LLM: &fakeLLM{}}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 0 {
		t.Fatalf("expected no events for an empty transcript, got %+v", got)
	}
	_, recent := sess.Export()
	if len(recent) != 0 {
		t.Fatalf("session should be untouched, got %+v", recent)
	}
}

func TestHandleUtteranceSTTErrorEmitsError(t *testing.T) {
	p := &Pipeline{FastSTT: fakeSTT{err: errors.New("mic disconnected")}, LLM: &fakeLLM{}}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Type != protocol.EvError {
		t.Fatalf("expected a single error event, got %+v", got)
	}
	if !strings.Contains(got[0].Text, "mic disconnected") {
		t.Fatalf("error text = %q", got[0].Text)
	}
}

// TestHandleUtteranceFullFlowUpgradesContextViaRefine guards a deliberate
// product decision: the refine track upgrades the session's user turn to the
// higher-quality STT re-transcription (accuracy), never to the grammar-
// corrected version — the learner's actual mistakes must stay in context.
func TestHandleUtteranceFullFlowUpgradesContextViaRefine(t *testing.T) {
	p := &Pipeline{
		FastSTT: fakeSTT{text: "i are hungry"},
		SlowSTT: fakeSTT{text: "I am hungry"}, // higher-quality re-transcription
		LLM: &fakeLLM{
			chatReply: "Let's get you some food!",
			complete: func(msgs []llm.Message) (string, error) {
				return `{"corrected":"I am hungry.","issues":[{"type":"grammar","span":"I are","suggestion":"I am","explanation":"be동사 인칭 오류"}]}`, nil
			},
		},
		ChatModel: "chat-model", CorrectModel: "correct-model", FeedbackLang: "ko",
	}
	sess := session.New("sys")
	events := make(chan protocol.ServerEvent, 16)
	p.HandleUtterance(context.Background(), sess, []byte("pcm-data"), func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	byType := map[protocol.EventType][]protocol.ServerEvent{}
	for _, ev := range got {
		byType[ev.Type] = append(byType[ev.Type], ev)
	}

	if len(byType[protocol.EvFinal]) != 1 || byType[protocol.EvFinal][0].Text != "i are hungry" {
		t.Fatalf("final_transcript (fast STT) wrong: %+v", byType[protocol.EvFinal])
	}
	if len(byType[protocol.EvRefined]) != 1 || byType[protocol.EvRefined][0].Text != "I am hungry" {
		t.Fatalf("refined_transcript (slow STT) wrong: %+v", byType[protocol.EvRefined])
	}
	if len(byType[protocol.EvAssistantDone]) != 1 || byType[protocol.EvAssistantDone][0].Text != "Let's get you some food!" {
		t.Fatalf("assistant_done wrong: %+v", byType[protocol.EvAssistantDone])
	}
	if len(byType[protocol.EvCorrection]) != 1 {
		t.Fatalf("expected a correction event, got %+v", byType[protocol.EvCorrection])
	}

	_, recent := sess.Export()
	if len(recent) != 2 {
		t.Fatalf("expected 2 messages (user+assistant), got %+v", recent)
	}
	if recent[0].Content != "I am hungry" {
		t.Fatalf("session user turn not upgraded by the refine track: %+v", recent[0])
	}
	if recent[1].Content != "Let's get you some food!" {
		t.Fatalf("assistant reply wrong: %+v", recent[1])
	}
}
