package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/workguard"
	"buddy/server/internal/workslot"
)

func TestModelOutputsCorrectKnownTypos(t *testing.T) {
	client := &fakeLLM{
		chatReply: "뉤앙스가 보여요.",
		complete:  func([]llm.Message) (string, error) { return "뉤앙스가 설명입니다.", nil },
	}
	p := &Pipeline{}

	if got, err := p.complete(context.Background(), client, "model", nil, false); err != nil || got != "뉘앙스가 설명입니다." {
		t.Fatalf("complete() = %q, %v", got, err)
	}
	var streamed string
	if got, err := p.chatStream(context.Background(), client, "model", nil, func(token string) { streamed += token }); err != nil || got != "뉘앙스가 보여요." || streamed != "뉘앙스가 보여요." {
		t.Fatalf("chatStream() = %q, streamed %q, %v", got, streamed, err)
	}
}

func startModelJob(t *testing.T, pool *workslot.Pool, p *Pipeline, ctx context.Context, client llm.Client, model string, stream bool) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- pool.Run(ctx, nil, func(ctx context.Context) error {
			var text string
			var err error
			if stream {
				text, err = p.chatStream(ctx, client, model, nil, nil)
			} else {
				text, err = p.complete(ctx, client, model, nil, false)
			}
			if err == nil && text != "result" {
				t.Errorf("model result = %q, want result", text)
			}
			return err
		})
	}()
	return done
}

func assertModelJobFinished(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("job error = %v, want %v", err, want)
		}
	default:
		t.Fatal("job did not finish")
	}
}

func TestModelWaitAllowsIndependentJobs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "complete"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pool := workslot.New(1)
				p := &Pipeline{}
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				blocked := &checkpointClient{
					endpoint: "server",
					complete: func(context.Context, []llm.Message) (string, error) {
						<-release
						return "result", nil
					},
					stream: func(context.Context, func(string)) (string, error) {
						<-release
						return "result", nil
					},
				}
				blockedDone := startModelJob(t, pool, p, context.Background(), blocked, "blocked", stream)
				synctest.Wait()
				if blocked.calls.Load() != 1 {
					t.Fatal("blocked model did not start")
				}
				queued := &checkpointClient{endpoint: "server"}
				queuedDone := startModelJob(t, pool, p, context.Background(), queued, "blocked", stream)
				synctest.Wait()
				healthy := &checkpointClient{endpoint: "server"}
				healthyDone := startModelJob(t, pool, p, context.Background(), healthy, "healthy", stream)
				synctest.Wait()
				assertModelJobFinished(t, healthyDone, nil)
				if queued.calls.Load() != 0 {
					t.Fatal("same-model call ran before its predecessor finished")
				}
				unblock()
				synctest.Wait()
				assertModelJobFinished(t, blockedDone, nil)
				assertModelJobFinished(t, queuedDone, nil)
				if queued.calls.Load() != 1 {
					t.Fatal("queued model did not run after its predecessor finished")
				}
			})
		})
	}
}

func TestCanceledModelJobsReleaseTheirLane(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "complete"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pool := workslot.New(1)
				p := &Pipeline{}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				blocked := &checkpointClient{
					endpoint: "server",
					complete: func(ctx context.Context, _ []llm.Message) (string, error) {
						<-ctx.Done()
						return "", ctx.Err()
					},
					stream: func(ctx context.Context, _ func(string)) (string, error) {
						<-ctx.Done()
						return "", ctx.Err()
					},
				}
				blockedDone := startModelJob(t, pool, p, ctx, blocked, "model", stream)
				synctest.Wait()
				queuedCtx, cancelQueued := context.WithCancel(context.Background())
				defer cancelQueued()
				queued := &checkpointClient{endpoint: "server"}
				queuedDone := startModelJob(t, pool, p, queuedCtx, queued, "model", stream)
				synctest.Wait()
				cancelQueued()
				synctest.Wait()
				assertModelJobFinished(t, queuedDone, context.Canceled)
				if queued.calls.Load() != 0 || blocked.calls.Load() != 1 {
					t.Fatalf("canceled queue entry reached model: queued = %d, running = %d", queued.calls.Load(), blocked.calls.Load())
				}
				following := &checkpointClient{endpoint: "server"}
				followingDone := startModelJob(t, pool, p, context.Background(), following, "model", stream)
				synctest.Wait()
				if following.calls.Load() != 0 {
					t.Fatal("following job ran before in-flight cancellation")
				}
				cancel()
				synctest.Wait()
				assertModelJobFinished(t, blockedDone, context.Canceled)
				assertModelJobFinished(t, followingDone, nil)
				if following.calls.Load() != 1 {
					t.Fatal("cancellation stranded the following model call")
				}
			})
		})
	}
}

func TestModelWaitPreservesCascadeOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := workslot.New(1)
		releaseChat := make(chan struct{})
		releaseAnalysis := make(chan struct{})
		unblockChat := sync.OnceFunc(func() { close(releaseChat) })
		unblockAnalysis := sync.OnceFunc(func() { close(releaseAnalysis) })
		defer unblockChat()
		defer unblockAnalysis()
		chat := &checkpointClient{endpoint: "server", complete: func(context.Context, []llm.Message) (string, error) {
			<-releaseChat
			return "result", nil
		}}
		analysis := &checkpointClient{endpoint: "server", complete: func(context.Context, []llm.Message) (string, error) {
			<-releaseAnalysis
			return "result", nil
		}}
		otherAnalysis := &checkpointClient{endpoint: "server"}
		judge := &checkpointClient{endpoint: "server"}
		p := &Pipeline{
			LLM: chat, ChatModel: "chat",
			Analysis: []Candidate{{LLM: analysis, Model: "analysis"}, {LLM: otherAnalysis, Model: "other-analysis"}},
			Judge:    judge, JudgeModel: "judge",
		}
		done := make(chan error, 1)
		go func() {
			done <- pool.Run(context.Background(), nil, func(ctx context.Context) error {
				text, err := p.analyze(ctx, "task", "input", false)
				if err == nil && text != "result" {
					t.Errorf("cascade result = %q, want result", text)
				}
				return err
			})
		}()
		synctest.Wait()
		assertCheckpointCalls(t, []*checkpointClient{chat, analysis, otherAnalysis, judge}, []int32{1, 0, 0, 0})
		independent := &checkpointClient{endpoint: "server"}
		independentDone := startModelJob(t, pool, p, context.Background(), independent, "analysis", false)
		synctest.Wait()
		assertModelJobFinished(t, independentDone, nil)
		assertCheckpointCalls(t, []*checkpointClient{analysis, otherAnalysis, judge}, []int32{0, 0, 0})
		unblockChat()
		synctest.Wait()
		assertCheckpointCalls(t, []*checkpointClient{chat, analysis, otherAnalysis, judge}, []int32{1, 1, 1, 0})
		independentDone = startModelJob(t, pool, p, context.Background(), independent, "judge", false)
		synctest.Wait()
		assertModelJobFinished(t, independentDone, nil)
		if judge.calls.Load() != 0 {
			t.Fatal("judge ran before every analysis candidate finished")
		}
		unblockAnalysis()
		synctest.Wait()
		assertModelJobFinished(t, done, nil)
		assertCheckpointCalls(t, []*checkpointClient{chat, analysis, otherAnalysis, judge}, []int32{1, 1, 1, 1})
	})
}

func TestModelGuardCancelsWhileWaitingToResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := workslot.New(1)
		p := &Pipeline{}
		var deleted atomic.Bool
		ctx := workguard.Bind(context.Background(), func(context.Context) error {
			if deleted.Load() {
				return workguard.ErrDeleted
			}
			return nil
		})
		releaseModel := make(chan struct{})
		unblockModel := sync.OnceFunc(func() { close(releaseModel) })
		defer unblockModel()
		client := &checkpointClient{complete: func(context.Context, []llm.Message) (string, error) {
			<-releaseModel
			return "result", nil
		}}
		done := startModelJob(t, pool, p, ctx, client, "model", false)
		synctest.Wait()
		releaseOther := make(chan struct{})
		unblockOther := sync.OnceFunc(func() { close(releaseOther) })
		defer unblockOther()
		otherDone := make(chan error, 1)
		go func() {
			otherDone <- pool.Run(context.Background(), nil, func(context.Context) error {
				<-releaseOther
				return nil
			})
		}()
		synctest.Wait()
		unblockModel()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("job resumed while another job held the worker slot: %v", err)
		default:
		}
		deleted.Store(true)
		// synctest advances its virtual clock to the guard's polling tick.
		time.Sleep(time.Second)
		synctest.Wait()
		assertModelJobFinished(t, done, workguard.ErrDeleted)
		unblockOther()
		synctest.Wait()
		assertModelJobFinished(t, otherDone, nil)
	})
}

func TestBackgroundCascadeYieldsAtEveryModel(t *testing.T) {
	for _, stage := range []string{"chat", "analysis", "judge"} {
		for _, stream := range []bool{false, true} {
			name := stage + "/complete"
			if stream {
				name = stage + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var calls []string
					model := func(name string) *fakeLLM {
						return &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
							calls = append(calls, name)
							return "migration result", nil
						}}
					}
					p := &Pipeline{
						LLM: model("chat"), ChatModel: "chat",
						Analysis: []Candidate{{LLM: model("analysis"), Model: "analysis"}},
						Judge:    model("judge"), JudgeModel: "judge",
					}
					release := make(chan struct{})
					unblock := sync.OnceFunc(func() { close(release) })
					defer unblock()
					go func() {
						if err := p.modelCallQueue().Do(context.Background(), stage, func() error {
							<-release
							return nil
						}); err != nil {
							t.Errorf("occupy %s: %v", stage, err)
						}
					}()
					synctest.Wait()
					go func() {
						got, err := p.analyze(llm.WithBackgroundPriority(context.Background()), "migration", "old entry", false)
						if err != nil || got != "migration result" {
							t.Errorf("migration = %q, %v", got, err)
						}
					}()
					synctest.Wait()
					interactive := &fakeLLM{
						complete: func([]llm.Message) (string, error) {
							calls = append(calls, "interactive")
							return "answer", nil
						},
						chatReply: "answer",
						onChat:    func([]llm.Message) { calls = append(calls, "interactive") },
					}
					go func() {
						var got string
						var err error
						if stream {
							got, err = p.chatStream(context.Background(), interactive, stage, nil, nil)
						} else {
							got, err = p.complete(context.Background(), interactive, stage, nil, false)
						}
						if err != nil || got != "answer" {
							t.Errorf("interactive = %q, %v", got, err)
						}
					}()
					synctest.Wait()
					unblock()
					synctest.Wait()
					want := []string{"chat", "analysis", "judge"}
					want = slices.Insert(want, slices.Index(want, stage), "interactive")
					if !slices.Equal(calls, want) {
						t.Errorf("model calls = %v, want %v", calls, want)
					}
				})
			})
		}
	}
}

// TestAnalyzeQueuesEachModelIndependently reproduces the important request
// interleaving: request A is holding llm2, request B must still enter llm1,
// and B's llm2 call must wait behind A's llm2 call. A request-wide semaphore
// would fail this test because B's llm1 would not be reached until A finished.
func TestAnalyzeQueuesEachModelIndependently(t *testing.T) {
	llm2StartedByA := make(chan struct{})
	releaseA := make(chan struct{})
	llm1StartedByB := make(chan struct{})
	llm2StartedByB := make(chan struct{})

	model1 := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		if strings.TrimSpace(msgs[len(msgs)-1].Content) == "request B" {
			close(llm1StartedByB)
		}
		return "model 1 result", nil
	}}
	model2 := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		switch strings.TrimSpace(msgs[len(msgs)-1].Content) {
		case "request A":
			close(llm2StartedByA)
			<-releaseA
		case "request B":
			close(llm2StartedByB)
		}
		return "model 2 result", nil
	}}

	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "llm1", LLM: model1},
			{Model: "llm2", LLM: model2},
		},
	}

	aDone := make(chan error, 1)
	go func() {
		_, err := p.analyze(context.Background(), "task", "request A", false)
		aDone <- err
	}()
	waitForPipelineSignal(t, llm2StartedByA)

	bDone := make(chan error, 1)
	go func() {
		_, err := p.analyze(context.Background(), "task", "request B", false)
		bDone <- err
	}()
	waitForPipelineSignal(t, llm1StartedByB)

	select {
	case <-llm2StartedByB:
		t.Fatal("request B entered llm2 before request A released llm2")
	default:
	}

	close(releaseA)
	waitForPipelineSignal(t, llm2StartedByB)
	if err := <-aDone; err != nil {
		t.Fatalf("request A: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("request B: %v", err)
	}
}

func waitForPipelineSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for model call")
	}
}
