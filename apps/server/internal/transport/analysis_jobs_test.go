package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
)

// ---- correction ---------------------------------------------------------

func TestNewCorrectHookReturnsNilWithoutQueue(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	if hook := NewCorrectHook(pipe, newFakeStore(), nil); hook != nil {
		t.Fatalf("NewCorrectHook(nil queue) = %v, want nil (so correct() falls back to the direct in-process path)", hook)
	}
}

func TestCorrectHookFastPathPersistsAndCallsOnResult(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-correct", 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{
			complete: `{"corrected":"He goes to school.","translation":"그는 학교에 간다","issues":[{"type":"grammar","span":"he go","suggestion":"he goes","explanation":"third person singular","explanationTranslation":"3인칭 단수"}]}`,
		}}},
		FeedbackLang: "ko",
	}
	hook := NewCorrectHook(pipe, st, queue)

	var gotCorrected, gotTranslation string
	var gotIssues []protocol.Issue
	var called bool
	hook(context.Background(), "alex", "sess-correct", 1, "he go school", "",
		func(corrected string, issues []protocol.Issue, translation string) {
			called = true
			gotCorrected, gotIssues, gotTranslation = corrected, issues, translation
		},
	)

	if !called {
		t.Fatalf("onResult was never called")
	}
	if gotCorrected != "He goes to school." {
		t.Fatalf("corrected = %q, want %q", gotCorrected, "He goes to school.")
	}
	if len(gotIssues) != 1 {
		t.Fatalf("issues = %+v, want exactly 1", gotIssues)
	}
	if gotTranslation != "그는 학교에 간다" {
		t.Fatalf("translation = %q", gotTranslation)
	}

	_, turns, err := st.SessionDetail(context.Background(), "alex", "sess-correct")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	var found bool
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "user" {
			found = true
			if tn.Correction == nil || tn.Correction.Corrected != "He goes to school." {
				t.Fatalf("persisted correction = %+v, want the analyzed result", tn.Correction)
			}
			if tn.Translation != "그는 학교에 간다" {
				t.Fatalf("persisted translation = %q", tn.Translation)
			}
		}
	}
	if !found {
		t.Fatalf("user turn 1 not found: %+v", turns)
	}
}

// ---- live translation -----------------------------------------------------

func TestNewTranslateHookReturnsNilWithoutQueue(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	if hook := NewTranslateHook(pipe, newFakeStore(), nil); hook != nil {
		t.Fatalf("NewTranslateHook(nil queue) = %v, want nil (so translateAssistant() falls back to the direct in-process path)", hook)
	}
}

func TestTranslateHookFastPathPersistsAndCallsOnResult(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-translate", 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(context.Background(), "alex", "sess-translate", 1, "assistant", "Nice to meet you!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	pipe := &pipeline.Pipeline{
		Analysis:     []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: "만나서 반가워요!"}}},
		FeedbackLang: "ko",
	}
	hook := NewTranslateHook(pipe, st, queue)

	var got string
	var called bool
	hook(context.Background(), "alex", "sess-translate", 1, "Nice to meet you!", func(translation string) {
		called = true
		got = translation
	})

	if !called || got != "만나서 반가워요!" {
		t.Fatalf("onResult called=%v got=%q, want 만나서 반가워요!", called, got)
	}

	_, turns, err := st.SessionDetail(context.Background(), "alex", "sess-translate")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "assistant" && tn.Translation != "만나서 반가워요!" {
			t.Fatalf("persisted translation = %q", tn.Translation)
		}
	}
}

// ---- title ---------------------------------------------------------------

func TestTitleJobHandlerPersistsGeneratedTitle(t *testing.T) {
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-title", 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{LLM: fixedCompleteLLM{reply: "Hiking Trip Plans"}, ChatModel: "m"}
	handler := TitleJobHandler(pipe, st)

	payload := titleJobPayload{UserID: "alex", SessionID: "sess-title", UserText: "I went hiking", AssistantText: "Nice!"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := handler(context.Background(), asyncjob.Job{ID: "job-1", Kind: asyncjob.KindTitle, Payload: raw}); err != nil {
		t.Fatalf("handler() error = %v", err)
	}

	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-title")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Hiking Trip Plans" {
		t.Fatalf("title = %q, want the generated title", meta.Title)
	}
}

func TestHandlerGenerateTitleUsesQueueWhenConfigured(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-handler-title", 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	pipe := &pipeline.Pipeline{LLM: fixedCompleteLLM{reply: "Queued Title"}, ChatModel: "m"}
	h := NewHandler(pipe, nil, st, nil, nil)
	h.SetTitleQueue(queue)

	sess := session.New("sys")
	sess.AppendUser("first message")
	h.generateTitle("alex", "sess-handler-title", sess, "assistant reply")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-handler-title")
		if err == nil && meta.Title == "Queued Title" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("title was never set to the generated value via the queue path")
}

// ---- test doubles ---------------------------------------------------------

// fakeAnalysisLLM is a minimal llm.Client double for the Analysis ensemble:
// Complete always returns the same canned response, ChatStream is unused.
type fakeAnalysisLLM struct{ complete string }

func (f fakeAnalysisLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (f fakeAnalysisLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete, nil
}

// fixedCompleteLLM is a minimal llm.Client double for GenerateTitle (which
// calls Complete, not ChatStream).
type fixedCompleteLLM struct{ reply string }

func (f fixedCompleteLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (f fixedCompleteLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.reply, nil
}
