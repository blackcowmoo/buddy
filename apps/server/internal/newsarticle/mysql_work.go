package newsarticle

import "context"

// Article generation belongs to all current instances. Deleting one learner's
// instance must not cancel work another learner still needs; the cached source
// alone is not evidence that anyone needs more generation.
func (s *MySQLStore) WorkExists(ctx context.Context, _ string, id string) (bool, error) {
	var exists bool
	err := s.rw.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+instancesTable+` WHERE article_id=?)`, id).Scan(&exists)
	return exists, err
}
