package transport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

// TestRunStudySummarySkipsLLMWhenNoIssuesFlagged mirrors the old
// sessionStudySummaryHandler's "nothing to synthesize" guard, now enforced
// by runStudySummary itself: no LLM call, an empty wrap-up persisted via
// CompleteStudySummary, and no profile merge (there's nothing to fold in).
func TestRunStudySummarySkipsLLMWhenNoIssuesFlagged(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLM{&calls, "should not be called"}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-1", 1, "user", "I like pizza.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-1", 1, protocol.Correction{Original: "I like pizza.", Corrected: "I like pizza."}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-1"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudySummaryInline(context.Background(), pipe, st, "alex", "sess-1"); err != nil {
		t.Fatalf("RunStudySummaryInline() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call when no issues were flagged, got %d calls", calls)
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-1")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusDone || len(meta.StudySummary) != 0 {
		t.Fatalf("meta = %+v, want JobStatusDone with an empty summary", meta)
	}
	if got, _ := st.GetLearnerProfile(context.Background(), "alex"); got != "" {
		t.Fatalf("learner profile = %q, want untouched", got)
	}
}

// TestRunStudySummaryWaitsForPendingCorrection guards waitForPendingCorrections:
// a correction job still running for the session's last turn when the
// wrap-up starts must not be silently skipped — the summary must wait for
// it to land and then include it, not race ahead with a partial issue set
// (the exact race a learner ending mid-correction, manually or via an
// instant room's auto-finalize, would otherwise hit).
func TestRunStudySummaryWaitsForPendingCorrection(t *testing.T) {
	origInterval, origMaxWait := pendingCorrectionPollInterval, pendingCorrectionMaxWait
	pendingCorrectionPollInterval = 5 * time.Millisecond
	pendingCorrectionMaxWait = time.Second
	defer func() { pendingCorrectionPollInterval, pendingCorrectionMaxWait = origInterval, origMaxWait }()

	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"sentences":[{"english":"Watch your verb agreement.","translation":"동사 일치에 주의하세요."}]}`}}},
	}
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-wait", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, "alex", "sess-wait", 1); err != nil {
		t.Fatalf("ReserveCorrectionJob() error = %v", err)
	}
	if err := st.EndSession(ctx, "alex", "sess-wait"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = st.SaveCorrection(ctx, "alex", "sess-wait", 1, protocol.Correction{
			Original: "He go to school.", Corrected: "He goes to school.",
			Issues: []protocol.Issue{{Type: "grammar", Span: "He go", Suggestion: "He goes"}},
		})
	}()

	if err := RunStudySummaryInline(ctx, pipe, st, "alex", "sess-wait"); err != nil {
		t.Fatalf("RunStudySummaryInline() error = %v", err)
	}

	meta, _, err := st.SessionDetail(ctx, "alex", "sess-wait")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusDone || len(meta.StudySummary) == 0 {
		t.Fatalf("meta = %+v, want a non-empty summary once the pending correction landed", meta)
	}
}

// TestRunStudySummaryGivesUpAfterMaxWait guards the other half: a correction
// that never lands (e.g. its own job crashed and hasn't been reaped yet)
// must not wedge the wrap-up forever — waitForPendingCorrections gives up
// once pendingCorrectionMaxWait passes and the summary proceeds with
// whatever's already there rather than never completing.
func TestRunStudySummaryGivesUpAfterMaxWait(t *testing.T) {
	origInterval, origMaxWait := pendingCorrectionPollInterval, pendingCorrectionMaxWait
	pendingCorrectionPollInterval = 5 * time.Millisecond
	pendingCorrectionMaxWait = 20 * time.Millisecond
	defer func() { pendingCorrectionPollInterval, pendingCorrectionMaxWait = origInterval, origMaxWait }()

	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLM{&calls, "should not be called"}}},
	}
	st := newFakeStore()
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-timeout", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, "alex", "sess-timeout", 1); err != nil {
		t.Fatalf("ReserveCorrectionJob() error = %v", err)
	}
	if err := st.EndSession(ctx, "alex", "sess-timeout"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	// Deliberately never resolved: simulates a correction job that crashed
	// and hasn't been reaped yet.

	if err := RunStudySummaryInline(ctx, pipe, st, "alex", "sess-timeout"); err != nil {
		t.Fatalf("RunStudySummaryInline() error = %v", err)
	}

	meta, _, err := st.SessionDetail(ctx, "alex", "sess-timeout")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusDone || len(meta.StudySummary) != 0 {
		t.Fatalf("meta = %+v, want an empty-but-done summary once the wait gave up with no issues ever landing", meta)
	}
}

// TestRunStudySummaryCollectsIssuesAndMergesProfile guards the primary flow:
// every issue from every user turn's correction (not an assistant turn's)
// reaches GenerateStudySummary, the result is persisted as done, and it's
// folded into the learner's cross-session profile.
func TestRunStudySummaryCollectsIssuesAndMergesProfile(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"sentences":[{"english":"Focus on subject-verb agreement.","translation":"주어-동사 일치에 집중하세요."}]}`}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-2", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(context.Background(), "alex", "sess-2", 1, "assistant", "Nice!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-2", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.SaveLearnerProfile(context.Background(), "alex", "old profile"); err != nil {
		t.Fatalf("SaveLearnerProfile() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-2"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudySummaryInline(context.Background(), pipe, st, "alex", "sess-2"); err != nil {
		t.Fatalf("RunStudySummaryInline() error = %v", err)
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-2")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusDone {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusDone", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 1 || meta.StudySummary[0].English != "Focus on subject-verb agreement." {
		t.Fatalf("StudySummary = %+v", meta.StudySummary)
	}
	if got, _ := st.GetLearnerProfile(context.Background(), "alex"); got == "old profile" || got == "" {
		t.Fatalf("learner profile = %q, want it merged with the new wrap-up", got)
	}
}

// TestRunStudySummaryGenerateErrorMarksFailed guards the error path: a
// failed LLM call must not persist any summary, must record
// JobStatusFailed for a poller to show, and must propagate the error so
// asyncjob's reaper retries the job from scratch.
func TestRunStudySummaryGenerateErrorMarksFailed(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: failingAnalysisLLM{}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-3", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-3", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-3"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudySummaryInline(context.Background(), pipe, st, "alex", "sess-3"); err == nil {
		t.Fatalf("RunStudySummaryInline() error = nil, want the LLM error propagated")
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-3")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusFailed {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusFailed", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 0 {
		t.Fatalf("StudySummary = %+v, want still empty after a failed attempt", meta.StudySummary)
	}
}

// TestRunStudySummaryEmptyResultDespiteIssuesMarksFailed guards against the
// exact bug httpserver.sessionRestudyHandler's "다시 확인하기" button exists to
// recover from: GenerateStudySummary can return syntactically valid JSON
// with an empty "sentences" array even when real issues were flagged. That
// must not land as JobStatusDone — indistinguishable from a genuinely clean
// session (see CompleteStudySummary's doc comment) with no automatic way
// back, since needsStudySummaryBackfill only re-triggers a JobStatusPending
// row — so it has to be treated as a failure instead, letting the reaper
// retry it like any other transient error.
func TestRunStudySummaryEmptyResultDespiteIssuesMarksFailed(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"sentences":[]}`}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-empty", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-empty", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-empty"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudySummaryInline(context.Background(), pipe, st, "alex", "sess-empty"); err == nil {
		t.Fatalf("RunStudySummaryInline() error = nil, want an error for an empty result despite flagged issues")
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-empty")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusFailed {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusFailed, not JobStatusDone-with-nothing-to-show", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 0 {
		t.Fatalf("StudySummary = %+v, want still empty after a rejected empty result", meta.StudySummary)
	}
}

// TestRunStudySummaryProfileMergeFailureStillCompletes guards the
// best-effort contract carried over from the old sessionEndHandler: a
// transient failure enriching the learner's cross-session profile must not
// stop this session's own wrap-up from landing as done.
func TestRunStudySummaryProfileMergeFailureStillCompletes(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"sentences":[{"english":"Focus on subject-verb agreement.","translation":"주어-동사 일치에 집중하세요."}]}`}}},
	}
	st := &erroringProfileStore{fakeStore: newFakeStore()}
	if err := st.SaveTurn(context.Background(), "alex", "sess-4", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-4", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-4"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := RunStudySummaryInline(context.Background(), pipe, st, "alex", "sess-4"); err != nil {
		t.Fatalf("RunStudySummaryInline() error = %v, want nil even when the profile merge fails", err)
	}
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-4")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != store.JobStatusDone || len(meta.StudySummary) != 1 || meta.StudySummary[0].English != "Focus on subject-verb agreement." {
		t.Fatalf("meta = %+v, want the summary still persisted as done", meta)
	}
}

func TestStudySummaryJobHandlerBadPayload(t *testing.T) {
	handler := StudySummaryJobHandler(&pipeline.Pipeline{}, newFakeStore())
	job := asyncjob.Job{Kind: asyncjob.KindStudySummary, Payload: json.RawMessage(`not json`)}
	if err := handler(context.Background(), job); err == nil {
		t.Fatalf("handler(bad payload) error = nil, want an unmarshal error")
	}
}

// TestEnqueueStudySummaryJobRunsInBackgroundAndPersists exercises the full
// durable path against real Redis: Enqueue returns before the LLM call, and
// the result still lands, from the detached goroutine racing the pooled
// Worker to claim it (see EnqueueStudySummaryJob's doc comment) — the same
// "keeps going after the connection that started it is gone" guarantee
// every other asyncjob.Kind in this package gets.
func TestEnqueueStudySummaryJobRunsInBackgroundAndPersists(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"sentences":[{"english":"Focus on subject-verb agreement.","translation":"주어-동사 일치에 집중하세요."}]}`}}},
	}
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-enqueue", 1, "user", "He go to school.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveCorrection(context.Background(), "alex", "sess-enqueue", 1, protocol.Correction{
		Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
	}); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "sess-enqueue"); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}

	if err := EnqueueStudySummaryJob(context.Background(), queue, pipe, st, "alex", "sess-enqueue"); err != nil {
		t.Fatalf("EnqueueStudySummaryJob() error = %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-enqueue")
		return err == nil && meta.StudySummaryStatus == store.JobStatusDone
	})
	meta, _, err := st.SessionDetail(context.Background(), "alex", "sess-enqueue")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(meta.StudySummary) != 1 || meta.StudySummary[0].English != "Focus on subject-verb agreement." {
		t.Fatalf("StudySummary = %+v", meta.StudySummary)
	}
}

// ---- test doubles ---------------------------------------------------------

// countingLLM increments *calls on every Complete call, for guarding "the
// LLM must never be called" assertions without caring what it would have
// returned.
type countingLLM struct {
	calls *int
	reply string
}

func (c countingLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}
func (c countingLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	*c.calls++
	return c.reply, nil
}

// erroringProfileStore wraps *fakeStore, overriding the learner-profile
// calls to fail — for exercising runStudySummary's best-effort profile
// merge without adding error-injection fields to the shared fakeStore every
// other transport test file also uses.
type erroringProfileStore struct {
	*fakeStore
}

func (e *erroringProfileStore) GetLearnerProfile(ctx context.Context, userID string) (string, error) {
	return "", errors.New("profile store down")
}

func (e *erroringProfileStore) SaveLearnerProfile(ctx context.Context, userID, profile string) error {
	return errors.New("profile store down")
}

// ---- test helpers ---------------------------------------------------------

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}
