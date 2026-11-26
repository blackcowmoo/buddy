package transport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/checkpoint"
	"buddy/server/internal/checkpoint/checkpointtest"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/workguard"
	"buddy/server/internal/writing"
)

// checkJobStore mirrors the real store's pending/answer fences so the job's
// supersession and failure paths are exercised against real transitions.
type checkJobStore struct {
	writing.Store
	deletedOwner
	mu     sync.Mutex
	prompt writing.Prompt
}

func (s *checkJobStore) Get(context.Context, string, string) (writing.Prompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt, nil
}

func (s *checkJobStore) StartCheck(_ context.Context, _, id, answer string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prompt.ID != id {
		return false, nil
	}
	s.prompt.Answer, s.prompt.CheckStatus, s.prompt.Corrected, s.prompt.Issues = answer, writing.CheckPending, "", nil
	return true, nil
}

func (s *checkJobStore) CompleteCheck(_ context.Context, id, answer, corrected string, issues []protocol.Issue) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prompt.ID != id || s.prompt.Answer != answer || s.prompt.CheckStatus != writing.CheckPending {
		return false, nil
	}
	s.prompt.CheckStatus, s.prompt.Corrected, s.prompt.Issues = writing.CheckDone, corrected, issues
	return true, nil
}

func (s *checkJobStore) FailCheck(_ context.Context, id, answer string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prompt.ID != id || s.prompt.Answer != answer || s.prompt.CheckStatus != writing.CheckPending {
		return false, nil
	}
	s.prompt.CheckStatus = writing.CheckFailed
	return true, nil
}

func (s *checkJobStore) snapshot() writing.Prompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt
}

const checkCorrectionJSON = `{"corrected":"오늘은 비가 와요.","translation":"It rains today.","issues":[{"type":"grammar","span":"와","suggestion":"와요","explanation":"polite ending","explanationTranslation":"해요체"}]}`

func pendingCheckStore() *checkJobStore {
	return &checkJobStore{prompt: writing.Prompt{
		ID: "w1", UserID: "alex", Korean: "오늘은 비가 와요.", Status: writing.StatusDone,
		Answer: "오늘 비가 와", CheckStatus: writing.CheckPending,
	}}
}

func checkJobLLM(text string, after func()) *nuanceLLM {
	return &nuanceLLM{complete: func(context.Context) (string, error) {
		if after != nil {
			after()
		}
		return text, nil
	}}
}

// TestWritingCheckGradesAndDeduplicatesDispatches covers the detached job's
// happy path and the poll-collapse guarantee: several inline dispatches for
// one prompt (submit plus read-time redispatches, no Redis) run exactly one
// grading and land the parsed result on the stored answer.
func TestWritingCheckGradesAndDeduplicatesDispatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := pendingCheckStore()
		calls := 0
		model := &nuanceLLM{complete: func(context.Context) (string, error) {
			calls++
			return checkCorrectionJSON, nil
		}}
		pipe := &pipeline.Pipeline{LLM: model, ChatModel: "chat"}
		for range 3 {
			if err := DispatchWritingCheck(context.Background(), nil, pipe, st, "w1", "alex"); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("model calls = %d, want one deduped run", calls)
		}
		p := st.snapshot()
		if p.CheckStatus != writing.CheckDone || p.Corrected != "오늘은 비가 와요." || len(p.Issues) != 1 || p.Issues[0].Suggestion != "와요" {
			t.Fatalf("graded prompt = %+v", p)
		}
	})
}

func TestWritingCheckSkipsAnswersThatAreNotPending(t *testing.T) {
	for _, status := range []string{"", writing.CheckDone, writing.CheckFailed} {
		st := pendingCheckStore()
		st.prompt.CheckStatus = status
		calls := 0
		model := &nuanceLLM{complete: func(context.Context) (string, error) {
			calls++
			return checkCorrectionJSON, nil
		}}
		job := asyncjob.Job{Payload: mustPayload(writingCheckJobPayload{PromptID: "w1", UserID: "alex"})}
		if err := WritingCheckJobHandler(&pipeline.Pipeline{LLM: model, ChatModel: "chat"}, st)(context.Background(), job); err != nil {
			t.Fatalf("status %q: %v", status, err)
		}
		if calls != 0 {
			t.Fatalf("status %q: model called %d times", status, calls)
		}
	}
}

func TestWritingCheckFailureMarksStoredCheckFailed(t *testing.T) {
	st := pendingCheckStore()
	boom := errors.New("model down")
	model := &nuanceLLM{complete: func(context.Context) (string, error) { return "", boom }}
	job := asyncjob.Job{Payload: mustPayload(writingCheckJobPayload{PromptID: "w1", UserID: "alex"})}
	// The cascade's terminal error is its own aggregate once the draft stage
	// fails without a configured refinement model; what matters here is that
	// the failure surfaces and the stored check is marked failed.
	if err := WritingCheckJobHandler(&pipeline.Pipeline{LLM: model, ChatModel: "chat"}, st)(context.Background(), job); err == nil {
		t.Fatal("failed grading should surface an error")
	}
	if p := st.snapshot(); p.CheckStatus != writing.CheckFailed {
		t.Fatalf("prompt after failed grading = %+v", p)
	}
}

// TestWritingCheckDropsSupersededAnswer guards the fence: if the learner
// submits a new answer while an older grade is still being produced, the
// stale result must not overwrite the newer pending check — the row stays
// pending so the newer dispatch (or the read-time redispatch) converges it.
func TestWritingCheckDropsSupersededAnswer(t *testing.T) {
	st := pendingCheckStore()
	// The learner submits a replacement answer while this grade is running.
	model := checkJobLLM(checkCorrectionJSON, func() {
		if _, err := st.StartCheck(context.Background(), "alex", "w1", "오늘은 비가 와요"); err != nil {
			t.Error(err)
		}
	})
	job := asyncjob.Job{Payload: mustPayload(writingCheckJobPayload{PromptID: "w1", UserID: "alex"})}
	if err := WritingCheckJobHandler(&pipeline.Pipeline{LLM: model, ChatModel: "chat"}, st)(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	p := st.snapshot()
	if p.Answer != "오늘은 비가 와요" || p.CheckStatus != writing.CheckPending || p.Corrected != "" {
		t.Fatalf("stale result overwrote the newer submission: %+v", p)
	}
}

func TestWritingCheckDeletedOwnerDiscardsGrading(t *testing.T) {
	st := pendingCheckStore()
	st.deleted.Store(true)
	calls := 0
	model := &nuanceLLM{complete: func(context.Context) (string, error) {
		calls++
		return checkCorrectionJSON, nil
	}}
	err := runWritingCheck(context.Background(), &pipeline.Pipeline{LLM: model, ChatModel: "chat"}, st, "w1", "alex")
	if !errors.Is(err, workguard.ErrDeleted) || calls != 0 {
		t.Fatalf("deleted prompt error=%v model calls=%d", err, calls)
	}
	if p := st.snapshot(); p.CheckStatus != writing.CheckPending {
		t.Fatalf("deleted prompt status changed: %+v", p)
	}
}

// TestWritingCheckResumeGradesSnapshotInput proves the input snapshot: a
// reap-retry grades the exact (target, answer) pair captured before the
// first model call, even though the row was re-read after it changed.
func TestWritingCheckResumeGradesSnapshotInput(t *testing.T) {
	st := pendingCheckStore()
	var saved checkpointtest.Memory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var inputs []string
	model := &nuanceLLM{
		messages: func(msgs []llm.Message) { inputs = append(inputs, msgs[1].Content) },
		complete: func(ctx context.Context) (string, error) {
			if len(inputs) == 1 {
				cancel()
				return "", ctx.Err()
			}
			return checkCorrectionJSON, nil
		},
	}
	pipe := &pipeline.Pipeline{LLM: model, ChatModel: "chat"}
	job := asyncjob.Job{Payload: mustPayload(writingCheckJobPayload{PromptID: "w1", UserID: "alex"})}
	handler := WritingCheckJobHandler(pipe, st)
	if err := handler(checkpoint.Bind(ctx, &saved), job); err == nil {
		t.Fatal("interrupted attempt should surface its error")
	}
	// The prompt row's target changes before the retry (e.g. a failed
	// prompt re-generated); the check must stay on the snapshotted pair.
	st.mu.Lock()
	st.prompt.Korean = "내일은 눈이 와요."
	st.mu.Unlock()
	if err := handler(checkpoint.Bind(context.Background(), &saved), job); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || !strings.Contains(inputs[0], "오늘은 비가 와요.") || !strings.Contains(inputs[1], "오늘은 비가 와요.") {
		t.Fatalf("resumed check graded a different target: %q", inputs)
	}
	if strings.Contains(inputs[1], "내일은") {
		t.Fatalf("resumed check re-read the changed row: %q", inputs[1])
	}
	if p := st.snapshot(); p.CheckStatus != writing.CheckDone || p.Corrected != "오늘은 비가 와요." {
		t.Fatalf("resumed result = %+v", p)
	}
}
