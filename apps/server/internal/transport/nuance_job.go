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
)

// asyncjob renews this lease during slow inference; it bounds crash recovery,
// not model runtime.
const NuanceClaimTTL = 15 * time.Minute

const nuanceGenerationAttempts = 3

type nuanceJobPayload struct{ UserID, LessonID string }

var nuanceInlineJobs sync.Map

func NuanceJobHandler(pipe *pipeline.Pipeline, st nuance.Store, profile func(context.Context, string) (string, error)) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindNuance, func(ctx context.Context, p nuanceJobPayload) (err error) {
		l, err := st.Get(ctx, p.UserID, p.LessonID)
		if errors.Is(err, nuance.ErrNotFound) || (err == nil && l.Status == nuance.StatusDone) {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() {
			if err != nil {
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

func nuanceComparison(c nuance.Content) string {
	words := make([]string, len(c.Words))
	for i, w := range c.Words {
		words[i] = w.Word
	}
	return strings.Join(words, " / ")
}

func EnqueueNuance(ctx context.Context, q *asyncjob.Queue, pipe *pipeline.Pipeline, st nuance.Store, profile func(context.Context, string) (string, error), userID, id string) error {
	payload := nuanceJobPayload{userID, id}
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
