package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
)

func TestDeletedSessionCannotBeRecreatedByLateWork(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user, id := t.Name(), "room"
	cleanupLifetimeUser(t, st, user)
	cleanupLifetimeUser(t, st, user+"other")
	if err := st.SaveTurn(ctx, user, id, 1, "user", "hello", false, "text"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveAssistantTurn(ctx, user, id, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveCorrectionJob(ctx, user, id, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(ctx, user, id); err != nil {
		t.Fatal(err)
	}
	for name, write := range map[string]func() error{
		"greeting":               func() error { return st.SaveTurn(ctx, user, id, 0, "assistant", "late greeting", false, "") },
		"first turn":             func() error { return st.SaveTurn(ctx, user, id, 1, "user", "late input", false, "text") },
		"later turn":             func() error { return st.SaveTurn(ctx, user, id, 3, "assistant", "late reply", false, "") },
		"title":                  func() error { return st.SaveGeneratedTitle(ctx, user, id, "late title") },
		"instant":                func() error { return st.MarkInstant(ctx, user, id) },
		"reply reservation":      func() error { return st.ReserveAssistantTurn(ctx, user, id, 2) },
		"greeting completion":    func() error { return st.CompleteAssistantTurn(ctx, user, id, 0, "late greeting") },
		"correction reservation": func() error { return st.ReserveCorrectionJob(ctx, user, id, 2) },
		"profile snapshot":       func() error { return st.PublishLearnerProfile(ctx, user, []string{id}, 0, "deleted context") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, workguard.ErrDeleted) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, table := range []string{sessionsTable, turnsTable, jobsTable} {
		var n int
		if err := st.rw.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE user_id=?", user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s has %d surviving rows", table, n)
		}
	}
	// Owner scoping must not prevent another user's independent room.
	if err := st.SaveTurn(ctx, user+"other", id, 0, "assistant", "hello", false, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDeletedUnstartedSessionCannotBeCreated(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	cleanupLifetimeUser(t, st, user)
	if err := st.DeleteSession(ctx, user, "new"); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteAssistantTurn(ctx, user, "new", 0, "late"); !errors.Is(err, workguard.ErrDeleted) {
		t.Fatalf("error=%v", err)
	}
	if alive, err := st.WorkExists(ctx, user, "another"); err != nil || !alive {
		t.Fatalf("new room alive=%v err=%v", alive, err)
	}
}

func TestSessionDeletionWaitsForEarlierDerivedWrite(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	cleanupLifetimeUser(t, st, user)
	if err := st.SaveTurn(ctx, user, "room", 1, "user", "hello", false, ""); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- st.WorkCommit(ctx, user, "room", func(context.Context) error {
			close(locked)
			<-release
			return st.SaveLearnerProfile(ctx, user, "earlier result")
		})
	}()
	<-locked
	deleted := make(chan error, 1)
	go func() { deleted <- st.DeleteSession(ctx, user, "room") }()
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	called := false
	err := st.WorkCommit(ctx, user, "room", func(context.Context) error { called = true; return nil })
	if !errors.Is(err, workguard.ErrDeleted) || called {
		t.Fatalf("late effect: called=%v err=%v", called, err)
	}
}

func cleanupLifetimeUser(t *testing.T, st *MySQLStore, user string) {
	t.Helper()
	t.Cleanup(func() {
		for _, table := range []string{turnsTable, jobsTable, sessionsTable, lifetimesTable, settingsTable} {
			if _, err := st.rw.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE user_id=?", user); err != nil {
				t.Error(err)
			}
		}
	})
}

func TestProfilePublicationCannotRestoreDeletedSessionThroughAnotherRoom(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	cleanupLifetimeUser(t, st, user)
	for _, id := range []string{"deleted", "surviving"} {
		if err := st.SaveTurn(ctx, user, id, 1, "user", "hello", false, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.EndSession(ctx, user, id); err != nil {
			t.Fatal(err)
		}
		if err := st.CompleteStudySummary(ctx, user, id, []protocol.StudySummarySentence{{English: id, Translation: "요약"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveLearnerProfile(ctx, user, "contains both rooms"); err != nil {
		t.Fatal(err)
	}
	_, revision, err := st.GetLearnerProfileSnapshot(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	contributed, err := st.DeleteSessionWithProfileState(ctx, user, "deleted")
	if err != nil || !contributed {
		t.Fatalf("contribution=%v err=%v", contributed, err)
	}
	profile, current, err := st.GetLearnerProfileSnapshot(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if profile != "" || current <= revision {
		t.Fatalf("deleted data was not invalidated: %q %d", profile, current)
	}
	// A fold owned by a different, still-existing room has stale aggregate input.
	if err := st.PublishLearnerProfile(ctx, user, []string{"surviving"}, revision, "restores deleted room"); !errors.Is(err, ErrProfileChanged) {
		t.Fatalf("stale aggregate published: %v", err)
	}
	if err := st.PublishLearnerProfile(ctx, user, []string{"surviving"}, current, "surviving room only"); err != nil {
		t.Fatal(err)
	}
	if err := st.PublishLearnerProfile(ctx, user, []string{"surviving"}, current, "late concurrent fold"); !errors.Is(err, ErrProfileChanged) {
		t.Fatalf("concurrent aggregate overwrote winner: %v", err)
	}
}

func TestSessionDerivedWriteUsesOneConnectionAndRollsBackAtomically(t *testing.T) {
	requireStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := openPool(sharedStoreConfig, sharedStoreConfig.RWHost)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	words, err := wordreview.NewMySQL(ctx, db, db)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	st := &MySQLStore{rw: db, ro: db}
	user := t.Name()
	cleanupLifetimeUser(t, st, user)
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM buddy_word_reviews WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	if err := st.SaveTurn(ctx, user, "room", 1, "user", "hello", false, ""); err != nil {
		t.Fatal(err)
	}

	rollback := errors.New("discard derived writes")
	err = st.WorkCommit(ctx, user, "room", func(ctx context.Context) error {
		// Rebinding a parent scope must reuse its transaction too.
		return st.WorkCommit(ctx, user, "room", func(ctx context.Context) error {
			if _, err := words.Save(ctx, user, "rollback", "meaning", "example"); err != nil {
				return err
			}
			if err := st.SaveLearnerProfile(ctx, user, "rollback"); err != nil {
				return err
			}
			return rollback
		})
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("write used another connection or failed: %v", err)
	}
	if list, err := words.List(ctx, user); err != nil || len(list) != 0 {
		t.Fatalf("rolled-back vocabulary=%v err=%v", list, err)
	}
	if profile, err := st.GetLearnerProfile(ctx, user); err != nil || profile != "" {
		t.Fatalf("rolled-back profile=%q err=%v", profile, err)
	}

	if err := st.WorkCommit(ctx, user, "room", func(ctx context.Context) error {
		_, err := words.Save(ctx, user, "saved", "meaning", "example")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if list, err := words.List(ctx, user); err != nil || len(list) != 1 {
		t.Fatalf("committed vocabulary=%v err=%v", list, err)
	}
}
