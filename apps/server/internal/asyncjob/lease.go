package asyncjob

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// runWithLease keeps a claimed job alive for as long as its handler is
// actually running. If the process dies, the heartbeat dies with it and the
// existing reaper can safely put the processing entry back on the queue.
func runWithLease(ctx context.Context, rdb redis.UniversalClient, kind Kind, id string, ttl time.Duration, handler func(context.Context) error) error {
	if ttl <= 0 {
		return handler(ctx)
	}
	heartbeatCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
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
				if err := rdb.Expire(context.Background(), claimKey(kind, id), ttl).Err(); err != nil {
					log.Printf("asyncjob: %s: renew claim %s: %v", kind, id, err)
				}
			}
		}
	}()
	err := handler(ctx)
	stop()
	<-done
	return err
}
