package transport

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/workguard"
)

// ProfileRegenerateClaimTTL/ProfileRegenerateWorkerConcurrency: unlike every
// other job in this package, runProfileRegenerate makes one
// UpdateLearnerProfile call per remaining ended session with a study summary
// — not just one — so its claim needs enough headroom for that whole
// sequential chain, not a single call's worth (see StudySummaryClaimTTL).
// Concurrency stays at 1: this only ever runs after a session delete (a rare,
// explicit action), and its DedupeKey (userID alone) already collapses
// several deletions in quick succession into a single rebuild, so there's
// nothing to parallelize per user, and no reason to size the pool for more
// than one user's rebuild finishing around the same time as another's.
const (
	ProfileRegenerateClaimTTL          = 48 * time.Hour
	ProfileRegenerateWorkerConcurrency = 1
)

// profileRegenerateJobPayload is the durable envelope for one queued
// profile-rebuild job — just the userID: the handler re-reads whatever
// sessions are actually left at execution time (see runProfileRegenerate),
// so a reap-retry (or a run that landed after several deletions were
// deduped into it) always rebuilds from current data rather than a payload
// that could be stale by the time it runs.
type profileRegenerateJobPayload struct {
	UserID string
}

// runProfileRegenerate rebuilds userID's persistent cross-session learner
// profile from scratch: every remaining ended session with a non-empty
// study summary, replayed through pipeline.UpdateLearnerProfile in the
// order they were originally ended (store.Store.ListSessionsWithStudySummary
// sorts oldest-ended first), the same one-session-at-a-time fold
// runStudySummary already does for a single session — just chained across
// every session that's left instead of one. Triggered by
// httpserver.sessionDeleteHandler after a session that itself had actually
// contributed to the profile (ended, non-empty study summary) is deleted,
// so a deleted session's influence doesn't linger in the profile forever.
//
// Deliberately all-or-nothing: if any step in the chain errors, this
// returns immediately without saving anything, leaving the existing
// (stale, but not further corrupted) profile in place for the reaper to
// retry from scratch — a partially-replayed profile would be a worse state
// than a merely-stale one, and there's no meaningful way to resume a
// half-finished chain of LLM folds.
func runProfileRegenerate(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID string) error {
	for {
		err := regenerateProfileSnapshot(ctx, pipe, st, userID)
		if !errors.Is(err, workguard.ErrDeleted) && !errors.Is(err, store.ErrProfileChanged) {
			return err
		}
		if err := workguard.Check(ctx); err != nil {
			return err
		}
	}
}

func regenerateProfileSnapshot(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID string) error {
	revision := int64(-1)
	if versioned, ok := st.(store.ProfileSnapshotStore); ok {
		_, current, err := versioned.GetLearnerProfileSnapshot(ctx, userID)
		if err != nil {
			return err
		}
		revision = current
		ctx = guardProfileRevision(ctx, versioned, userID, revision)
	}

	sessions, err := st.ListSessionsWithStudySummary(ctx, userID)
	if err != nil {
		return fmt.Errorf("profile regenerate: list sessions: %w", err)
	}

	// Lock source identities in a stable order when publishing the result.
	ids := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if source, ok := st.(workguard.Source); ok {
			ctx = workguard.Bind(ctx, func(ctx context.Context) error {
				exists, err := source.WorkExists(ctx, userID, id)
				if err != nil {
					return err
				}
				if !exists {
					return workguard.ErrDeleted
				}
				return nil
			})
		}
	}
	var profile string
	for _, sess := range sessions {
		english := studySummaryEnglish(sess.StudySummary)
		if english == "" {
			continue // defensive: the store query already filters for non-empty study_summary
		}
		profile, err = pipe.UpdateLearnerProfile(ctx, profile, english)
		if err != nil {
			return fmt.Errorf("profile regenerate: update learner profile %s: %w", sess.ID, err)
		}
	}

	if err := saveProfileSnapshot(ctx, st, userID, ids, revision, profile); err != nil {
		return fmt.Errorf("profile regenerate: save learner profile: %w", err)
	}
	return nil
}

// ProfileRegenerateJobHandler builds the asyncjob.Handler that runs one
// queued profile-rebuild job — mirrors StudySummaryJobHandler's shape.
func ProfileRegenerateJobHandler(pipe *pipeline.Pipeline, st store.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindProfileRegenerate, func(ctx context.Context, payload profileRegenerateJobPayload) error {
		return runProfileRegenerate(ctx, pipe, st, payload.UserID)
	})
}

// RunProfileRegenerateInline runs the exact same work as
// ProfileRegenerateJobHandler, synchronously, for
// httpserver.sessionDeleteHandler's no-Redis fallback — see
// RunStudySummaryInline's doc comment for the same reasoning (the caller is
// expected to run this on a detached context.Background() goroutine of its
// own).
func RunProfileRegenerateInline(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID string) error {
	return runProfileRegenerate(ctx, pipe, st, userID)
}

// EnqueueProfileRegenerateJob durably queues a profile rebuild for userID —
// see httpserver.sessionDeleteHandler, called right after store.Store.
// DeleteSession removes a session that had contributed to the profile.
// DedupeKey is userID alone (not a turnKey — this job has no session/turn to
// key off, see KindProfileRegenerate's doc comment), so several deletions in
// a row collapse into one rebuild rather than racing several redundant ones.
func EnqueueProfileRegenerateJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, st store.Store, userID string) error {
	payload := profileRegenerateJobPayload{UserID: userID}
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindProfileRegenerate, userID,
		userID, payload, ProfileRegenerateClaimTTL, ProfileRegenerateJobHandler(pipe, st))
}

func saveProfileSnapshot(ctx context.Context, st store.LearnerStore, userID string, ids []string, revision int64, profile string) error {
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	if guarded, ok := st.(store.ProfileSnapshotStore); ok {
		return guarded.PublishLearnerProfile(ctx, userID, ids, revision, profile)
	}
	return workguard.Commit(ctx, func(ctx context.Context) error { return st.SaveLearnerProfile(ctx, userID, profile) })
}

func guardProfileRevision(ctx context.Context, st store.ProfileSnapshotStore, userID string, revision int64) context.Context {
	return workguard.Bind(ctx, func(ctx context.Context) error {
		_, current, err := st.GetLearnerProfileSnapshot(ctx, userID)
		if err != nil {
			return err
		}
		if current != revision {
			return store.ErrProfileChanged
		}
		return nil
	})
}

func mergeSummaryProfile(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID, sessionID, summary string) error {
	generationCtx := ctx
	revision := int64(-1)
	var previous string
	var err error
	if versioned, ok := st.(store.ProfileSnapshotStore); ok {
		previous, revision, err = versioned.GetLearnerProfileSnapshot(ctx, userID)
		generationCtx = guardProfileRevision(ctx, versioned, userID, revision)
	} else {
		previous, err = st.GetLearnerProfile(ctx, userID)
	}
	if err != nil {
		return err
	}
	merged, err := pipe.UpdateLearnerProfile(generationCtx, previous, summary)
	if err == nil {
		err = saveProfileSnapshot(generationCtx, st, userID, []string{sessionID}, revision, merged)
	}
	// The old profile may have contained deleted data. Rebuild all surviving
	// sources on a conflicting publication rather than folding into stale text.
	if errors.Is(err, store.ErrProfileChanged) {
		return runProfileRegenerate(ctx, pipe, st, userID)
	}
	return err
}
