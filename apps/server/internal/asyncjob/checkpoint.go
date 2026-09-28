package asyncjob

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Checkpoints belong to the durable job, rather than its attempt or dedupe
// key: retries share completed work, while a later request starts fresh.
// They have no TTL while the job is pending, so a prolonged outage cannot
// expire expensive completed model work. An invalid step can be removed
// independently; terminal cleanup removes all of the job's checkpoints.
func checkpointKey(kind Kind, jobID string) string {
	return fmt.Sprintf("buddy:job:{%s}:checkpoint:%s", kind, jobID)
}

var loadCheckpointScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return {0, false}
end
return {1, redis.call('HGET', KEYS[2], ARGV[2])}
`)

var saveCheckpointScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return 0
end
redis.call('HSETNX', KEYS[2], ARGV[2], ARGV[3])
return 1
`)

var deleteCheckpointScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return 0
end
redis.call('HDEL', KEYS[2], ARGV[2])
return 1
`)

type redisCheckpointStore struct {
	rdb   redis.UniversalClient
	kind  Kind
	id    string
	token string
}

func (s redisCheckpointStore) Load(ctx context.Context, key string) (string, bool, error) {
	result, err := loadCheckpointScript.Run(ctx, s.rdb,
		[]string{claimKey(s.kind, s.id), checkpointKey(s.kind, s.id)}, s.token, key,
	).Slice()
	if err != nil {
		return "", false, fmt.Errorf("load job checkpoint: %w", err)
	}
	if result[0] != int64(1) {
		return "", false, ErrClaimLost
	}
	if result[1] == nil {
		return "", false, nil
	}
	return result[1].(string), true, nil
}

func (s redisCheckpointStore) Save(ctx context.Context, key, value string) error {
	owned, err := saveCheckpointScript.Run(ctx, s.rdb,
		[]string{claimKey(s.kind, s.id), checkpointKey(s.kind, s.id)}, s.token, key, value,
	).Int()
	if err != nil {
		return fmt.Errorf("save job checkpoint: %w", err)
	}
	if owned != 1 {
		return ErrClaimLost
	}
	return nil
}

func (s redisCheckpointStore) Delete(ctx context.Context, key string) error {
	owned, err := deleteCheckpointScript.Run(ctx, s.rdb,
		[]string{claimKey(s.kind, s.id), checkpointKey(s.kind, s.id)}, s.token, key,
	).Int()
	if err != nil {
		return fmt.Errorf("delete job checkpoint: %w", err)
	}
	if owned != 1 {
		return ErrClaimLost
	}
	return nil
}
