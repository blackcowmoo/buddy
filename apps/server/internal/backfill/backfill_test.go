package backfill

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

// Generic queue-mechanics behavior (concurrent claiming across replicas,
// crash-mid-job recovery via the stale-claim reaper, idempotent
// completion) is exercised once, at the shared-infrastructure level, in
// internal/asyncjob's own tests — it applies identically to every job kind,
// translation included, so it isn't re-tested per kind here. This file only
// covers what's specific to this package: the Queue/Enqueue dedupe wrapper,
// and translateSession's translation logic.

// sharedRedis backs every test in this file, started once in TestMain
// rather than per-test (container startup dominates test time; each test
// uses its own dedupe keys so isolation doesn't need a fresh container).
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runBackfillTests(m))
}

func runBackfillTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		// No/unreachable Docker: skip container-backed tests, but still run
		// any pure-function ones.
		sharedRedisErr = err
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		sharedRedisErr = err
		return m.Run()
	}
	opts, err := redis.ParseURL(connStr)
	if err != nil {
		sharedRedisErr = err
		return m.Run()
	}
	sharedRedis = redis.NewClient(opts)
	defer func() { _ = sharedRedis.Close() }()

	return m.Run()
}

func requireRedis(t *testing.T) *redis.Client {
	t.Helper()
	if sharedRedisErr != nil {
		t.Skipf("redis testcontainer unavailable (no/unreachable Docker?): %v", sharedRedisErr)
	}
	if err := sharedRedis.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("flush redis state: %v", err)
	}
	return sharedRedis
}

// fakeLLM is a minimal deterministic llm.Client double, mirroring
// internal/pipeline's own unexported test double (not reusable across
// packages) — only Complete is exercised here since TranslateWithContext
// never streams.
type fakeLLM struct {
	mu       sync.Mutex
	complete func(msgs []llm.Message) (string, error)
}

func (f *fakeLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.complete(msgs)
}

// fakeStore is an in-memory store.Store — real SQL correctness is covered by
// internal/store's own container-backed tests; this only needs SessionDetail,
// SaveTranslation, and SaveCorrection for translateSession/correctSession to
// exercise.
type fakeStore struct {
	mu               sync.Mutex
	turns            map[string][]store.Turn // "userID/sessionID" -> turns, in SessionDetail order
	saved            []savedTranslation
	savedCorrections []savedCorrection
}

type savedTranslation struct {
	userID, sessionID string
	turn              int
	role, translation string
}

type savedCorrection struct {
	userID, sessionID string
	turn              int
	correction        protocol.Correction
}

func newFakeStore() *fakeStore {
	return &fakeStore{turns: map[string][]store.Turn{}}
}

func (f *fakeStore) key(userID, sessionID string) string { return userID + "/" + sessionID }

func (f *fakeStore) seed(userID, sessionID string, turns []store.Turn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns[f.key(userID, sessionID)] = turns
}

func (f *fakeStore) Load(ctx context.Context, userID, sessionID string) (store.Profile, error) {
	return store.Profile{}, errors.New("not used by these tests")
}
func (f *fakeStore) Save(ctx context.Context, userID, sessionID string, p store.Profile) error {
	return errors.New("not used by these tests")
}
func (f *fakeStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error {
	return errors.New("not used by these tests")
}
func (f *fakeStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedCorrections = append(f.savedCorrections, savedCorrection{userID, sessionID, turn, c})
	turns := f.turns[f.key(userID, sessionID)]
	for i := range turns {
		if turns[i].Turn == turn && turns[i].Role == "user" {
			turns[i].Correction = &c
		}
	}
	return nil
}

func (f *fakeStore) ReserveCorrectionJob(ctx context.Context, userID, sessionID string, turn int) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) SaveTranslation(ctx context.Context, userID, sessionID string, turn int, role, translation string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, savedTranslation{userID, sessionID, turn, role, translation})
	turns := f.turns[f.key(userID, sessionID)]
	for i := range turns {
		if turns[i].Turn == turn && turns[i].Role == role {
			turns[i].Translation = translation
		}
	}
	return nil
}

func (f *fakeStore) SaveGeneratedTitle(ctx context.Context, userID, sessionID, title string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) ReserveAssistantTurn(ctx context.Context, userID, sessionID string, turn int) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) CompleteAssistantTurn(ctx context.Context, userID, sessionID string, turn int, text string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) FailJob(ctx context.Context, userID, sessionID string, turn int, kind, errMsg string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) JobStatus(ctx context.Context, userID, sessionID string, turn int, kind string) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeStore) AssistantTurnText(ctx context.Context, userID, sessionID string, turn int) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeStore) LastTurn(ctx context.Context, userID, sessionID string) (int, error) {
	return 0, errors.New("not used by these tests")
}

func (f *fakeStore) ListSessions(ctx context.Context, userID string) ([]store.SessionMeta, error) {
	return nil, errors.New("not used by these tests")
}

func (f *fakeStore) SessionDetail(ctx context.Context, userID, sessionID string) (store.SessionMeta, []store.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	turns, ok := f.turns[f.key(userID, sessionID)]
	if !ok {
		return store.SessionMeta{}, nil, store.ErrNotFound
	}
	out := make([]store.Turn, len(turns))
	copy(out, turns)
	return store.SessionMeta{ID: sessionID}, out, nil
}

func (f *fakeStore) SessionDetailPage(ctx context.Context, userID, sessionID string, beforeTurn, limit int) (store.SessionMeta, []store.Turn, bool, error) {
	return store.SessionMeta{}, nil, false, errors.New("not used by these tests")
}

func (f *fakeStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) GetInterlocutorStyle(ctx context.Context, userID string) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeStore) SaveInterlocutorStyle(ctx context.Context, userID, style string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) EndSession(ctx context.Context, userID, sessionID string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) CompleteStudySummary(ctx context.Context, userID, sessionID string, summary []protocol.StudySummarySentence) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) FailStudySummary(ctx context.Context, userID, sessionID string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) RestartStudySummary(ctx context.Context, userID, sessionID string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) GetLearnerProfile(ctx context.Context, userID string) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeStore) SaveLearnerProfile(ctx context.Context, userID, profile string) error {
	return errors.New("not used by these tests")
}

func (f *fakeStore) Close() error { return nil }

func newTestPipeline(complete func(msgs []llm.Message) (string, error)) *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Analysis:     []pipeline.Candidate{{Model: "m", LLM: &fakeLLM{complete: complete}}},
		FeedbackLang: "ko",
	}
}

// ---- Queue ------------------------------------------------------------

func TestQueueEnqueueDedupesAlreadyQueuedSession(t *testing.T) {
	rdb := requireRedis(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	q.Enqueue(ctx, "alex", "sess-1")
	q.Enqueue(ctx, "alex", "sess-1")

	n, err := rdb.LLen(ctx, "buddy:job:{translation}:queue").Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 1 {
		t.Fatalf("queue length = %d, want 1 (second Enqueue should have deduped)", n)
	}
}

func TestQueueEnqueueAllowsDistinctSessions(t *testing.T) {
	rdb := requireRedis(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	q.Enqueue(ctx, "alex", "sess-1")
	q.Enqueue(ctx, "alex", "sess-2")
	q.Enqueue(ctx, "casey", "sess-1") // same session ID, different user — must not dedupe against alex's

	n, err := rdb.LLen(ctx, "buddy:job:{translation}:queue").Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 3 {
		t.Fatalf("queue length = %d, want 3", n)
	}
}

func TestQueueEnqueueNilQueueIsNoop(t *testing.T) {
	var q *Queue
	q.Enqueue(context.Background(), "alex", "sess-1") // must not panic
}

// ---- translateSession --------------------------------------------------

func TestTranslateSessionTranslatesMissingTurnsWithAccumulatingContext(t *testing.T) {
	ctx := context.Background()

	var seenInputs []string
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		in := msgs[len(msgs)-1].Content
		seenInputs = append(seenInputs, in)
		switch {
		// Turn 2's input also contains turn 1's text as context, so the
		// more specific "text under translation" check must come first.
		case strings.Contains(in, "Text to translate:\nIt sleeps a lot."):
			return "많이 자요.", nil
		case strings.Contains(in, "I have a cat."):
			return "고양이가 있어요.", nil
		}
		return "번역", nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "user", Text: "I have a cat."},
		{Turn: 1, Role: "assistant", Text: "Nice!"},
		{Turn: 2, Role: "user", Text: "It sleeps a lot."},
	})

	translateSession(ctx, st, pipe, "alex", "sess-1")

	if len(st.saved) != 3 {
		t.Fatalf("saved %d translations, want 3: %+v", len(st.saved), st.saved)
	}
	if st.saved[0].turn != 1 || st.saved[0].role != "user" || st.saved[0].translation != "고양이가 있어요." {
		t.Fatalf("first saved translation wrong: %+v", st.saved[0])
	}
	if st.saved[2].turn != 2 || st.saved[2].translation != "많이 자요." {
		t.Fatalf("third saved translation wrong: %+v", st.saved[2])
	}

	// The second turn's translation input must include the first turn's
	// original text as context — otherwise "It sleeps a lot." can't be
	// disambiguated (what sleeps?).
	if !strings.Contains(seenInputs[2], "I have a cat.") {
		t.Fatalf("turn 2's translation input should include turn 1 as context, got %q", seenInputs[2])
	}
}

func TestTranslateSessionSkipsTurnsThatAlreadyHaveATranslationButStillUsesThemAsContext(t *testing.T) {
	ctx := context.Background()

	var calls int
	var lastInput string
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		lastInput = msgs[len(msgs)-1].Content
		return "번역됨", nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "user", Text: "I have a cat.", Translation: "고양이가 있어요."},
		{Turn: 2, Role: "user", Text: "It sleeps a lot."}, // missing
	})

	translateSession(ctx, st, pipe, "alex", "sess-1")

	if calls != 1 {
		t.Fatalf("LLM called %d times, want 1 (turn 1 already has a translation)", calls)
	}
	if len(st.saved) != 1 || st.saved[0].turn != 2 {
		t.Fatalf("saved translations = %+v, want exactly turn 2", st.saved)
	}
	if !strings.Contains(lastInput, "I have a cat.") {
		t.Fatalf("already-translated turn 1 should still be used as context, got %q", lastInput)
	}
}

func TestTranslateSessionContinuesAfterAPerTurnTranslateError(t *testing.T) {
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("llm down")
		}
		return "번역", nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "user", Text: "first"},
		{Turn: 2, Role: "user", Text: "second"},
	})

	translateSession(ctx, st, pipe, "alex", "sess-1")

	if len(st.saved) != 1 || st.saved[0].turn != 2 {
		t.Fatalf("saved translations = %+v, want only turn 2 (turn 1 failed)", st.saved)
	}
}

func TestTranslateSessionSkipsSessionMissingFromStore(t *testing.T) {
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		return "번역", nil
	})
	st := newFakeStore() // nothing seeded — SessionDetail returns ErrNotFound

	translateSession(ctx, st, pipe, "alex", "sess-gone") // must not panic

	if calls != 0 {
		t.Fatalf("LLM called %d times, want 0", calls)
	}
}

// ---- CorrectionQueue ----------------------------------------------------

func TestCorrectionQueueEnqueueDedupesAlreadyQueuedSession(t *testing.T) {
	rdb := requireRedis(t)
	q := NewCorrectionQueue(rdb)
	ctx := context.Background()

	q.Enqueue(ctx, "alex", "sess-1")
	q.Enqueue(ctx, "alex", "sess-1")

	n, err := rdb.LLen(ctx, "buddy:job:{correction-backfill}:queue").Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 1 {
		t.Fatalf("queue length = %d, want 1 (second Enqueue should have deduped)", n)
	}
}

func TestCorrectionQueueEnqueueNilQueueIsNoop(t *testing.T) {
	var q *CorrectionQueue
	q.Enqueue(context.Background(), "alex", "sess-1") // must not panic
}

// correctionJSON builds the strict-JSON shape pipeline.parseCorrection
// expects from a fake analysis-ensemble response.
func correctionJSON(corrected string, translation string) string {
	return `{"corrected":"` + corrected + `","translation":"` + translation + `","issues":[]}`
}

// ---- correctSession -----------------------------------------------------

func TestCorrectSessionCorrectsUntrackedTurnsWithAccumulatingContext(t *testing.T) {
	ctx := context.Background()

	var seenInputs []string
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		in := msgs[len(msgs)-1].Content
		seenInputs = append(seenInputs, in)
		switch {
		case strings.Contains(in, "Sentence to correct:\nIt run fast."):
			return correctionJSON("It runs fast.", "빨리 뛴다."), nil
		case strings.Contains(in, "I has a dog."):
			return correctionJSON("I have a dog.", "개가 있어요."), nil
		}
		return correctionJSON("?", "?"), nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "user", Text: "I has a dog."},
		{Turn: 1, Role: "assistant", Text: "Nice!"},
		{Turn: 2, Role: "user", Text: "It run fast."},
	})

	correctSession(ctx, st, pipe, "alex", "sess-1")

	if len(st.savedCorrections) != 2 {
		t.Fatalf("saved %d corrections, want 2: %+v", len(st.savedCorrections), st.savedCorrections)
	}
	if st.savedCorrections[0].turn != 1 || st.savedCorrections[0].correction.Corrected != "I have a dog." {
		t.Fatalf("first saved correction wrong: %+v", st.savedCorrections[0])
	}
	if st.savedCorrections[1].turn != 2 || st.savedCorrections[1].correction.Corrected != "It runs fast." {
		t.Fatalf("second saved correction wrong: %+v", st.savedCorrections[1])
	}
	// Turn 2's correction input must include turn 1's original text as
	// context, the same "conversation so far" shape correct() uses live.
	if !strings.Contains(seenInputs[1], "I has a dog.") {
		t.Fatalf("turn 2's correction input should include turn 1 as context, got %q", seenInputs[1])
	}
	// AnalyzeCorrection's own translation output also gets persisted, same
	// as CorrectionJobHandler does for the live path.
	if len(st.saved) != 2 || st.saved[0].translation != "개가 있어요." || st.saved[1].translation != "빨리 뛴다." {
		t.Fatalf("saved translations = %+v", st.saved)
	}
}

func TestCorrectSessionSkipsTurnsThatAlreadyHaveACorrectionOrStatusButStillUsesThemAsContext(t *testing.T) {
	ctx := context.Background()

	var calls int
	var lastInput string
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		lastInput = msgs[len(msgs)-1].Content
		return correctionJSON("It runs fast.", "빨리 뛴다."), nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "user", Text: "I has a dog.", Correction: &protocol.Correction{Original: "I has a dog.", Corrected: "I have a dog."}},
		{Turn: 2, Role: "user", Text: "already flagged as failed", CorrectionStatus: "failed"}, // tracked by the live job's own reaper
		{Turn: 3, Role: "user", Text: "It run fast."},                                          // untracked — needs backfill
	})

	correctSession(ctx, st, pipe, "alex", "sess-1")

	if calls != 1 {
		t.Fatalf("LLM called %d times, want 1 (turns 1 and 2 must not be re-corrected)", calls)
	}
	if len(st.savedCorrections) != 1 || st.savedCorrections[0].turn != 3 {
		t.Fatalf("saved corrections = %+v, want exactly turn 3", st.savedCorrections)
	}
	if !strings.Contains(lastInput, "I has a dog.") {
		t.Fatalf("already-corrected turn 1 should still be used as context, got %q", lastInput)
	}
}

func TestCorrectSessionOnlyConsidersUserTurns(t *testing.T) {
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		return correctionJSON("fine", "번역"), nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "assistant", Text: "an assistant turn, never correction-tracked"},
	})

	correctSession(ctx, st, pipe, "alex", "sess-1")

	if calls != 0 {
		t.Fatalf("LLM called %d times, want 0 (assistant turns are never corrected)", calls)
	}
}

func TestCorrectSessionContinuesAfterAPerTurnCorrectError(t *testing.T) {
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("llm down")
		}
		return correctionJSON("second, fixed", "번역"), nil
	})

	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{
		{Turn: 1, Role: "user", Text: "first"},
		{Turn: 2, Role: "user", Text: "second"},
	})

	correctSession(ctx, st, pipe, "alex", "sess-1")

	if len(st.savedCorrections) != 1 || st.savedCorrections[0].turn != 2 {
		t.Fatalf("saved corrections = %+v, want only turn 2 (turn 1 failed)", st.savedCorrections)
	}
}

func TestCorrectSessionSkipsSessionMissingFromStore(t *testing.T) {
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		return correctionJSON("x", "y"), nil
	})
	st := newFakeStore() // nothing seeded — SessionDetail returns ErrNotFound

	correctSession(ctx, st, pipe, "alex", "sess-gone") // must not panic

	if calls != 0 {
		t.Fatalf("LLM called %d times, want 0", calls)
	}
}

// ---- end-to-end: CorrectionQueue -> CorrectionWorker wiring --------------

func TestCorrectionQueueAndWorkerEndToEnd(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		return correctionJSON("fixed.", "번역"), nil
	})
	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{{Turn: 1, Role: "user", Text: "hi"}})

	q := NewCorrectionQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewCorrectionWorker(rdb, st, pipe)
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.savedCorrections)
		st.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("correction not saved within deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if st.savedCorrections[0].turn != 1 || st.savedCorrections[0].correction.Corrected != "fixed." {
		t.Fatalf("saved correction wrong: %+v", st.savedCorrections[0])
	}
}

// ---- end-to-end: Queue -> Worker wiring --------------------------------

// TestQueueAndWorkerEndToEnd is a smoke test of the full path — Enqueue
// through a real Redis, a Worker draining it, translateSession running,
// and the result landing in the store — to confirm this package's
// asyncjob.KindTranslation wiring is correct. Queue-mechanics edge cases
// (concurrent claims, crash recovery, idempotent completion) are the
// asyncjob package's responsibility and are covered there, not repeated
// here.
func TestQueueAndWorkerEndToEnd(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) { return "번역", nil })
	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{{Turn: 1, Role: "user", Text: "hi"}})

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.saved)
		st.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("translation not saved within deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if st.saved[0].turn != 1 || st.saved[0].translation != "번역" {
		t.Fatalf("saved translation wrong: %+v", st.saved[0])
	}
}
