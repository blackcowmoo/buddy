package transport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/nuance"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/workguard"
	"github.com/google/uuid"
)

// asyncjob renews this lease during slow inference; it bounds crash recovery,
// not model runtime.
const NuanceClaimTTL = 15 * time.Minute

const nuanceGenerationAttempts = 3

const nuanceSupplementOperation = "supplement"

type nuanceJobPayload struct {
	UserID    string
	LessonID  string
	Operation string
	RequestID string
}

var nuanceInlineJobs sync.Map

func NuanceJobHandler(pipe *pipeline.Pipeline, st nuance.Store, profile func(context.Context, string) (string, error)) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindNuance, func(ctx context.Context, p nuanceJobPayload) (err error) {
		ctx = workguard.BindStore(ctx, st, p.UserID, p.LessonID)
		if err := workguard.Check(ctx); err != nil {
			return err
		}
		if p.Operation == nuanceSupplementOperation {
			return supplementNuance(ctx, pipe, st, p)
		}
		l, err := st.Get(ctx, p.UserID, p.LessonID)
		if errors.Is(err, nuance.ErrNotFound) || (err == nil && l.Status == nuance.StatusDone) {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() {
			if err != nil && workguard.Check(ctx) == nil {
				_ = st.SetStatus(context.Background(), p.LessonID, nuance.StatusFailed)
			}
		}()
		if err = st.SetStatus(ctx, p.LessonID, nuance.StatusProcessing); err != nil {
			return err
		}
		learner, err := profile(ctx, p.UserID)
		if err != nil {
			return err
		}
		lessons, err := st.List(ctx, p.UserID)
		if err != nil {
			return err
		}
		previous := []string{}
		for _, old := range lessons {
			if old.Content != nil {
				previous = append(previous, nuanceComparison(*old.Content))
			}
		}
		// Status transitions advance the durable revision so a rejected model
		// response cached by the LLM client cannot poison every retry of this draw.
		for attempt := 0; attempt < nuanceGenerationAttempts; attempt++ {
			// Only the prompt is bounded; MySQL checks the entire saved history.
			if len(previous) > 100 {
				previous = previous[len(previous)-100:]
			}
			requestID := fmt.Sprintf("%s:%d:%d", p.LessonID, l.Revision, attempt)
			c, err := pipe.GenerateNuance(ctx, learner, previous, requestID)
			if err != nil {
				// A candidate that still violates the lesson contract after its
				// focused Judge repair will fail identically on an automatic job
				// retry. Persist the terminal state and reserve queue retries for
				// transient model, network, and storage failures. The explicit UI
				// retry gets a fresh durable revision and request ID.
				if errors.Is(err, nuance.ErrInvalid) {
					log.Printf("nuance: invalid generated lesson %s: %v", p.LessonID, err)
					return st.SetStatus(ctx, p.LessonID, nuance.StatusFailed)
				}
				return err
			}
			err = st.Complete(ctx, p.LessonID, c)
			if !errors.Is(err, nuance.ErrDuplicate) {
				return err
			}
			// Include the rejected comparison even if it is old or another draw
			// saved it after we loaded the exclusions. The attempt ID avoids cache reuse.
			previous = append(previous, nuanceComparison(c))
		}
		return fmt.Errorf("%w after %d generation attempts", nuance.ErrDuplicate, nuanceGenerationAttempts)
	})
}

func supplementNuance(ctx context.Context, pipe *pipeline.Pipeline, st nuance.Store, p nuanceJobPayload) error {
	l, err := st.Get(ctx, p.UserID, p.LessonID)
	if errors.Is(err, nuance.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if l.Status != nuance.StatusDone || l.Content == nil || len(l.Content.MissingAnswers()) == 0 {
		return nil
	}
	questions, err := pipe.SupplementNuance(ctx, *l.Content, p.RequestID)
	if errors.Is(err, nuance.ErrInvalid) {
		// A focused repair already failed for this request. Finish this job so a
		// later page load can enqueue a fresh request ID instead of replaying a
		// deterministic cached response through the durable retry loop.
		log.Printf("nuance: invalid question supplement %s: %v", p.LessonID, err)
		return nil
	}
	if err != nil {
		return err
	}
	err = st.AddQuestions(ctx, p.UserID, p.LessonID, questions)
	if errors.Is(err, nuance.ErrNotFound) {
		return nil
	}
	return err
}

func nuanceComparison(c nuance.Content) string {
	words := make([]string, len(c.Words))
	for i, w := range c.Words {
		words[i] = w.Word
	}
	return strings.Join(words, " / ")
}

func EnqueueNuance(ctx context.Context, q *asyncjob.Queue, pipe *pipeline.Pipeline, st nuance.Store, profile func(context.Context, string) (string, error), userID, id string) error {
	payload := nuanceJobPayload{UserID: userID, LessonID: id}
	handler := NuanceJobHandler(pipe, st, profile)
	if q != nil {
		return q.EnqueueAndRunInBackground(ctx, asyncjob.KindNuance, id, id, payload, NuanceClaimTTL, handler)
	}
	// Read-triggered recovery must not start another inline call on every poll.
	if _, loaded := nuanceInlineJobs.LoadOrStore(id, struct{}{}); loaded {
		return nil
	}
	go func() {
		defer nuanceInlineJobs.Delete(id)
		if err := handler(context.Background(), asyncjob.Job{Payload: mustPayload(payload)}); err != nil {
			log.Printf("nuance: generate %s: %v", id, err)
		}
	}()
	return nil
}

// EnqueueNuanceSupplement lazily repairs an existing lesson without changing
// its visible status or discarding practice history. Redis deduplicates across
// replicas; nuanceInlineJobs does the same inside a no-Redis process.
func EnqueueNuanceSupplement(ctx context.Context, q *asyncjob.Queue, pipe *pipeline.Pipeline, st nuance.Store, profile func(context.Context, string) (string, error), userID, id string) error {
	key := "supplement:" + id
	payload := nuanceJobPayload{UserID: userID, LessonID: id, Operation: nuanceSupplementOperation, RequestID: uuid.NewString()}
	handler := NuanceJobHandler(pipe, st, profile)
	if q != nil {
		return q.EnqueueAndRunInBackground(ctx, asyncjob.KindNuance, key, key, payload, NuanceClaimTTL, handler)
	}
	if _, loaded := nuanceInlineJobs.LoadOrStore(key, struct{}{}); loaded {
		return nil
	}
	go func() {
		defer nuanceInlineJobs.Delete(key)
		if err := handler(context.Background(), asyncjob.Job{Payload: mustPayload(payload)}); err != nil {
			log.Printf("nuance: supplement %s: %v", id, err)
		}
	}()
	return nil
}
