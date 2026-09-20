package writing

import "context"

// WorkExists reads the primary so deletion cancels work on every replica.
func (s *MySQLStore) WorkExists(ctx context.Context, userID, id string) (bool, error) {
	var exists bool
	err := s.rw.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+promptsTable+` WHERE user_id=? AND id=?)`, userID, id).Scan(&exists)
	return exists, err
}
