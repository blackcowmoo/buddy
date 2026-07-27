package transport

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/stt"
)

// sharedRedis backs every test in this file, started once in TestMain rather
// than per-test — see internal/asyncjob/asyncjob_test.go for the same
// reasoning. Each test uses its own userID/sessionID (and so its own Redis
// dedupe/queue keys, namespaced by Kind), so isolation doesn't need a fresh
// container per test.
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runReplyJobTests(m))
}

func runReplyJobTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
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

func requireReplyRedis(t *testing.T) *redis.Client {
	t.Helper()
	if sharedRedisErr != nil {
		t.Skipf("redis testcontainer unavailable (no/unreachable Docker?): %v", sharedRedisErr)
	}
	return sharedRedis
}

// gateChatLLM is a chat-model double whose ChatStream call blocks until
// release is closed (or ctx cancels first) — used to pin a reply job in
// flight so a test can disconnect its WS client, or otherwise let time
// pass, before the reply actually finishes, and confirm it still completes
// and persists regardless.
type gateChatLLM struct {
	started chan struct{}
	release chan struct{}
	reply   string
	once    sync.Once
}

func newGateChatLLM(reply string) *gateChatLLM {
	return &gateChatLLM{started: make(chan struct{}), release: make(chan struct{}), reply: reply}
}

func (g *gateChatLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		if onToken != nil {
			onToken(g.reply)
		}
		return g.reply, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (g *gateChatLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return "", errFakeLLMUnavailable
}

func waitForJobStatusDone(t *testing.T, st *fakeStore, userID, sessionID string, turn int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := st.JobStatus(context.Background(), userID, sessionID, turn, "reply")
		if err != nil {
			t.Fatalf("JobStatus() error = %v", err)
		}
		if status == "done" {
			_, turns, err := st.SessionDetail(context.Background(), userID, sessionID)
			if err != nil {
				t.Fatalf("SessionDetail() error = %v", err)
			}
			for _, tn := range turns {
				if tn.Turn == turn && tn.Role == "assistant" {
					return tn.Text
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reply job for turn %d did not complete within %s", turn, timeout)
	return ""
}

// ---- NewReplyHook -----------------------------------------------------

func TestNewReplyHookReturnsNilWithoutQueue(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	if hook := NewReplyHook(pipe, newFakeStore(), nil); hook != nil {
		t.Fatalf("NewReplyHook(nil queue) = %v, want nil (so reply()/StartConversation fall back to the direct in-process path)", hook)
	}
}

func TestReplyHookFastPathStreamsTokensAndCompletesInline(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	pipe := &pipeline.Pipeline{LLM: fixedChatLLM{reply: "hi there"}, ChatModel: "m"}
	hook := NewReplyHook(pipe, st, queue)

	var tokens []string
	var done string
	var doneCalled bool
	hook(context.Background(), "alex", "sess-fastpath", 1, []llm.Message{{Role: llm.RoleUser, Content: "hello"}}, "fallback",
		func(tok string) { tokens = append(tokens, tok) },
		func(full string) { done = full; doneCalled = true },
	)

	if !doneCalled {
		t.Fatalf("onDone was never called")
	}
	if done != "hi there" {
		t.Fatalf("onDone text = %q, want %q", done, "hi there")
	}
	if len(tokens) != 1 || tokens[0] != "hi there" {
		t.Fatalf("onToken calls = %+v, want a single token with the reply text", tokens)
	}
	status, err := st.JobStatus(context.Background(), "alex", "sess-fastpath", 1, "reply")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != "done" {
		t.Fatalf("JobStatus() = %q, want done", status)
	}
}

// TestReplyHookSurvivesCallerContextCancellation is the core requirement
// this whole feature exists for: a reply already in flight when the
// learner's connection disconnects (ctx canceled) must still finish and
// persist, not be dropped like the old direct-call path did.
func TestReplyHookSurvivesCallerContextCancellation(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	gate := newGateChatLLM("finished despite disconnect")
	pipe := &pipeline.Pipeline{LLM: gate, ChatModel: "m"}
	hook := NewReplyHook(pipe, st, queue)

	// Mirrors production: the user's own turn is always persisted (which is
	// what creates the session row — see store.MySQLStore.SaveTurn) before
	// its reply job is ever enqueued.
	if err := st.SaveTurn(context.Background(), "alex", "sess-disconnect", 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go hook(ctx, "alex", "sess-disconnect", 1, []llm.Message{{Role: llm.RoleUser, Content: "hello"}}, "fallback",
		func(string) {},
		func(string) {},
	)

	select {
	case <-gate.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("ChatStream was never called")
	}
	// Simulate the learner's connection disconnecting while the reply is
	// still streaming.
	cancel()
	time.Sleep(50 * time.Millisecond) // give a (buggy) ctx-tied implementation a chance to abort
	close(gate.release)               // now let the "LLM" actually finish

	text := waitForJobStatusDone(t, st, "alex", "sess-disconnect", 1, 3*time.Second)
	if text != "finished despite disconnect" {
		t.Fatalf("persisted reply text = %q, want the full reply despite the caller's ctx being canceled", text)
	}
}

// TestReplyJobHandlerCompletesViaBackgroundWorkerPool covers the path a job
// takes after a crashed replica's claim is recovered: a plain background
// asyncjob.Worker (not the connection that created the job — see
// TestReplyHookFastPathStreamsTokensAndCompletesInline for that path) picks
// it up from the queue and completes it. This is exactly the pool every
// replica runs in cmd/server/main.go, and what asyncjob's own stale-claim
// reaper (tested exhaustively in internal/asyncjob) hands a job back to
// after its original owner dies mid-generation — that reap mechanism
// itself isn't re-tested here, only that ReplyJobHandler behaves correctly
// when driven by a Worker instead of the inline fast path.
func TestReplyJobHandlerCompletesViaBackgroundWorkerPool(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	pipe := &pipeline.Pipeline{LLM: fixedChatLLM{reply: "recovered reply"}, ChatModel: "m"}

	if err := st.SaveTurn(context.Background(), "alex", "sess-crash", 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveAssistantTurn(context.Background(), "alex", "sess-crash", 1); err != nil {
		t.Fatalf("ReserveAssistantTurn() error = %v", err)
	}
	payload := replyJobPayload{
		UserID: "alex", SessionID: "sess-crash", Turn: 1,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}, Fallback: "fallback",
	}
	// A test-local, unique-per-run Kind (not the shared asyncjob.KindReply
	// every other test in this file's inline fast path also enqueues onto,
	// and not just t.Name() — this test's background Worker can stay
	// blocked in a claim call for a few seconds after the test function
	// returns, so a repeated run (`go test -count`) needs its own kind too,
	// not just isolation from other tests) so it can never race another
	// run's job for the same queue.
	kind := asyncjob.Kind(t.Name() + "-" + uuid.New().String())
	if _, ok, err := queue.Enqueue(context.Background(), kind, turnKey("alex", "sess-crash", 1), payload); err != nil || !ok {
		t.Fatalf("Enqueue: ok=%v err=%v", ok, err)
	}

	worker := asyncjob.NewWorker(rdb, kind, 2, ReplyClaimTTL, ReplyJobHandler(pipe, st, nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go worker.Run(ctx)

	text := waitForJobStatusDone(t, st, "alex", "sess-crash", 1, 4*time.Second)
	if text != "recovered reply" {
		t.Fatalf("recovered reply text = %q, want %q", text, "recovered reply")
	}
}

// TestReplyJobHandlerSkipsAlreadyDoneJob guards the idempotency contract
// asyncjob.Handler requires: a job handed to the handler twice (e.g. a
// spurious extra reap) must not call the LLM a second time once the turn
// is already marked done.
func TestReplyJobHandlerSkipsAlreadyDoneJob(t *testing.T) {
	st := newFakeStore()
	if err := st.SaveTurn(context.Background(), "alex", "sess-idempotent", 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveAssistantTurn(context.Background(), "alex", "sess-idempotent", 1); err != nil {
		t.Fatalf("ReserveAssistantTurn() error = %v", err)
	}
	if err := st.CompleteAssistantTurn(context.Background(), "alex", "sess-idempotent", 1, "already done"); err != nil {
		t.Fatalf("CompleteAssistantTurn() error = %v", err)
	}

	var calls int
	pipe := &pipeline.Pipeline{LLM: fixedChatLLM{reply: "should not be called"}, ChatModel: "m"}
	handler := ReplyJobHandler(pipe, st, nil, func(string) { calls++ })

	payload := replyJobPayload{UserID: "alex", SessionID: "sess-idempotent", Turn: 1, Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, Fallback: "fb"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := handler(context.Background(), asyncjob.Job{ID: "job-1", Kind: asyncjob.KindReply, Payload: raw}); err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("onDone called %d times, want 0 (job was already done, LLM must not be re-invoked)", calls)
	}
	_, turns, err := st.SessionDetail(context.Background(), "alex", "sess-idempotent")
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "assistant" && tn.Text != "already done" {
			t.Fatalf("text = %q, want the original completed text left untouched", tn.Text)
		}
	}
}

// ---- end-to-end over a real WS connection ------------------------------

// TestWSReplyPersistsAfterClientDisconnectsMidReply is the end-to-end
// regression test for the original complaint this feature was built to
// fix: today, a reply still streaming when the learner's connection drops
// is silently discarded. With a durable ReplyHook wired in, it must
// instead keep going and land in the store.
func TestWSReplyPersistsAfterClientDisconnectsMidReply(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	gate := newGateChatLLM("the reply that must survive disconnect")
	pipe := &pipeline.Pipeline{
		STT:                []stt.Recognizer{fakeSTT{text: "hello there"}},
		LLM:                gate,
		MaxHistoryMessages: 20,
	}
	pipe.ReplyHook = NewReplyHook(pipe, st, queue)
	h := NewHandler(pipe, identity.NewCookieIdentifier(), st, nil, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Dial with an explicit session ID (skipping isNewSession's opening
	// greeting, which would otherwise also run through gate and block
	// before the read loop even starts) so turn 1's reply is the only call
	// that hits the gate.
	sessionID := "disconnect-mid-reply-session"
	c, _ := dial(t, srv, "disconnect-mid-reply-user", sessionID)
	readEvent(t, c) // ready
	sendText(t, c, "first message")
	readUntilTurn(t, c, protocol.EvFinal, 1)

	select {
	case <-gate.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("ChatStream was never called")
	}
	// Disconnect right as the reply is streaming — the exact scenario the
	// learner reported: closing the tab/losing connectivity mid-answer.
	c.Close(websocket.StatusNormalClosure, "")
	close(gate.release) // now let the "LLM" actually finish, after the client is gone

	text := waitForJobStatusDone(t, st, "disconnect-mid-reply-user", sessionID, 1, 3*time.Second)
	if text != "the reply that must survive disconnect" {
		t.Fatalf("persisted reply = %q, want the full reply even though the client disconnected mid-stream", text)
	}
}
