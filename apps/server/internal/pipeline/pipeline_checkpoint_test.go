package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"buddy/server/internal/checkpoint"
	"buddy/server/internal/checkpoint/checkpointtest"
	"buddy/server/internal/llm"
	"buddy/server/internal/workguard"
)

type checkpointClient struct {
	endpoint string
	calls    atomic.Int32
	complete func(context.Context, []llm.Message) (string, error)
	stream   func(context.Context, func(string)) (string, error)
}

func (c *checkpointClient) QueueKey(model string) string { return c.endpoint + "/" + model }

func (c *checkpointClient) Complete(ctx context.Context, _ string, msgs []llm.Message, _ bool) (string, error) {
	c.calls.Add(1)
	if c.complete != nil {
		return c.complete(ctx, msgs)
	}
	return "result", nil
}

func (c *checkpointClient) ChatStream(ctx context.Context, _ string, _ []llm.Message, onToken func(string)) (string, error) {
	c.calls.Add(1)
	if c.stream != nil {
		return c.stream(ctx, onToken)
	}
	onToken("result")
	return "result", nil
}

type observingCheckpointStore struct {
	checkpointtest.Memory
	afterLoad func()
	afterSave func(string)
	loadErr   error
	saveErr   error
	failValue string
}

func (s *observingCheckpointStore) Load(ctx context.Context, key string) (string, bool, error) {
	if s.loadErr != nil {
		return "", false, s.loadErr
	}
	text, found, err := s.Memory.Load(ctx, key)
	if s.afterLoad != nil {
		s.afterLoad()
	}
	return text, found, err
}

func (s *observingCheckpointStore) Save(ctx context.Context, key, text string) error {
	if s.saveErr != nil && (s.failValue == "" || s.failValue == text) {
		return s.saveErr
	}
	if err := s.Memory.Save(ctx, key, text); err != nil {
		return err
	}
	if s.afterSave != nil {
		s.afterSave(text)
	}
	return nil
}

func checkpointCascade() (*Pipeline, []*checkpointClient) {
	clients := make([]*checkpointClient, 4)
	for i, response := range []string{"chat draft", "first refinement", "second refinement", "judge final"} {
		clients[i] = &checkpointClient{endpoint: "server", complete: func(context.Context, []llm.Message) (string, error) {
			return response, nil
		}}
	}
	return &Pipeline{
		LLM: clients[0], ChatModel: "chat",
		Analysis: []Candidate{{LLM: clients[1], Model: "first"}, {LLM: clients[2], Model: "second"}},
		Judge:    clients[3], JudgeModel: "judge",
	}, clients
}

func assertCheckpointCalls(t *testing.T, clients []*checkpointClient, want []int32) {
	t.Helper()
	for i, c := range clients {
		if got := c.calls.Load(); got != want[i] {
			t.Errorf("model %d calls = %d, want %d", i, got, want[i])
		}
	}
}

func TestCheckpointCascadeResumesAfterJudgeCancellation(t *testing.T) {
	var store checkpointtest.Memory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, clients := checkpointCascade()
	clients[3].complete = func(context.Context, []llm.Message) (string, error) {
		cancel()
		return "unfinished judge", context.Canceled
	}
	if _, err := first.analyze(checkpoint.Bind(ctx, &store), "task", "input", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted cascade error = %v", err)
	}
	assertCheckpointCalls(t, clients, []int32{1, 1, 1, 1})

	// A new worker and new attempt share only the durable job's store.
	resumed, resumedClients := checkpointCascade()
	resumedClients[3].complete = func(_ context.Context, msgs []llm.Message) (string, error) {
		for _, prior := range []string{"chat draft", "first refinement", "second refinement"} {
			if !strings.Contains(msgs[1].Content, prior) {
				t.Errorf("resumed judge lost completed stage %q", prior)
			}
		}
		return "judge final", nil
	}
	got, err := resumed.analyze(checkpoint.Bind(context.Background(), &store), "task", "input", true)
	if err != nil || got != "judge final" {
		t.Fatalf("resumed cascade = %q, %v", got, err)
	}
	assertCheckpointCalls(t, resumedClients, []int32{0, 0, 0, 1})
}

func TestCheckpointCascadePreservesCompletedParallelCandidate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondStarted := make(chan struct{})
	store := &observingCheckpointStore{afterSave: func(text string) {
		if text == "first refinement" {
			<-secondStarted
			cancel()
		}
	}}
	first, clients := checkpointCascade()
	clients[2].complete = func(ctx context.Context, _ []llm.Message) (string, error) {
		close(secondStarted)
		<-ctx.Done()
		return "partial refinement", ctx.Err()
	}
	if _, err := first.analyze(checkpoint.Bind(ctx, store), "task", "input", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted cascade error = %v", err)
	}
	assertCheckpointCalls(t, clients, []int32{1, 1, 1, 0})
	store.afterSave = nil

	resumed, resumedClients := checkpointCascade()
	got, err := resumed.analyze(checkpoint.Bind(context.Background(), store), "task", "input", false)
	if err != nil || got != "judge final" {
		t.Fatalf("resumed cascade = %q, %v", got, err)
	}
	assertCheckpointCalls(t, resumedClients, []int32{0, 0, 1, 1})
}

func TestCheckpointCascadeRetriesRejectedTerminalOutput(t *testing.T) {
	accepts := func(raw string) bool {
		var result struct {
			Answer string `json:"answer"`
		}
		return json.Unmarshal([]byte(raw), &result) == nil && result.Answer != ""
	}
	newCascade := func() (*Pipeline, []*checkpointClient) {
		p, clients := checkpointCascade()
		for i, answer := range []string{"chat draft", "first refinement", "second refinement", "judge final"} {
			clients[i].complete = func(context.Context, []llm.Message) (string, error) {
				return `{"answer":"` + answer + `"}`, nil
			}
		}
		return p, clients
	}
	for _, invalid := range []string{`{"answer":`, `{"answer":""}`} {
		t.Run(invalid, func(t *testing.T) {
			var store checkpointtest.Memory
			first, clients := newCascade()
			clients[3].complete = func(context.Context, []llm.Message) (string, error) {
				return invalid, nil
			}
			// Validation governs reuse; the existing caller still parses and
			// reports the current attempt's invalid terminal output itself.
			got, err := first.analyze(checkpoint.Bind(context.Background(), &store), "task", "input", true, accepts)
			if err != nil || got != invalid {
				t.Fatalf("first cascade = %q, %v", got, err)
			}
			assertCheckpointCalls(t, clients, []int32{1, 1, 1, 1})
			for attempt := 0; attempt < 2; attempt++ {
				resumed, resumedClients := newCascade()
				got, err := resumed.analyze(checkpoint.Bind(context.Background(), &store), "task", "input", true, accepts)
				if err != nil || got != `{"answer":"judge final"}` {
					t.Fatalf("resumed cascade = %q, %v", got, err)
				}
				want := []int32{0, 0, 0, 0}
				if attempt == 0 {
					want[3] = 1
				}
				assertCheckpointCalls(t, resumedClients, want)
			}
		})
	}
}

func TestCheckpointCascadeRetriesRejectedFallback(t *testing.T) {
	for _, fallback := range []string{"chat", "analysis"} {
		t.Run(fallback, func(t *testing.T) {
			var store checkpointtest.Memory
			accepts := func(raw string) bool { return raw == "valid" }
			newFallback := func(invalid bool) (*Pipeline, []*checkpointClient) {
				p, clients := checkpointCascade()
				p.Judge = nil
				p.Analysis = nil
				terminal := 0
				if fallback == "analysis" {
					p.Analysis = []Candidate{{LLM: clients[1], Model: "first"}}
					terminal = 1
				}
				for i, c := range clients {
					c.complete = func(context.Context, []llm.Message) (string, error) {
						if invalid && i == terminal {
							return "invalid", nil
						}
						return "valid", nil
					}
				}
				return p, clients
			}
			first, _ := newFallback(true)
			if got, err := first.analyze(checkpoint.Bind(context.Background(), &store), "task", "input", false, accepts); err != nil || got != "invalid" {
				t.Fatalf("first fallback = %q, %v", got, err)
			}
			resumed, resumedClients := newFallback(false)
			if got, err := resumed.analyze(checkpoint.Bind(context.Background(), &store), "task", "input", false, accepts); err != nil || got != "valid" {
				t.Fatalf("resumed fallback = %q, %v", got, err)
			}
			want := []int32{1, 0, 0, 0}
			if fallback == "analysis" {
				want = []int32{0, 1, 0, 0}
			}
			assertCheckpointCalls(t, resumedClients, want)
		})
	}
}

func TestCheckpointStreamReplaysAfterHandlerWriteFailure(t *testing.T) {
	var store checkpointtest.Memory
	client := &checkpointClient{endpoint: "server", stream: func(_ context.Context, token func(string)) (string, error) {
		token("complete ")
		token("reply")
		return "complete reply", nil
	}}
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "hello"}}
	writeFailure := errors.New("turn write failed")
	var firstTokens []string
	handler := func() error {
		p := &Pipeline{}
		_, err := p.chatStream(checkpoint.Bind(context.Background(), &store), client, "chat", msgs, func(token string) {
			firstTokens = append(firstTokens, token)
		})
		if err != nil {
			return err
		}
		return writeFailure
	}
	if err := handler(); !errors.Is(err, writeFailure) {
		t.Fatalf("first handler error = %v", err)
	}
	if !reflect.DeepEqual(firstTokens, []string{"complete ", "reply"}) {
		t.Fatalf("initial stream tokens = %#v", firstTokens)
	}
	var replayTokens []string
	resumed := &Pipeline{}
	resumedClient := &checkpointClient{endpoint: "server"}
	got, err := resumed.chatStream(checkpoint.Bind(context.Background(), &store), resumedClient, "chat", msgs, func(token string) {
		replayTokens = append(replayTokens, token)
	})
	if err != nil || got != "complete reply" || !reflect.DeepEqual(replayTokens, []string{"complete reply"}) {
		t.Fatalf("replayed stream = %q, %v, tokens %#v", got, err, replayTokens)
	}
	assertCheckpointCalls(t, []*checkpointClient{client, resumedClient}, []int32{1, 0})
}

func TestCheckpointDoesNotReuseFailedPartialOrEmptyResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		text   string
		err    error
		cancel bool
	}{
		{name: "failed", err: errors.New("model failed")},
		{name: "partial", text: "partial result", err: errors.New("stream interrupted")},
		{name: "empty"},
		{name: "whitespace", text: " \n\t"},
		{name: "canceled despite success", text: "response after cancellation", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				var store checkpointtest.Memory
				ctx, cancel := context.WithCancel(context.Background())
				client := &checkpointClient{endpoint: "server"}
				generate := func() (string, error) {
					if test.cancel {
						cancel()
					}
					return test.text, test.err
				}
				client.complete = func(context.Context, []llm.Message) (string, error) { return generate() }
				client.stream = func(_ context.Context, token func(string)) (string, error) {
					token(test.text)
					return generate()
				}
				call := func(ctx context.Context, client *checkpointClient) (string, error) {
					p := &Pipeline{}
					if streaming {
						return p.chatStream(ctx, client, "model", nil, func(string) {})
					}
					return p.complete(ctx, client, "model", nil, false)
				}
				_, err := call(checkpoint.Bind(ctx, &store), client)
				cancel()
				if (test.err != nil || test.cancel) && err == nil {
					t.Fatalf("stream=%v: interrupted call unexpectedly succeeded", streaming)
				}
				resumed := &checkpointClient{endpoint: "server"}
				for i := 0; i < 2; i++ {
					got, err := call(checkpoint.Bind(context.Background(), &store), resumed)
					if err != nil || got != "result" {
						t.Fatalf("stream=%v: resumed call = %q, %v", streaming, got, err)
					}
				}
				assertCheckpointCalls(t, []*checkpointClient{client, resumed}, []int32{1, 1})
			}
		})
	}
}

func TestCheckpointRequestIdentity(t *testing.T) {
	for _, change := range []string{"endpoint", "model", "content", "role", "json", "stream", "message boundaries"} {
		t.Run(change, func(t *testing.T) {
			var store checkpointtest.Memory
			p := &Pipeline{}
			client := &checkpointClient{endpoint: "original endpoint"}
			msgs := []llm.Message{{Role: "user", Content: "hello"}}
			if change == "message boundaries" {
				msgs = append(msgs, llm.Message{Role: "assistant", Content: "answer"})
			}
			if _, err := p.complete(checkpoint.Bind(context.Background(), &store), client, "original model", msgs, false); err != nil {
				t.Fatal(err)
			}
			model, jsonMode := "original model", false
			switch change {
			case "endpoint":
				client.endpoint = "new endpoint"
			case "model":
				model = "new model"
			case "content":
				msgs[0].Content = "different input"
			case "role":
				msgs[0].Role = "system"
			case "json":
				jsonMode = true
			case "message boundaries":
				msgs = []llm.Message{{Role: "user", Content: "hello\x00assistant\x00answer"}}
			}
			for i := 0; i < 2; i++ {
				ctx := checkpoint.Bind(context.Background(), &store)
				var err error
				if change == "stream" {
					_, err = p.chatStream(ctx, client, model, msgs, func(string) {})
				} else {
					_, err = p.complete(ctx, client, model, msgs, jsonMode)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := client.calls.Load(); got != 2 {
				t.Fatalf("calls after request change and replay = %d, want 2", got)
			}
		})
	}
}

func TestCheckpointIsScopedToJobContext(t *testing.T) {
	p := &Pipeline{}
	client := &checkpointClient{endpoint: "server"}
	var first, second checkpointtest.Memory
	for _, ctx := range []context.Context{
		checkpoint.Bind(context.Background(), &first),
		checkpoint.Bind(context.Background(), &first),
		checkpoint.Bind(context.Background(), &second),
		context.Background(), context.Background(),
	} {
		if _, err := p.chatStream(ctx, client, "chat", nil, func(string) {}); err != nil {
			t.Fatal(err)
		}
	}
	if got := client.calls.Load(); got != 4 {
		t.Fatalf("calls across two jobs and unbound live chats = %d, want 4", got)
	}
}

func TestCheckpointStorageFailureStopsCascade(t *testing.T) {
	for _, operation := range []string{"load", "save chat", "save analysis", "save judge"} {
		t.Run(operation, func(t *testing.T) {
			failure := errors.New("checkpoint storage unavailable")
			store := &observingCheckpointStore{}
			if operation == "load" {
				store.loadErr = failure
			} else {
				store.saveErr = failure
				switch operation {
				case "save chat":
					store.failValue = "chat draft"
				case "save analysis":
					store.failValue = "first refinement"
				case "save judge":
					store.failValue = "judge final"
				}
			}
			p, clients := checkpointCascade()
			result, err := p.analyze(checkpoint.Bind(context.Background(), store), "task", "input", true)
			if !errors.Is(err, failure) || result != "" {
				t.Fatalf("cascade = %q, %v; want storage failure without fallback", result, err)
			}
			want := []int32{0, 0, 0, 0}
			if operation == "save chat" {
				want[0] = 1
			} else if operation == "save judge" {
				want = []int32{1, 1, 1, 1}
			} else if operation == "save analysis" {
				// The other parallel candidate may enter the model before the
				// failing candidate stops this attempt. Judge must never start.
				want = []int32{1, 1, clients[2].calls.Load(), 0}
			}
			assertCheckpointCalls(t, clients, want)
		})
	}
}

func TestCheckpointHitRespectsDeletionAndReplayCancellation(t *testing.T) {
	for _, when := range []string{"before lookup", "during lookup", "during replay"} {
		t.Run(when, func(t *testing.T) {
			store := &observingCheckpointStore{}
			p := &Pipeline{}
			client := &checkpointClient{endpoint: "server"}
			if _, err := p.chatStream(checkpoint.Bind(context.Background(), store), client, "model", nil, func(string) {}); err != nil {
				t.Fatal(err)
			}
			var deleted atomic.Bool
			ctx := workguard.Bind(context.Background(), func(context.Context) error {
				if deleted.Load() {
					return workguard.ErrDeleted
				}
				return nil
			})
			switch when {
			case "before lookup":
				deleted.Store(true)
			case "during lookup":
				store.afterLoad = func() { deleted.Store(true) }
			}
			tokens := 0
			result, err := p.chatStream(checkpoint.Bind(ctx, store), client, "model", nil, func(string) {
				tokens++
				if when == "during replay" {
					deleted.Store(true)
				}
			})
			if !errors.Is(err, workguard.ErrDeleted) || result != "" {
				t.Fatalf("guarded replay = %q, %v", result, err)
			}
			if when != "during replay" && tokens != 0 {
				t.Fatalf("deleted job replayed %d tokens", tokens)
			}
			if got := client.calls.Load(); got != 1 {
				t.Fatalf("deleted job made new model call: %d", got)
			}
		})
	}
}

func TestCheckpointHitBypassesBusyModelQueue(t *testing.T) {
	var store checkpointtest.Memory
	p := &Pipeline{}
	client := &checkpointClient{endpoint: "server"}
	if _, err := p.complete(checkpoint.Bind(context.Background(), &store), client, "model", nil, false); err != nil {
		t.Fatal(err)
	}
	occupied := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- p.runModelCall(context.Background(), client, "model", func(context.Context) error {
			close(occupied)
			<-release
			return nil
		})
	}()
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	waitForPipelineSignal(t, occupied)
	replayed := make(chan struct{})
	go func() {
		defer close(replayed)
		text, err := p.complete(checkpoint.Bind(context.Background(), &store), client, "model", nil, false)
		if err != nil || text != "result" {
			t.Errorf("queued checkpoint replay = %q, %v", text, err)
		}
	}()
	waitForPipelineSignal(t, replayed)
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("checkpoint hit called model again: %d", got)
	}
}
