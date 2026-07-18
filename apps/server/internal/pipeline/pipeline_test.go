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
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls++
			return "should not be called", nil
		}}}},
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
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "rolled-up summary", nil
		}}}},
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
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("llm down")
		}}}},
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
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I like pizza.","issues":[{"type":"grammar","span":"I likes","suggestion":"I like","explanation":"수 일치 오류"}]}`, nil
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "I likes pizza", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

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
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I like pizza.","issues":[]}`, nil
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "I like pizza.", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 0 {
		t.Fatalf("expected no event for an already-correct sentence, got %+v", got)
	}
}

func TestCorrectIgnoresMalformedJSON(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "not json", nil
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "whatever", "", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 0 {
		t.Fatalf("expected no event for malformed JSON, got %+v", got)
	}
}

func TestCorrectIgnoresLLMError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), 1, "whatever", "", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 0 {
		t.Fatalf("expected no event when the LLM call fails, got %+v", got)
	}
}

func TestCorrectSendsBareSentenceWhenNoContext(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return `{"corrected":"ok","issues":[]}`, nil
		}}}},
	}
	p.correct(context.Background(), 1, "ok", "", func(protocol.ServerEvent) {})
	if gotInput != "ok" {
		t.Fatalf("with no context, analyze input should be the bare sentence, got %q", gotInput)
	}
}

func TestCorrectFoldsContextInFrontOfSentence(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return `{"corrected":"I am 20 years old.","issues":[]}`, nil
		}}}},
	}
	ctxMsg := "Conversation so far:\nassistant: How old are you?\n"
	p.correct(context.Background(), 1, "I am 20 years old.", ctxMsg, func(protocol.ServerEvent) {})

	if !strings.Contains(gotInput, "How old are you?") {
		t.Fatalf("analyze input missing the context block: %q", gotInput)
	}
	if !strings.HasSuffix(gotInput, "Sentence to correct:\nI am 20 years old.") {
		t.Fatalf("the sentence under correction should be delimited at the end: %q", gotInput)
	}
}

func TestRenderCorrectionContext(t *testing.T) {
	if got := renderCorrectionContext("", nil); got != "" {
		t.Fatalf("expected empty string with no summary/prior turns, got %q", got)
	}
	got := renderCorrectionContext("likes hiking", []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	})
	if !strings.Contains(got, "likes hiking") || !strings.Contains(got, "hi") || !strings.Contains(got, "hello") {
		t.Fatalf("renderCorrectionContext missing content: %q", got)
	}
}

func TestRenderCorrectionInput(t *testing.T) {
	if got := renderCorrectionInput("", "just the sentence"); got != "just the sentence" {
		t.Fatalf("no context should pass the sentence through unchanged, got %q", got)
	}
	got := renderCorrectionInput("CTX BLOCK\n", "the sentence")
	if !strings.HasPrefix(got, "CTX BLOCK") || !strings.HasSuffix(got, "Sentence to correct:\nthe sentence") {
		t.Fatalf("combined input wrong: %q", got)
	}
}

// ---- analyze() -----------------------------------------------------------------

func TestAnalyzeSingleCandidateSkipsJudge(t *testing.T) {
	judgeCalls := 0
	p := &Pipeline{
		Analysis: []Candidate{{Model: "solo", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "solo answer", nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			return "should not be called", nil
		}},
		JudgeModel: "judge",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "solo answer" {
		t.Fatalf("analyze() = %q, want %q", got, "solo answer")
	}
	if judgeCalls != 0 {
		t.Fatalf("judge should not be called for a single candidate, got %d calls", judgeCalls)
	}
}

func TestAnalyzeMultipleCandidatesSynthesizedByJudge(t *testing.T) {
	var judgeInput string
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "gemma-4-e4b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "candidate A", nil
			}}},
			{Model: "qwen3-6-35b-a3b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "candidate B", nil
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeInput = msgs[len(msgs)-1].Content
			return "synthesized answer", nil
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "synthesized answer" {
		t.Fatalf("analyze() = %q, want %q", got, "synthesized answer")
	}
	if !strings.Contains(judgeInput, "candidate A") || !strings.Contains(judgeInput, "candidate B") {
		t.Fatalf("judge input should include both candidates' outputs, got %q", judgeInput)
	}
}

func TestAnalyzeFallsBackToFirstCandidateOnJudgeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "first", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "first candidate answer", nil
			}}},
			{Model: "second", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "second candidate answer", nil
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("judge down")
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "first candidate answer" {
		t.Fatalf("analyze() = %q, want the first configured candidate's answer", got)
	}
}

func TestAnalyzeSkipsFailedCandidateWithoutCallingJudge(t *testing.T) {
	judgeCalls := 0
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "flaky", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down")
			}}},
			{Model: "ok", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "only surviving answer", nil
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			return "unused", nil
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "only surviving answer" {
		t.Fatalf("analyze() = %q, want %q", got, "only surviving answer")
	}
	if judgeCalls != 0 {
		t.Fatalf("judge should not be called when only one candidate survives, got %d calls", judgeCalls)
	}
}

func TestAnalyzeAllCandidatesFailReturnsError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "a", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down a")
			}}},
			{Model: "b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down b")
			}}},
		},
	}
	if _, err := p.analyze(context.Background(), "sys", "input", false); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

func TestAnalyzeNoCandidatesConfiguredReturnsError(t *testing.T) {
	p := &Pipeline{}
	if _, err := p.analyze(context.Background(), "sys", "input", false); err == nil {
		t.Fatal("expected an error when no candidates are configured")
	}
}

// ---- transcribe() -----------------------------------------------------------

func TestTranscribeSingleEngineSkipsSynthesis(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		STT: []stt.Recognizer{fakeSTT{text: "hello there"}},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			chatCalls++
			return "should not be called", nil
		}},
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "hello there" {
		t.Fatalf("final = %q, want %q", final, "hello there")
	}
	if len(candidates) != 1 || candidates[0] != "hello there" {
		t.Fatalf("candidates = %v, want [\"hello there\"]", candidates)
	}
	if chatCalls != 0 {
		t.Fatalf("chat model should not be called for a single STT engine, got %d calls", chatCalls)
	}
}

func TestTranscribeMultipleEnginesSynthesizedByChatModel(t *testing.T) {
	var synthInput string
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{text: "i scream"},
			fakeSTT{text: "ice cream"},
		},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			synthInput = msgs[len(msgs)-1].Content
			return "ice cream", nil
		}},
		ChatModel: "chat-model",
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "ice cream" {
		t.Fatalf("final = %q, want %q", final, "ice cream")
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v, want both engines' texts", candidates)
	}
	if !strings.Contains(synthInput, "i scream") || !strings.Contains(synthInput, "ice cream") {
		t.Fatalf("synthesis input should include both candidates, got %q", synthInput)
	}
}

func TestTranscribeFallsBackToFirstCandidateOnSynthesisError(t *testing.T) {
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{text: "first candidate"},
			fakeSTT{text: "second candidate"},
		},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("chat model down")
		}},
	}
	final, _, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "first candidate" {
		t.Fatalf("final = %q, want the first engine's candidate", final)
	}
}

func TestTranscribeSkipsFailedEngineWithoutSynthesizing(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{err: errors.New("engine down")},
			fakeSTT{text: "only surviving text"},
		},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			chatCalls++
			return "unused", nil
		}},
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "only surviving text" {
		t.Fatalf("final = %q, want %q", final, "only surviving text")
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want just the surviving engine's text", candidates)
	}
	if chatCalls != 0 {
		t.Fatalf("chat model should not be called with only one surviving candidate, got %d calls", chatCalls)
	}
}

func TestTranscribeAllEnginesFailReturnsErrorMentioningCause(t *testing.T) {
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{err: errors.New("engine A down")},
			fakeSTT{err: errors.New("engine B down")},
		},
	}
	_, _, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err == nil {
		t.Fatal("expected an error when every STT engine fails")
	}
	if !strings.Contains(err.Error(), "engine A down") || !strings.Contains(err.Error(), "engine B down") {
		t.Fatalf("err = %v, want it to mention both underlying failures", err)
	}
}

func TestTranscribeSilenceAcrossAllEnginesIsNotAnError(t *testing.T) {
	p := &Pipeline{
		STT: []stt.Recognizer{fakeSTT{text: "   "}, fakeSTT{text: ""}},
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v, want nil (silence is not a failure)", err)
	}
	if final != "" || candidates != nil {
		t.Fatalf("final/candidates = %q/%v, want empty", final, candidates)
	}
}

func TestTranscribeNoEnginesConfiguredReturnsError(t *testing.T) {
	p := &Pipeline{}
	if _, _, err := p.transcribe(context.Background(), "", nil, []byte("pcm")); err == nil {
		t.Fatal("expected an error when no STT engines are configured")
	}
}

// ---- refine() -----------------------------------------------------------------

func TestRefineUpgradesSessionWhenJudgeDisagrees(t *testing.T) {
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "the real sentence", nil
	}}
	analysis := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"corrected":"the real sentence","issues":[]}`, nil
	}}
	p := &Pipeline{
		Judge:      judge,
		JudgeModel: "judge-model",
		Analysis:   []Candidate{{Model: "m", LLM: analysis}},
	}
	sess := session.New("sys")
	sess.AppendUser("fast track guess")
	var got []protocol.ServerEvent
	p.refine(context.Background(), sess, 1, "", nil, []string{"fast track guess"}, "fast track guess", func(ev protocol.ServerEvent) { got = append(got, ev) })

	var refinedEvents []protocol.ServerEvent
	for _, ev := range got {
		if ev.Type == protocol.EvRefined {
			refinedEvents = append(refinedEvents, ev)
		}
	}
	if len(refinedEvents) != 1 || refinedEvents[0].Text != "the real sentence" {
		t.Fatalf("expected one refined_transcript event, got %+v", got)
	}
	_, recent := sess.Export()
	if len(recent) != 1 || recent[0].Content != "the real sentence" {
		t.Fatalf("session user turn not upgraded: %+v", recent)
	}
}

func TestRefineNoopWhenJudgeAgrees(t *testing.T) {
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "same text", nil
	}}
	analysis := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"corrected":"same text","issues":[]}`, nil
	}}
	p := &Pipeline{
		Judge:      judge,
		JudgeModel: "judge-model",
		Analysis:   []Candidate{{Model: "m", LLM: analysis}},
	}
	sess := session.New("sys")
	sess.AppendUser("same text")
	var got []protocol.ServerEvent
	p.refine(context.Background(), sess, 1, "", nil, []string{"same text"}, "same text", func(ev protocol.ServerEvent) { got = append(got, ev) })

	for _, ev := range got {
		if ev.Type == protocol.EvRefined {
			t.Fatalf("expected no refined_transcript event when Judge agrees, got %+v", got)
		}
	}
}

func TestRefineFallsBackToFastTextOnJudgeError(t *testing.T) {
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("judge down")
	}}
	analysis := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"corrected":"fast text","issues":[]}`, nil
	}}
	p := &Pipeline{
		Judge:      judge,
		JudgeModel: "judge-model",
		Analysis:   []Candidate{{Model: "m", LLM: analysis}},
	}
	sess := session.New("sys")
	sess.AppendUser("fast text")
	var got []protocol.ServerEvent
	p.refine(context.Background(), sess, 1, "", nil, []string{"fast text"}, "fast text", func(ev protocol.ServerEvent) { got = append(got, ev) })

	for _, ev := range got {
		if ev.Type == protocol.EvRefined {
			t.Fatalf("expected no refined_transcript event on judge error, got %+v", got)
		}
	}
	_, recent := sess.Export()
	if recent[0].Content != "fast text" {
		t.Fatalf("session user turn should be untouched on judge error: %+v", recent)
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
	shared := &fakeLLM{
		chatReply: "Nice to meet you!",
		complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"Hello, my name is Alex.","issues":[{"type":"grammar","span":"name Alex","suggestion":"my name is Alex","explanation":"주어 누락"}]}`, nil
		},
	}
	p := &Pipeline{
		LLM:          shared,
		ChatModel:    "chat-model",
		Analysis:     []Candidate{{Model: "correct-model", LLM: shared}},
		FeedbackLang: "ko",
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
	p := &Pipeline{STT: []stt.Recognizer{fakeSTT{text: "   "}}, LLM: &fakeLLM{}}
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
	p := &Pipeline{STT: []stt.Recognizer{fakeSTT{err: errors.New("mic disconnected")}}, LLM: &fakeLLM{}}
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
// product decision: the refine track upgrades the session's user turn to
// Judge's reconciled transcript (accuracy), never to the grammar-corrected
// version — the learner's actual mistakes must stay in context.
func TestHandleUtteranceFullFlowUpgradesContextViaRefine(t *testing.T) {
	shared := &fakeLLM{
		chatReply: "Let's get you some food!",
		complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I am hungry.","issues":[{"type":"grammar","span":"I are","suggestion":"I am","explanation":"be동사 인칭 오류"}]}`, nil
		},
	}
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "I am hungry", nil // Judge reconciles the STT mishearing using context
	}}
	p := &Pipeline{
		STT:          []stt.Recognizer{fakeSTT{text: "i are hungry"}},
		LLM:          shared,
		ChatModel:    "chat-model",
		Analysis:     []Candidate{{Model: "correct-model", LLM: shared}},
		Judge:        judge,
		JudgeModel:   "judge-model",
		FeedbackLang: "ko",
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
		t.Fatalf("final_transcript (FAST track) wrong: %+v", byType[protocol.EvFinal])
	}
	if len(byType[protocol.EvRefined]) != 1 || byType[protocol.EvRefined][0].Text != "I am hungry" {
		t.Fatalf("refined_transcript (Judge reconciliation) wrong: %+v", byType[protocol.EvRefined])
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
		t.Fatalf("session user turn not upgraded by refine: %+v", recent[0])
	}
	if recent[1].Content != "Let's get you some food!" {
		t.Fatalf("assistant reply wrong: %+v", recent[1])
	}
}

// TestHandleUtteranceCorrectionContextExcludesCurrentTurn guards the voice
// path's context capture: HandleUtterance snapshots the conversation with
// sess.Export() BEFORE appending the new utterance and threads it into
// refine()'s correction pass. If that capture ever moved after the append
// (or were recomputed inside refine(), racing reply()'s assistant append),
// the sentence under correction would leak into its own "prior conversation"
// block. Here the correction sees the prior turn but never the current words.
func TestHandleUtteranceCorrectionContextExcludesCurrentTurn(t *testing.T) {
	var inputs []string
	analysis := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		inputs = append(inputs, msgs[len(msgs)-1].Content)
		return `{"corrected":"I am sad.","issues":[]}`, nil
	}}
	p := &Pipeline{
		STT:          []stt.Recognizer{fakeSTT{text: "I are sad"}},
		LLM:          &fakeLLM{chatReply: "There, there."},
		ChatModel:    "chat-model",
		Analysis:     []Candidate{{Model: "correct-model", LLM: analysis}},
		FeedbackLang: "ko",
	}
	sess := session.New("sys")
	// A prior, completed turn already in the conversation.
	sess.AppendUser("I am happy")
	sess.AppendAssistant("Glad to hear it!")

	events := make(chan protocol.ServerEvent, 32)
	p.HandleUtterance(context.Background(), sess, []byte("pcm"), func(ev protocol.ServerEvent) { events <- ev })
	collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)

	analysis.mu.Lock()
	captured := append([]string(nil), inputs...)
	analysis.mu.Unlock()

	if len(captured) != 1 {
		t.Fatalf("expected exactly one correction call, got %+v", captured)
	}
	const marker = "\nSentence to correct:\nI are sad"
	in := captured[0]
	if !strings.HasSuffix(in, marker) {
		t.Fatalf("correction input should end with the current sentence, got %q", in)
	}
	ctxPart := strings.TrimSuffix(in, marker)
	if !strings.Contains(ctxPart, "I am happy") || !strings.Contains(ctxPart, "Glad to hear it!") {
		t.Fatalf("context should carry the prior turn, got %q", ctxPart)
	}
	if strings.Contains(ctxPart, "sad") {
		t.Fatalf("context must not include the current sentence under correction, got %q", ctxPart)
	}
}
