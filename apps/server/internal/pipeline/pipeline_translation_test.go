package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

func TestTranslateAssistantEmitsEvent(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return "안녕하세요, 오늘 어때요?", nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 3, "Hello, how are you today?", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Type != protocol.EvAssistantTranslation {
		t.Fatalf("expected one assistant_translation event, got %+v", got)
	}
	if got[0].Text != "안녕하세요, 오늘 어때요?" || got[0].Turn != 3 {
		t.Fatalf("translation event wrong: %+v", got[0])
	}
}

func TestTranslateAssistantIgnoresLLMError(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 1, "whatever", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 0 {
		t.Fatalf("expected no event when the LLM call fails, got %+v", got)
	}
}

// TestTranslateAssistantSkipsEmptyResult: analyze() itself never succeeds
// with an all-whitespace result (a candidate's blank output is filtered out
// before it can win — see analyze()), so a single candidate returning only
// whitespace makes analyze() fail outright ("every candidate failed"), which
// translateAssistant swallows without emitting, same as any other analyze()
// error.
func TestTranslateAssistantSkipsEmptyResult(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return "   ", nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 1, "whatever", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 0 {
		t.Fatalf("expected no event when analyze() has nothing but blank output, got %+v", got)
	}
}

// TestTranslateAssistantTrimsWhitespace guards the trim itself: analyze() can
// still return a non-empty candidate with leading/trailing whitespace (e.g.
// a model wrapping its answer in a newline), and the emitted event's Text
// must be trimmed before it reaches the client.
func TestTranslateAssistantTrimsWhitespace(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return "  안녕하세요  \n", nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 1, "whatever", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 1 || got[0].Text != "안녕하세요" {
		t.Fatalf("expected trimmed translation text, got %+v", got)
	}
}

// TestTranslateAssistantEmitsFastThenRefinedWhenDifferent guards the
// two-stage flow: the FAST pass (p.LLM/p.ChatModel) emits first, and the
// slower REFINE ensemble patches it in with a second event once it lands on
// a different translation.
func TestTranslateAssistantEmitsFastThenRefinedWhenDifferent(t *testing.T) {
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "빠른 번역", nil }},
		ChatModel: "chat",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "정제된 번역", nil }}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 3, "Hello, how are you today?", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 || got[0].Type != protocol.EvAssistantTranslation || got[1].Type != protocol.EvAssistantTranslation {
		t.Fatalf("expected two assistant_translation events (fast, then refined), got %+v", got)
	}
	if got[0].Text != "빠른 번역" {
		t.Fatalf("fast event should carry the fast pass's own translation, got %+v", got[0])
	}
	if got[1].Text != "정제된 번역" {
		t.Fatalf("refined event should carry the ensemble's translation, got %+v", got[1])
	}
}

// TestTranslateAssistantSkipsSecondEmitWhenSame is
// TestTranslateAssistantEmitsFastThenRefinedWhenDifferent's counterpart: no
// second event when the ensemble agrees with the fast pass.
func TestTranslateAssistantSkipsSecondEmitWhenSame(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) { return "같은 번역", nil }
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "chat",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 3, "Hello, how are you today?", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 {
		t.Fatalf("expected exactly one assistant_translation event when fast and refined agree, got %+v", got)
	}
}

// TestTranslateAssistantFastFailureFallsBackToRefineOnly: a fast-pass error
// must not stop the ensemble's own result from reaching the learner, and
// must not itself produce any event (translateAssistant has never reported
// translation failures — see TestTranslateAssistantIgnoresLLMError).
func TestTranslateAssistantFastFailureFallsBackToRefineOnly(t *testing.T) {
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "", errors.New("chat model down") }},
		ChatModel: "chat",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "정제된 번역", nil }}}},
	}
	var got []protocol.ServerEvent
	p.translateAssistant(context.Background(), "alex", "sess-1", 3, "Hello, how are you today?", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Text != "정제된 번역" {
		t.Fatalf("expected exactly one event carrying the ensemble's translation, got %+v", got)
	}
}

// ---- TranslateWithContext() -----------------------------------------------------

func TestTranslateWithContextSendsBareTextWhenNoPriorTurns(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return "  안녕하세요  ", nil
		}}}},
	}
	got, err := p.TranslateWithContext(context.Background(), nil, "hello")
	if err != nil {
		t.Fatalf("TranslateWithContext() error = %v", err)
	}
	if got != "안녕하세요" {
		t.Fatalf("TranslateWithContext() = %q, want trimmed translation", got)
	}
	if gotInput != "hello" {
		t.Fatalf("input with no prior turns should be the bare text, got %q", gotInput)
	}
}

func TestTranslateWithContextFoldsPriorTurnsIntoInput(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return "그건 어디에 있나요?", nil
		}}}},
	}
	prior := []llm.Message{
		{Role: llm.RoleAssistant, Content: "I left my keys on the table."},
	}
	got, err := p.TranslateWithContext(context.Background(), prior, "Where is it?")
	if err != nil {
		t.Fatalf("TranslateWithContext() error = %v", err)
	}
	if got != "그건 어디에 있나요?" {
		t.Fatalf("TranslateWithContext() = %q", got)
	}
	if !strings.Contains(gotInput, "I left my keys on the table.") {
		t.Fatalf("input should fold in prior turns for context, got %q", gotInput)
	}
	if !strings.Contains(gotInput, "Text to translate:\nWhere is it?") {
		t.Fatalf("input should label the text under translation, got %q", gotInput)
	}
}

func TestTranslateWithContextPropagatesAnalyzeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	if _, err := p.TranslateWithContext(context.Background(), nil, "whatever"); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

// ---- translation concurrency/priority (acquireTranslationSlot) -----------

// TestTranslationCallsAreSerializedAcrossLiveAndBackfill fires several
// translateAssistant calls (live, per-turn) and a TranslateWithContext call
// (backfill) concurrently against a slow candidate, and asserts the observed
// concurrency never exceeds 1 — both entry points must share the same
// process-wide translation slot.
func TestTranslationCallsAreSerializedAcrossLiveAndBackfill(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	slow := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return "translated", nil
	}}
	// translateAssistant's FAST pass (p.LLM/p.ChatModel) is a separate,
	// immediate call unrelated to the ensemble slot this test measures —
	// give it its own fast fake so it doesn't perturb maxInFlight.
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "fast", nil }},
		ChatModel: "chat",
		Analysis:  []Candidate{{Model: "m", LLM: slow}},
	}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(turn int) {
			defer wg.Done()
			p.translateAssistant(context.Background(), "alex", "sess-1", turn, "hello", func(protocol.ServerEvent) {})
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := p.TranslateWithContext(context.Background(), nil, "hi"); err != nil {
			t.Errorf("TranslateWithContext() error = %v", err)
		}
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Fatalf("max concurrent translation calls = %d, want 1", maxInFlight)
	}
}

// TestAcquireTranslationSlotWaitsForChatActivity asserts that a translation
// slot acquisition blocks while chatActive is nonzero and unblocks as soon
// as chat activity ends — translation defers to an in-flight chat reply
// rather than contending with it, but is never itself cancelled once
// acquired (that half is exercised by
// TestTranslationCallsAreSerializedAcrossLiveAndBackfill's non-preemption).
func TestAcquireTranslationSlotWaitsForChatActivity(t *testing.T) {
	p := &Pipeline{}
	atomic.AddInt32(&p.chatActive, 1)

	done := make(chan error, 1)
	go func() { done <- p.acquireTranslationSlot(context.Background()) }()

	select {
	case <-done:
		t.Fatal("acquireTranslationSlot returned while chat was still active")
	case <-time.After(150 * time.Millisecond):
	}

	atomic.AddInt32(&p.chatActive, -1)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acquireTranslationSlot() error = %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("acquireTranslationSlot did not return after chat activity ended")
	}
}

// TestAcquireTranslationSlotRespectsContextCancellation asserts that waiting
// for the (already-held) translation slot gives up promptly when ctx is
// cancelled, instead of blocking until the slot frees.
func TestAcquireTranslationSlotRespectsContextCancellation(t *testing.T) {
	p := &Pipeline{}
	if err := p.acquireTranslationSlot(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer p.releaseTranslationSlot()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := p.acquireTranslationSlot(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireTranslationSlot() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("acquireTranslationSlot took too long to respect cancellation: %v", elapsed)
	}
}

// ---- GenerateTitle() -----------------------------------------------------------

func TestTranslationSystemPromptNamesTargetLanguage(t *testing.T) {
	p := translationSystemPrompt("ko")
	if !strings.Contains(p, "Korean") {
		t.Fatalf("prompt should mention Korean: %s", p)
	}
}
