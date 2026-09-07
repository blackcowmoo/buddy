package pipeline

import (
	"context"
	"errors"
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

// fakeLLM is a deterministic llm.Client double. ChatStream and Complete both
// check ctx.Err() first, mirroring a real HTTP client failing immediately on
// an already-cancelled context — this is what makes the barge-in tests exact,
// including that correct()/translateAssistant() now run on
// context.Background() and so must stay unaffected even when the caller's
// own ctx is cancelled out from under them (see TestReply*SurvivesBargeIn*).
type fakeLLM struct {
	mu sync.Mutex

	chatReply string
	chatErr   error
	complete  func(msgs []llm.Message) (string, error)
	// onChat, if set, is called with the exact msgs ChatStream received —
	// for tests asserting on prompt content, not just the reply.
	onChat func(msgs []llm.Message)
}

func (f *fakeLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onChat != nil {
		f.onChat(msgs)
	}
	if f.chatErr != nil {
		return "", f.chatErr
	}
	if onToken != nil {
		onToken(f.chatReply)
	}
	return f.chatReply, nil
}

func (f *fakeLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
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

// gateAnalysisLLM blocks each Complete() call until release is closed, or
// returns ctx's error if ctx is cancelled first — mirroring how a real HTTP
// call aborts when its context cancels mid-flight. Used to pin an analysis
// call in flight so a test can cancel the caller's context and verify
// whether the call is actually tied to it.
type gateAnalysisLLM struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	reply   string
}

func (g *gateAnalysisLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errors.New("gateAnalysisLLM: ChatStream not used")
}

func (g *gateAnalysisLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return g.reply, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestStartConversationEmitsDeltaThenDoneOnTurnZeroAndAppends(t *testing.T) {
	sess := session.New("sys")
	p := &Pipeline{LLM: &fakeLLM{chatReply: "Hey! What's on your mind today?"}, ChatModel: "m"}

	events := make(chan protocol.ServerEvent, 8)
	p.StartConversation(context.Background(), "alex", "sess-1", sess, func(ev protocol.ServerEvent) { events <- ev })
	close(events)

	var got []protocol.ServerEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].Type != protocol.EvAssistantDelta || got[1].Type != protocol.EvAssistantDone {
		t.Fatalf("unexpected events: %+v", got)
	}
	for _, ev := range got {
		if ev.Turn != 0 {
			t.Fatalf("opening greeting must use the reserved turn-0 sentinel, got %+v", ev)
		}
	}
	if got[1].Text != "Hey! What's on your mind today?" {
		t.Fatalf("assistant_done text = %q", got[1].Text)
	}
	_, recent := sess.Export()
	if len(recent) != 1 || recent[0].Role != llm.RoleAssistant || recent[0].Content != "Hey! What's on your mind today?" {
		t.Fatalf("greeting not appended to session history: %+v", recent)
	}
	// The greeting must not consume a turn number — the learner's first real
	// utterance still has to land on turn 1.
	if next := sess.NextTurn(); next != 1 {
		t.Fatalf("NextTurn() after greeting = %d, want 1", next)
	}
}

func TestStartConversationEmitsTranslation(t *testing.T) {
	sess := session.New("sys")
	p := &Pipeline{
		LLM:       &fakeLLM{chatReply: "Hi there!"},
		ChatModel: "m",
		Analysis: []Candidate{{Model: "t", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "안녕하세요!", nil
		}}}},
	}

	events := make(chan protocol.ServerEvent, 8)
	p.StartConversation(context.Background(), "alex", "sess-1", sess, func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	var translations []protocol.ServerEvent
	for _, ev := range got {
		if ev.Type == protocol.EvAssistantTranslation {
			translations = append(translations, ev)
		}
	}
	if len(translations) != 1 || translations[0].Text != "안녕하세요!" || translations[0].Turn != 0 {
		t.Fatalf("expected one turn-0 assistant_translation event, got %+v (all events: %+v)", translations, got)
	}
}

func TestStartConversationFallbackOnLLMError(t *testing.T) {
	sess := session.New("sys")
	p := &Pipeline{LLM: &fakeLLM{chatErr: errors.New("connection refused")}, ChatModel: "m"}

	var got []protocol.ServerEvent
	p.StartConversation(context.Background(), "alex", "sess-1", sess, func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 || got[0].Text != openingFallback || got[1].Text != openingFallback {
		t.Fatalf("expected fallback delta + done with the canned opening line, got %+v", got)
	}
	_, recent := sess.Export()
	if len(recent) != 1 || recent[0].Content != openingFallback {
		t.Fatalf("fallback greeting not appended: %+v", recent)
	}
}

func TestStartConversationSkipsDoneAndAppendOnDisconnect(t *testing.T) {
	sess := session.New("sys")
	p := &Pipeline{LLM: &fakeLLM{chatReply: "should not be used"}, ChatModel: "m"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the connection closed before the greeting landed

	var got []protocol.ServerEvent
	p.StartConversation(ctx, "alex", "sess-1", sess, func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 0 {
		t.Fatalf("expected no events emitted after disconnect, got %+v", got)
	}
	_, recent := sess.Export()
	if len(recent) != 0 {
		t.Fatalf("greeting must not be appended after disconnect, got %+v", recent)
	}
}

func TestOpeningSystemPromptPersonalizesFromLearnerProfileWithVariety(t *testing.T) {
	if !strings.Contains(openingSystemPrompt, "learner profile") {
		t.Fatalf("opening prompt should instruct drawing on the learner profile when available: %s", openingSystemPrompt)
	}
	if !strings.Contains(openingSystemPrompt, "Vary which detail you pick each time") {
		t.Fatalf("opening prompt should instruct varying which profile detail is used, not defaulting to one: %s", openingSystemPrompt)
	}
	if !strings.Contains(openingSystemPrompt, "never say or imply that you're recalling stored notes") {
		t.Fatalf("opening prompt should keep the personalization implicit, matching BuildSystemPrompt's 'never mention this explicitly' rule: %s", openingSystemPrompt)
	}
	if !strings.Contains(openingSystemPrompt, "avoid always asking the same question") {
		t.Fatalf("opening prompt should still vary the fallback (no-profile) question: %s", openingSystemPrompt)
	}
}

// TestOpeningSystemPromptCalibratesDifficultyToLearnerLevel guards the
// "induce a question appropriate to the learner's level" requirement: the
// opening question (asked for every brand-new room, whether started from the
// normal "+ 새 대화" entry point or the "오늘의 한 문장" quick-mode one — both
// call the same StartConversation) should scale its vocabulary/grammar to
// whatever the learner profile says about proficiency, and fall back to
// simple wording when nothing is known yet rather than guessing too hard.
func TestOpeningSystemPromptCalibratesDifficultyToLearnerLevel(t *testing.T) {
	if !strings.Contains(openingSystemPrompt, "proficiency level") {
		t.Fatalf("opening prompt should calibrate to a proficiency level noted in the learner profile: %s", openingSystemPrompt)
	}
	if !strings.Contains(openingSystemPrompt, "beginner") || !strings.Contains(openingSystemPrompt, "advanced") {
		t.Fatalf("opening prompt should give concrete beginner vs. advanced calibration guidance: %s", openingSystemPrompt)
	}
	if !strings.Contains(openingSystemPrompt, "default to simple") {
		t.Fatalf("opening prompt should default to simple wording when nothing is known about the learner yet: %s", openingSystemPrompt)
	}
}

// TestStartConversationSendsLearnerProfileAlongsideOpeningPrompt guards that
// StartConversation's msgs actually carry whatever learner-profile context
// BuildSystemPrompt layered into the session — the opening prompt's
// personalization instructions are useless if the profile text itself never
// reaches the model.
func TestStartConversationSendsLearnerProfileAlongsideOpeningPrompt(t *testing.T) {
	sess := session.New(BuildSystemPrompt("", "loves hiking, preparing for a job interview"))
	var got []llm.Message
	p := &Pipeline{
		LLM: &fakeLLM{chatReply: "Hey! How's the job interview prep going?", onChat: func(msgs []llm.Message) {
			got = msgs
		}},
		ChatModel: "m",
	}

	p.StartConversation(context.Background(), "alex", "sess-1", sess, func(protocol.ServerEvent) {})

	var combined strings.Builder
	for _, m := range got {
		combined.WriteString(m.Content)
	}
	if !strings.Contains(combined.String(), "loves hiking, preparing for a job interview") {
		t.Fatalf("learner profile should reach the model alongside the opening prompt, got messages: %+v", got)
	}
	if !strings.Contains(combined.String(), "learner profile") {
		t.Fatalf("opening system message should still be present, got messages: %+v", got)
	}
}

// ---- compact() -----------------------------------------------------------------

// TestHandleTextCorrectionSurvivesCtxCancellation guards HandleText's
// backgrounded correct() call: it must run on a context detached from ctx
// (see pipeline.go's context.WithoutCancel there), so a barge-in or
// disconnect that cancels the turn context doesn't silently drop the
// grammar-check result. This is the only place voice input reaches correct()
// too now (see HandleText's doc comment) — a confirmed voice draft goes
// through this exact same call.
func TestHandleTextCorrectionSurvivesCtxCancellation(t *testing.T) {
	gate := &gateAnalysisLLM{
		started: make(chan struct{}),
		release: make(chan struct{}),
		reply:   `{"corrected":"fixed.","issues":[]}`,
	}
	p := &Pipeline{
		LLM:       &fakeLLM{chatReply: "ok"},
		ChatModel: "chat-model",
		Analysis:  []Candidate{{Model: "m", LLM: gate}},
	}
	sess := session.New("sys")

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan protocol.ServerEvent, 8)
	go p.HandleText(ctx, "alex", "sess-1", sess, "broken", protocol.SourceText, func(ev protocol.ServerEvent) { events <- ev })

	<-gate.started                    // correct()'s analyze() call is in flight
	cancel()                          // simulate a barge-in/disconnect cancelling the turn context
	time.Sleep(50 * time.Millisecond) // give the cancellation a chance to (wrongly) abort the call
	close(gate.release)               // let the call actually finish

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	var corrections []protocol.ServerEvent
	for _, ev := range got {
		if ev.Type == protocol.EvCorrection {
			corrections = append(corrections, ev)
		}
	}
	if len(corrections) != 1 || corrections[0].Correction.Corrected != "fixed." {
		t.Fatalf("expected correct()'s result to survive cancellation of the turn context, got %+v (all events: %+v)", corrections, got)
	}
}

// ---- HandleUtterance draft flow (EvPendingTranscript) --------------------------
//
// HandleUtterance no longer commits anything: it only ever emits
// EvPendingTranscript (once, or twice if a Judge upgrade differs) and
// otherwise leaves the session, correct(), and reply() entirely untouched —
// those only run once the learner sends the draft back as a "text" ClientMsg
// (see HandleText). Pipeline.LLM/ChatModel/Analysis are deliberately left
// unset in these tests: if HandleUtterance ever regressed into calling
// reply()/correct() again, that would panic on the nil LLM instead of
// silently passing.

func TestHandleUtteranceEmitsPendingTranscriptWithNoJudgeConfigured(t *testing.T) {
	p := &Pipeline{STT: []stt.Recognizer{fakeSTT{text: "i are hungry"}}}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), "alex", "sess-1", sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Type != protocol.EvPendingTranscript || got[0].Text != "i are hungry" {
		t.Fatalf("expected a single pending_transcript event, got %+v", got)
	}
	if got[0].Source != protocol.SourceVoice {
		t.Fatalf("pending_transcript Source = %q, want %q", got[0].Source, protocol.SourceVoice)
	}
	_, recent := sess.Export()
	if len(recent) != 0 {
		t.Fatalf("session should be untouched by an unconfirmed draft, got %+v", recent)
	}
}

func TestHandleUtteranceEmitsUpgradedPendingTranscriptWhenJudgeDisagrees(t *testing.T) {
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "I am hungry", nil // Judge reconciles the STT mishearing using context
	}}
	p := &Pipeline{
		STT:        []stt.Recognizer{fakeSTT{text: "i are hungry"}},
		Judge:      judge,
		JudgeModel: "judge-model",
	}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), "alex", "sess-1", sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 {
		t.Fatalf("expected fast + upgraded pending_transcript events, got %+v", got)
	}
	if got[0].Type != protocol.EvPendingTranscript || got[0].Text != "i are hungry" {
		t.Fatalf("first event should be the FAST guess, got %+v", got[0])
	}
	if got[1].Type != protocol.EvPendingTranscript || got[1].Text != "I am hungry" || got[1].Source != protocol.SourceVoice {
		t.Fatalf("second event should be the Judge-reconciled guess, got %+v", got[1])
	}
	_, recent := sess.Export()
	if len(recent) != 0 {
		t.Fatalf("session should be untouched by an unconfirmed draft, got %+v", recent)
	}
}

func TestHandleUtteranceReusesChatReconciliationThroughAnalysisAndJudge(t *testing.T) {
	var calls []string
	var analysisInput, judgeInput string
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{text: "ice scream"},
			fakeSTT{text: "ice cream"},
		},
		LLM: &fakeLLM{complete: func([]llm.Message) (string, error) {
			calls = append(calls, "chat")
			return "ice scream", nil
		}},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "analysis")
			analysisInput = msgs[len(msgs)-1].Content
			return "ice cream", nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "judge")
			judgeInput = msgs[len(msgs)-1].Content
			return "ice cream", nil
		}},
		JudgeModel: "judge",
	}
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), "alex", "sess-1", session.New("sys"), []byte("pcm"), func(ev protocol.ServerEvent) {
		got = append(got, ev)
	})

	if !reflect.DeepEqual(calls, []string{"chat", "analysis", "judge"}) {
		t.Fatalf("stage order = %v, want chat -> analysis -> judge", calls)
	}
	if !strings.Contains(analysisInput, "Chat draft to refine:\nice scream") {
		t.Fatalf("Analysis input = %q, want exact Chat reconciliation", analysisInput)
	}
	if !strings.Contains(judgeInput, "Chat draft:\nice scream") || !strings.Contains(judgeInput, "ice cream") {
		t.Fatalf("Judge input = %q, want Chat and Analysis results", judgeInput)
	}
	if len(got) != 2 || got[0].Text != "ice scream" || got[1].Text != "ice cream" {
		t.Fatalf("pending transcript events = %+v, want Chat preview then Judge upgrade", got)
	}
}

func TestHandleUtteranceNoopSecondEmitWhenJudgeAgrees(t *testing.T) {
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "same text", nil }}
	p := &Pipeline{
		STT:        []stt.Recognizer{fakeSTT{text: "same text"}},
		Judge:      judge,
		JudgeModel: "judge-model",
	}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), "alex", "sess-1", sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 {
		t.Fatalf("expected no second pending_transcript event when Judge agrees, got %+v", got)
	}
}

func TestHandleUtteranceKeepsFastGuessOnJudgeError(t *testing.T) {
	judge := &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "", errors.New("judge down") }}
	p := &Pipeline{
		STT:        []stt.Recognizer{fakeSTT{text: "fast text"}},
		Judge:      judge,
		JudgeModel: "judge-model",
	}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), "alex", "sess-1", sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Text != "fast text" {
		t.Fatalf("expected only the fast guess on judge error, got %+v", got)
	}
}

// ---- prompt helpers -----------------------------------------------------------

func TestBuildSystemPromptWithoutStyleOrProfileReturnsBasePersonaUnchanged(t *testing.T) {
	if got := BuildSystemPrompt("", ""); got != basePersonaPrompt {
		t.Fatalf("BuildSystemPrompt(\"\", \"\") = %q, want the base persona verbatim", got)
	}
	if got := BuildSystemPrompt("   ", "   "); got != basePersonaPrompt {
		t.Fatalf("BuildSystemPrompt(whitespace, whitespace) = %q, want the base persona verbatim", got)
	}
}

func TestBuildSystemPromptLayersLearnersStyleOntoBasePersona(t *testing.T) {
	got := BuildSystemPrompt("  ask interview-style questions  ", "")
	if !strings.Contains(got, basePersonaPrompt) {
		t.Fatalf("prompt should still contain the base persona: %s", got)
	}
	if !strings.Contains(got, "ask interview-style questions") {
		t.Fatalf("prompt should contain the (trimmed) learner style: %s", got)
	}
}

func TestBuildSystemPromptLayersLearnerProfileOntoBasePersona(t *testing.T) {
	got := BuildSystemPrompt("", "  struggles with third-person -s  ")
	if !strings.Contains(got, basePersonaPrompt) {
		t.Fatalf("prompt should still contain the base persona: %s", got)
	}
	if !strings.Contains(got, "struggles with third-person -s") {
		t.Fatalf("prompt should contain the (trimmed) learner profile: %s", got)
	}
}

func TestBuildSystemPromptLayersBothStyleAndProfile(t *testing.T) {
	got := BuildSystemPrompt("ask interview-style questions", "struggles with third-person -s")
	if !strings.Contains(got, "ask interview-style questions") || !strings.Contains(got, "struggles with third-person -s") {
		t.Fatalf("prompt should contain both style and profile: %s", got)
	}
}

func TestHandleTextEndToEnd(t *testing.T) {
	shared := &fakeLLM{
		chatReply: "Nice to meet you!",
		complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"Hello, my name is Alex.","issues":[{"type":"grammar","span":"name Alex","suggestion":"my name is Alex","explanation":"missing subject","explanationTranslation":"주어 누락"}]}`, nil
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
	p.HandleText(context.Background(), "alex", "sess-1", sess, "  Hello name Alex  ", protocol.SourceText, func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	byType := map[protocol.EventType][]protocol.ServerEvent{}
	for _, ev := range got {
		byType[ev.Type] = append(byType[ev.Type], ev)
	}

	if len(byType[protocol.EvFinal]) != 1 || byType[protocol.EvFinal][0].Text != "Hello name Alex" {
		t.Fatalf("final_transcript wrong: %+v", byType[protocol.EvFinal])
	}
	if byType[protocol.EvFinal][0].Source != protocol.SourceText {
		t.Fatalf("final_transcript Source = %q, want %q (typed input)", byType[protocol.EvFinal][0].Source, protocol.SourceText)
	}
	if len(byType[protocol.EvAssistantDone]) != 1 || byType[protocol.EvAssistantDone][0].Text != "Nice to meet you!" {
		t.Fatalf("assistant_done wrong: %+v", byType[protocol.EvAssistantDone])
	}
	if len(byType[protocol.EvCorrection]) != 2 {
		t.Fatalf("expected Chat preview and Judge-final correction events, got %+v", byType[protocol.EvCorrection])
	}
	corr := byType[protocol.EvCorrection][len(byType[protocol.EvCorrection])-1].Correction
	if corr == nil || corr.Corrected != "Hello, my name is Alex." || len(corr.Issues) != 1 {
		t.Fatalf("correction payload wrong: %+v", corr)
	}
	if corr.Issues[0].Explanation != "missing subject" {
		t.Fatalf("correction explanation not in English: %+v", corr.Issues[0])
	}
	if corr.Issues[0].ExplanationTranslation != "주어 누락" {
		t.Fatalf("correction explanationTranslation not in Korean: %+v", corr.Issues[0])
	}

	_, recent := sess.Export()
	if len(recent) != 2 || recent[0].Content != "Hello name Alex" || recent[1].Content != "Nice to meet you!" {
		t.Fatalf("session history wrong: %+v", recent)
	}
}

// TestHandleTextRepliesBeforeCorrecting guards the priority order documented
// on HandleText: the assistant's answer must fully stream and complete
// before correct() (grammar feedback + this turn's translation) even starts,
// so a learner never sees a correction/translation badge on their own
// message land before the answer they're waiting on. Regression coverage
// for a bug where correct() ran concurrently with (and often finished
// before) the streamed reply, since correct() is a single non-streaming
// call and therefore usually faster than a full streamed chat reply.
func TestHandleTextRepliesBeforeCorrecting(t *testing.T) {
	shared := &fakeLLM{
		chatReply: "Nice to meet you!",
		complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"Hello, my name is Alex.","issues":[{"type":"grammar","span":"name Alex","suggestion":"my name is Alex","explanation":"missing subject"}],"translation":"안녕, 내 이름은 알렉스야."}`, nil
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
	p.HandleText(context.Background(), "alex", "sess-1", sess, "Hello name Alex", protocol.SourceText, func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	doneIdx, correctionIdx, translationIdx := -1, -1, -1
	for i, ev := range got {
		switch ev.Type {
		case protocol.EvAssistantDone:
			doneIdx = i
		case protocol.EvCorrection:
			if correctionIdx == -1 {
				correctionIdx = i
			}
		case protocol.EvUserTranslation:
			if translationIdx == -1 {
				translationIdx = i
			}
		}
	}
	if doneIdx == -1 || correctionIdx == -1 || translationIdx == -1 {
		t.Fatalf("expected assistant_done, correction, and user_translation events, got %+v", got)
	}
	if doneIdx > correctionIdx {
		t.Fatalf("assistant_done (index %d) must come before correction (index %d): %+v", doneIdx, correctionIdx, got)
	}
	if doneIdx > translationIdx {
		t.Fatalf("assistant_done (index %d) must come before user_translation (index %d): %+v", doneIdx, translationIdx, got)
	}
}

// TestHandleTextUnknownSourceNormalizedToText guards HandleText's source
// normalization: anything other than protocol.SourceVoice must fall back to
// protocol.SourceText, so a malformed/forged ClientMsg.Source never taints a
// typed turn with an incorrect "voice" label.
func TestHandleTextUnknownSourceNormalizedToText(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{chatReply: "ok"}, ChatModel: "chat-model"}
	sess := session.New("sys")
	events := make(chan protocol.ServerEvent, 8)
	p.HandleText(context.Background(), "alex", "sess-1", sess, "hi", "bogus", func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	if len(got) == 0 || got[0].Type != protocol.EvFinal || got[0].Source != protocol.SourceText {
		t.Fatalf("expected final_transcript with Source normalized to %q, got %+v", protocol.SourceText, got)
	}
}

func TestHandleUtteranceEmptyTranscriptIsNoop(t *testing.T) {
	p := &Pipeline{STT: []stt.Recognizer{fakeSTT{text: "   "}}, LLM: &fakeLLM{}}
	sess := session.New("sys")
	var got []protocol.ServerEvent
	p.HandleUtterance(context.Background(), "alex", "sess-1", sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

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
	p.HandleUtterance(context.Background(), "alex", "sess-1", sess, []byte("pcm"), func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Type != protocol.EvError {
		t.Fatalf("expected a single error event, got %+v", got)
	}
	if !strings.Contains(got[0].Text, "mic disconnected") {
		t.Fatalf("error text = %q", got[0].Text)
	}
}

// TestHandleTextCommitsConfirmedVoiceDraftEndToEnd guards the funnel's other
// half: once the learner sends back a draft HandleUtterance proposed (with
// Source tagged voice), it must go through the exact same commit path as
// typed input — session append, correct(), reply() — with the voice Source
// preserved on the committed turn.
func TestHandleTextCommitsConfirmedVoiceDraftEndToEnd(t *testing.T) {
	shared := &fakeLLM{
		chatReply: "Let's get you some food!",
		complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I am hungry.","issues":[{"type":"grammar","span":"I are","suggestion":"I am","explanation":"be-verb agreement error","explanationTranslation":"be동사 인칭 오류"}]}`, nil
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
	// "I am hungry" stands in for the learner's confirmed (possibly
	// hand-edited or Judge-upgraded) draft text — HandleText never re-runs STT.
	p.HandleText(context.Background(), "alex", "sess-1", sess, "I am hungry", protocol.SourceVoice, func(ev protocol.ServerEvent) { events <- ev })

	got := collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)
	byType := map[protocol.EventType][]protocol.ServerEvent{}
	for _, ev := range got {
		byType[ev.Type] = append(byType[ev.Type], ev)
	}

	if len(byType[protocol.EvFinal]) != 1 || byType[protocol.EvFinal][0].Text != "I am hungry" {
		t.Fatalf("final_transcript wrong: %+v", byType[protocol.EvFinal])
	}
	if byType[protocol.EvFinal][0].Source != protocol.SourceVoice {
		t.Fatalf("final_transcript Source = %q, want %q (confirmed voice draft)", byType[protocol.EvFinal][0].Source, protocol.SourceVoice)
	}
	if len(byType[protocol.EvAssistantDone]) != 1 || byType[protocol.EvAssistantDone][0].Text != "Let's get you some food!" {
		t.Fatalf("assistant_done wrong: %+v", byType[protocol.EvAssistantDone])
	}
	if len(byType[protocol.EvCorrection]) != 2 {
		t.Fatalf("expected Chat preview and Judge-final correction events, got %+v", byType[protocol.EvCorrection])
	}

	_, recent := sess.Export()
	if len(recent) != 2 {
		t.Fatalf("expected 2 messages (user+assistant), got %+v", recent)
	}
	if recent[0].Content != "I am hungry" {
		t.Fatalf("session user turn wrong: %+v", recent[0])
	}
	if recent[1].Content != "Let's get you some food!" {
		t.Fatalf("assistant reply wrong: %+v", recent[1])
	}
}

// TestHandleTextCorrectionContextExcludesCurrentTurn guards context capture
// shared by typed input and confirmed voice drafts alike: HandleText
// snapshots the conversation with sess.Export() BEFORE appending the new
// turn and threads it into correct(). If that capture ever moved after the
// append, the sentence under correction would leak into its own "prior
// conversation" block. Here the correction sees the prior turn but never the
// current words.
func TestHandleTextCorrectionContextExcludesCurrentTurn(t *testing.T) {
	var inputs []string
	analysis := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		inputs = append(inputs, msgs[len(msgs)-1].Content)
		return `{"corrected":"I am sad.","issues":[]}`, nil
	}}
	p := &Pipeline{
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
	p.HandleText(context.Background(), "alex", "sess-1", sess, "I are sad", protocol.SourceVoice, func(ev protocol.ServerEvent) { events <- ev })
	collectUntilQuiet(t, events, 200*time.Millisecond, 2*time.Second)

	analysis.mu.Lock()
	captured := append([]string(nil), inputs...)
	analysis.mu.Unlock()

	// The same Analysis ensemble also serves translateAssistant's reply
	// translation now, so filter down to the correction call specifically.
	var correctionCalls []string
	for _, in := range captured {
		if strings.Contains(in, "Sentence to correct:") {
			correctionCalls = append(correctionCalls, in)
		}
	}
	if len(correctionCalls) != 1 {
		t.Fatalf("expected exactly one correction call, got %+v (all analysis calls: %+v)", correctionCalls, captured)
	}
	const marker = "\nSentence to correct:\nI are sad"
	in := correctionCalls[0]
	markerAt := strings.Index(in, marker)
	if markerAt < 0 {
		t.Fatalf("correction input should contain the delimited current sentence, got %q", in)
	}
	ctxPart := in[:markerAt]
	if !strings.Contains(ctxPart, "I am happy") || !strings.Contains(ctxPart, "Glad to hear it!") {
		t.Fatalf("context should carry the prior turn, got %q", ctxPart)
	}
	if strings.Contains(ctxPart, "sad") {
		t.Fatalf("context must not include the current sentence under correction, got %q", ctxPart)
	}
}
