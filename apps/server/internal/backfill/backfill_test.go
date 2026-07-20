package backfill

import (
	"context"
	"encoding/json"
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

// sharedRedis backs every test in this file, started once in TestMain rather
// than per-test — see internal/identity/cached_oidc_test.go for the same
// reasoning (container startup dominates test time; each test uses its own
// keys so isolation doesn't need a fresh container).
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
	// Each test gets a clean slate for the fixed keys this package uses.
	if err := sharedRedis.Del(context.Background(), queueKey, queuedSet, processingKey, lockKey).Err(); err != nil {
		t.Fatalf("clean redis state: %v", err)
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
// internal/store's own container-backed tests; this only needs SessionDetail
// and SaveTranslation for the Worker to exercise.
type fakeStore struct {
	mu    sync.Mutex
	turns map[string][]store.Turn // "userID/sessionID" -> turns, in SessionDetail order
	saved []savedTranslation
}

type savedTranslation struct {
	userID, sessionID string
	turn              int
	role, translation string
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

func (f *fakeStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	return errors.New("not used by these tests")
}
func (f *fakeStore) Close() error { return nil }

// ---- Queue -----------------------------------------------------------------

func TestQueueEnqueueDedupesAlreadyQueuedSession(t *testing.T) {
	rdb := requireRedis(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	q.Enqueue(ctx, "alex", "sess-1")
	q.Enqueue(ctx, "alex", "sess-1")

	n, err := rdb.LLen(ctx, queueKey).Result()
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

	n, err := rdb.LLen(ctx, queueKey).Result()
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

// ---- Worker ------------------------------------------------------------------

func newTestPipeline(complete func(msgs []llm.Message) (string, error)) *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Analysis:     []pipeline.Candidate{{Model: "m", LLM: &fakeLLM{complete: complete}}},
		FeedbackLang: "ko",
	}
}

func TestWorkerTranslatesMissingTurnsWithAccumulatingContext(t *testing.T) {
	rdb := requireRedis(t)
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

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx)

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

	// Queue and dedupe set should both be empty after a full drain.
	if n, _ := rdb.LLen(ctx, queueKey).Result(); n != 0 {
		t.Fatalf("queue should be drained, length = %d", n)
	}
	if n, _ := rdb.SCard(ctx, queuedSet).Result(); n != 0 {
		t.Fatalf("dedupe set should be cleared, size = %d", n)
	}
}

func TestWorkerSkipsTurnsThatAlreadyHaveATranslationButStillUsesThemAsContext(t *testing.T) {
	rdb := requireRedis(t)
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

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx)

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

func TestWorkerContinuesAfterAPerTurnTranslateError(t *testing.T) {
	rdb := requireRedis(t)
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

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx)

	if len(st.saved) != 1 || st.saved[0].turn != 2 {
		t.Fatalf("saved translations = %+v, want only turn 2 (turn 1 failed)", st.saved)
	}
}

func TestWorkerDrainOnceIsNoopWhenLockAlreadyHeld(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	if err := rdb.SetNX(ctx, lockKey, "someone-else", time.Minute).Err(); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		return "번역", nil
	})
	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{{Turn: 1, Role: "user", Text: "hi"}})

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx)

	if calls != 0 {
		t.Fatalf("LLM called %d times, want 0 — the lock is held by someone else", calls)
	}
	if n, _ := rdb.LLen(ctx, queueKey).Result(); n != 1 {
		t.Fatalf("queued job should be untouched while locked out, length = %d", n)
	}
}

func TestWorkerDrainOnceReleasesLockAfterDraining(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) { return "번역", nil })
	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{{Turn: 1, Role: "user", Text: "hi"}})

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx)

	exists, err := rdb.Exists(ctx, lockKey).Result()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists != 0 {
		t.Fatalf("lock should be released once the drain pass finishes, so the LLM is immediately free for other work")
	}
}

func TestWorkerDrainOnceClearsProcessingClaimAfterSuccess(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) { return "번역", nil })
	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{{Turn: 1, Role: "user", Text: "hi"}})

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx)

	if n, _ := rdb.ZCard(ctx, processingKey).Result(); n != 0 {
		t.Fatalf("processing set should be cleared once the job finishes normally, size = %d", n)
	}
}

// TestReapStaleJobsRecoversFromCrashedWorker guards the fix for a real bug: a
// worker that dies between claiming a job (LPop out of queueKey, ZAdd into
// processingKey) and finishing it (SRem out of queuedSet) used to leave that
// session's dedupeKey stuck in queuedSet forever — every future Enqueue for
// it would see SAdd return 0 ("already queued") and silently no-op, so a
// session whose backfill worker happened to crash mid-translation could never
// be retried again, no matter how many times the learner reopened it. This
// reproduces exactly that crashed state (queued but neither in queueKey nor
// recently claimed) and asserts drainOnce's reapStaleJobs step recovers it.
func TestReapStaleJobsRecoversFromCrashedWorker(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		return "번역", nil
	})
	st := newFakeStore()
	st.seed("alex", "sess-1", []store.Turn{{Turn: 1, Role: "user", Text: "hi"}})

	j := job{UserID: "alex", SessionID: "sess-1"}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if err := rdb.SAdd(ctx, queuedSet, j.dedupeKey()).Err(); err != nil {
		t.Fatalf("seed queuedSet: %v", err)
	}
	staleClaim := time.Now().Add(-processingStaleThreshold - time.Minute).Unix()
	if err := rdb.ZAdd(ctx, processingKey, redis.Z{Score: float64(staleClaim), Member: raw}).Err(); err != nil {
		t.Fatalf("seed processingKey: %v", err)
	}

	// Confirm the stuck state actually behaves as described: a fresh Enqueue
	// silently no-ops because the dedupe key is still held.
	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-1")
	if n, _ := rdb.LLen(ctx, queueKey).Result(); n != 0 {
		t.Fatalf("Enqueue should still no-op while the dedupe key is held, queue length = %d", n)
	}

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx) // reapStaleJobs should recover the stuck job and process it

	if calls != 1 {
		t.Fatalf("LLM called %d times, want 1 (the reaped job should have been translated)", calls)
	}
	if len(st.saved) != 1 || st.saved[0].turn != 1 {
		t.Fatalf("saved translations = %+v, want turn 1 translated", st.saved)
	}
	if n, _ := rdb.LLen(ctx, queueKey).Result(); n != 0 {
		t.Fatalf("queue should be drained after reap+process, length = %d", n)
	}
	if n, _ := rdb.SCard(ctx, queuedSet).Result(); n != 0 {
		t.Fatalf("dedupe set should be cleared after reap+process, size = %d", n)
	}
	if n, _ := rdb.ZCard(ctx, processingKey).Result(); n != 0 {
		t.Fatalf("processing set should be cleared after reap+process, size = %d", n)
	}
}

// TestReapStaleJobsLeavesFreshClaimsAlone guards against reaping a job that's
// merely being translated slowly right now (well within
// processingStaleThreshold) — only claims older than the threshold should be
// touched.
func TestReapStaleJobsLeavesFreshClaimsAlone(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	j := job{UserID: "alex", SessionID: "sess-1"}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if err := rdb.ZAdd(ctx, processingKey, redis.Z{Score: float64(time.Now().Unix()), Member: raw}).Err(); err != nil {
		t.Fatalf("seed processingKey: %v", err)
	}

	w := NewWorker(rdb, newFakeStore(), newTestPipeline(nil))
	w.reapStaleJobs(ctx)

	if n, _ := rdb.ZCard(ctx, processingKey).Result(); n != 1 {
		t.Fatalf("fresh claim should be left alone, processing set size = %d", n)
	}
	if n, _ := rdb.LLen(ctx, queueKey).Result(); n != 0 {
		t.Fatalf("fresh claim must not be requeued, queue length = %d", n)
	}
}

func TestWorkerDrainOnceSkipsSessionMissingFromStore(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()

	var calls int
	pipe := newTestPipeline(func(msgs []llm.Message) (string, error) {
		calls++
		return "번역", nil
	})
	st := newFakeStore() // nothing seeded — SessionDetail returns ErrNotFound

	q := NewQueue(rdb)
	q.Enqueue(ctx, "alex", "sess-gone")

	w := NewWorker(rdb, st, pipe)
	w.drainOnce(ctx) // must not panic or hang

	if calls != 0 {
		t.Fatalf("LLM called %d times, want 0", calls)
	}
}
