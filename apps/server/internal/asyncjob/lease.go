package asyncjob

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/workguard"
)

// MaxClaimLeaseTTL caps the renewable Redis claim itself, independently of
// how long a handler may legitimately run. A live handler refreshes this
// lease every third of the interval, so even a day-long local-model request
// remains owned. If its process dies, however, the short lease disappears
// quickly enough for another replica to reap and retry the job instead of
// leaving it stuck for the handler's full worst-case duration.
//
// A var, rather than a const, lets tests exercise crash recovery without
// waiting for the production interval.
var MaxClaimLeaseTTL = 2 * time.Minute

// ErrClaimLost means this execution no longer owns the job's Redis claim.
// Callers must discard its result: a newer attempt is now responsible for
// completing the same idempotent job.
var ErrClaimLost = errors.New("asyncjob: claim ownership lost")

func claimLeaseTTL(requested time.Duration) time.Duration {
	if requested <= 0 || MaxClaimLeaseTTL <= 0 || requested <= MaxClaimLeaseTTL {
		return requested
	}
	return MaxClaimLeaseTTL
}

func claimToken(job Job) string {
	return fmt.Sprintf("attempt:%d", job.Attempts)
}

func renewClaim(ctx context.Context, rdb redis.UniversalClient, kind Kind, id, token string, ttl time.Duration) (bool, error) {
	n, err := renewClaimScript.Run(ctx, rdb, []string{claimKey(kind, id)}, token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// runWithLease keeps a claimed job alive for as long as its handler is
// actually running. If the process dies, the heartbeat dies with it and the
// existing reaper can safely put the processing entry back on the queue.
func runWithLease(ctx context.Context, rdb redis.UniversalClient, kind Kind, id, token string, ttl time.Duration, handler func(context.Context) error) (result error) {
	defer func() {
		if errors.Is(result, workguard.ErrDeleted) {
			result = nil
		}
	}()
	if ttl <= 0 {
		return handler(ctx)
	}
	ttl = claimLeaseTTL(ttl)
	owned, err := renewClaim(ctx, rdb, kind, id, token, ttl)
	if err != nil {
		return fmt.Errorf("renew initial claim: %w", err)
	}
	if !owned {
		return ErrClaimLost
	}
	handlerCtx, cancelHandler := context.WithCancel(ctx)
	defer cancelHandler()
	heartbeatCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	leaseErr := make(chan error, 1)
	interval := ttl / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				owned, err := renewClaim(context.Background(), rdb, kind, id, token, ttl)
				if err != nil {
					claimErr := fmt.Errorf("renew claim: %w", err)
					log.Printf("asyncjob: %s: renew claim %s: %v", kind, id, err)
					leaseErr <- claimErr
					cancelHandler()
					return
				}
				if !owned {
					leaseErr <- ErrClaimLost
					cancelHandler()
					return
				}
			}
		}
	}()
	err = handler(handlerCtx)
	stop()
	<-done
	select {
	case err := <-leaseErr:
		return err
	default:
	}
	return err
}
