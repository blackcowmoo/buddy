package newsarticle

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/store"
)

// sharedStore backs every container test in this file, started once in
// TestMain — same rationale as internal/wordreview's own mysql_test.go: real
// SQL semantics (the UNIQUE-key upsert in ReserveArticle, the join in
// List/Get) need a real database, but a fresh container per test would
// needlessly multiply CI time since every test below scopes its own writes
// by a unique user ID or article URL.
var (
	sharedStore    *MySQLStore
	sharedStoreErr error
)

func TestMain(m *testing.M) {
	os.Exit(runContainerTests(m))
}

func runContainerTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	container, err := tcmysql.Run(ctx, "mysql:8.0",
		tcmysql.WithDatabase("buddy"),
		tcmysql.WithUsername("buddy"),
		tcmysql.WithPassword("buddy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		// No/unreachable Docker (sandboxed CI, restricted dev box): record why
		// so container tests skip themselves instead of failing the suite.
		sharedStoreErr = err
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	host, err := container.Host(ctx)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	port, err := container.MappedPort(ctx, "3306/tcp")
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}

	// newsarticle shares internal/store's MySQLStore pools in production (see
	// cmd/server/main.go's buildNewsArticleStore) — go through the same
	// constructor here so the test exercises the real connection-sharing path.
	profileStore, err := store.NewMySQL(store.MySQLConfig{
		RWHost:   host,
		Port:     int(port.Num()),
		User:     "buddy",
		Password: "buddy",
		Database: "buddy",
	})
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	defer func() { _ = profileStore.Close() }()
	rw, ro := profileStore.DB()

	st, err := NewMySQL(ctx, rw, ro)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	sharedStore = st
	return m.Run()
}

func requireStore(t *testing.T) *MySQLStore {
	t.Helper()
	if sharedStoreErr != nil {
		t.Skipf("mysql testcontainer unavailable (no/unreachable Docker?): %v", sharedStoreErr)
	}
	return sharedStore
}

// mustSaveArticle reserves a fresh Article row and immediately completes it
// with study content — the two-step ReserveArticle + CompleteArticle
// sequence httpserver.articleDrawHandler and asyncjob.KindArticleStudy
// perform separately, collapsed into one helper for tests that just want an
// already-StatusDone Article to build an Instance against.
func mustSaveArticle(t *testing.T, st *MySQLStore, url string) Article {
	t.Helper()
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Test headline", url, "A short test snippet.")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	completed, err := st.CompleteArticle(context.Background(), reserved.ID,
		"A short English study paragraph about the test headline.",
		[]string{"정확한 해석", "틀린 해석 1", "틀린 해석 2", "틀린 해석 3"}, 0,
		"정확한 해석이 원문의 의미를 그대로 담고 있기 때문이다.")
	if err != nil {
		t.Fatalf("CompleteArticle() error = %v", err)
	}
	return completed
}

func TestReserveArticleDedupesByURL(t *testing.T) {
	st := requireStore(t)
	url := "https://example.com/dedupe-test"

	first := mustSaveArticle(t, st, url)

	second, err := st.ReserveArticle(context.Background(), "NPR", "A different title", url, "A different snippet.")
	if err != nil {
		t.Fatalf("ReserveArticle() #2 error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("ReserveArticle() #2 ID = %q, want the same cached row %q", second.ID, first.ID)
	}
	if second.Source != "BBC" || second.Status != StatusDone {
		t.Errorf("ReserveArticle() #2 = %+v, want the original completed fields kept, not overwritten", second)
	}

	found, ok, err := st.GetArticle(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if !ok || found.ID != first.ID {
		t.Fatalf("GetArticle() = (%+v, %v), want the cached row", found, ok)
	}
}

func TestReserveArticleStartsPendingForAFreshURL(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Brand new", "https://example.com/fresh-reserve", "A fresh snippet.")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if reserved.Status != StatusPending {
		t.Fatalf("ReserveArticle() Status = %q, want %q for a never-seen URL", reserved.Status, StatusPending)
	}
	if reserved.Summary != "" || len(reserved.Choices) != 0 {
		t.Errorf("ReserveArticle() = %+v, want empty study content while pending", reserved)
	}
}

func TestCompleteArticleIsNoopIfNoLongerPending(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Race", "https://example.com/complete-race", "A race snippet.")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	first, err := st.CompleteArticle(context.Background(), reserved.ID, "first summary", []string{"a", "b", "c", "d"}, 1, "e1")
	if err != nil {
		t.Fatalf("CompleteArticle() #1 error = %v", err)
	}
	second, err := st.CompleteArticle(context.Background(), reserved.ID, "second summary", []string{"w", "x", "y", "z"}, 3, "e2")
	if err != nil {
		t.Fatalf("CompleteArticle() #2 error = %v", err)
	}
	if second.Summary != first.Summary || second.CorrectIndex != first.CorrectIndex {
		t.Fatalf("CompleteArticle() #2 = %+v, want the first completion's content kept unchanged", second)
	}
}

func TestFailArticleMarksFailedOnlyWhilePending(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Failure", "https://example.com/fail-article", "A failure snippet.")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if err := st.FailArticle(context.Background(), reserved.ID); err != nil {
		t.Fatalf("FailArticle() error = %v", err)
	}
	failed, ok, err := st.GetArticle(context.Background(), reserved.ID)
	if err != nil || !ok {
		t.Fatalf("GetArticle() = (%+v, %v, %v)", failed, ok, err)
	}
	if failed.Status != StatusFailed {
		t.Fatalf("Status after FailArticle() = %q, want %q", failed.Status, StatusFailed)
	}

	// A second FailArticle call is a no-op (the WHERE guards on
	// StatusPending) — mirrors CompleteArticle's own idempotence.
	if err := st.FailArticle(context.Background(), reserved.ID); err != nil {
		t.Fatalf("FailArticle() #2 error = %v", err)
	}
}

func TestGetArticleMissing(t *testing.T) {
	st := requireStore(t)
	_, ok, err := st.GetArticle(context.Background(), "never-reserved")
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if ok {
		t.Fatal("GetArticle() ok = true, want false for an id never reserved")
	}
}

// backdateClaim directly rewrites id's claimed_at column, simulating a
// generation attempt abandoned age ago — StalePending has no other way to
// observe staleness than the wall clock, so tests need this rather than
// sleeping out a real ArticleStudyStaleAfter-sized window.
func backdateClaim(t *testing.T, st *MySQLStore, id string, age time.Duration) {
	t.Helper()
	if _, err := st.rw.ExecContext(context.Background(),
		`UPDATE `+articlesTable+` SET claimed_at = ? WHERE id = ?`, time.Now().Add(-age).UnixNano(), id,
	); err != nil {
		t.Fatalf("backdateClaim(%s): %v", id, err)
	}
}

func TestReserveArticlePersistsDescription(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Has description", "https://example.com/description-test", "The feed's own short snippet.")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if reserved.Description != "The feed's own short snippet." {
		t.Fatalf("ReserveArticle() Description = %q, want the snippet passed in", reserved.Description)
	}
}

// TestStalePendingExcludesFreshlyReservedRows guards against the sweep
// (transport.SweepStaleArticleStudies) firing on a draw that's simply still
// legitimately generating: a row's claimed_at starts equal to created_at, so
// it must not show up as stale immediately.
func TestStalePendingExcludesFreshlyReservedRows(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	fresh, err := st.ReserveArticle(ctx, "BBC", "Fresh", "https://example.com/stale-fresh", "fresh snippet")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}

	stale, err := st.StalePending(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("StalePending() error = %v", err)
	}
	for _, a := range stale {
		if a.ID == fresh.ID {
			t.Fatalf("StalePending() unexpectedly included a freshly reserved article %s", fresh.ID)
		}
	}
}

// TestStalePendingIncludesRowsClaimedLongAgo guards the actual orphan-sweep
// signal: a StatusPending row whose claimed_at is old enough (simulating a
// generation attempt killed mid-job by a crash or redeploy, which never got
// to call CompleteArticle/FailArticle) must be reported, with its
// Description intact so the caller can regenerate without the original
// newsfeed.Candidate.
func TestStalePendingIncludesRowsClaimedLongAgo(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	old, err := st.ReserveArticle(ctx, "BBC", "Old", "https://example.com/stale-old", "old snippet")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	backdateClaim(t, st, old.ID, time.Hour)

	stale, err := st.StalePending(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("StalePending() error = %v", err)
	}
	var found bool
	for _, a := range stale {
		if a.ID != old.ID {
			continue
		}
		found = true
		if a.Description != "old snippet" {
			t.Errorf("StalePending() Description = %q, want the persisted snippet", a.Description)
		}
	}
	if !found {
		t.Fatalf("StalePending() = %+v, want it to include the backdated article %s", stale, old.ID)
	}
}

func TestClaimArticleReportsWhetherStillPending(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	reserved, err := st.ReserveArticle(ctx, "BBC", "Claim", "https://example.com/claim-test", "claim snippet")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}

	claimed, err := st.ClaimArticle(ctx, reserved.ID)
	if err != nil {
		t.Fatalf("ClaimArticle() error = %v", err)
	}
	if !claimed {
		t.Fatal("ClaimArticle() on a StatusPending article = false, want true")
	}

	if _, err := st.CompleteArticle(ctx, reserved.ID, "s", []string{"a", "b", "c", "d"}, 0, "e"); err != nil {
		t.Fatalf("CompleteArticle() error = %v", err)
	}

	claimedAfterDone, err := st.ClaimArticle(ctx, reserved.ID)
	if err != nil {
		t.Fatalf("ClaimArticle() #2 error = %v", err)
	}
	if claimedAfterDone {
		t.Fatal("ClaimArticle() after CompleteArticle = true, want false")
	}
}

func TestClaimArticleMissingID(t *testing.T) {
	st := requireStore(t)
	claimed, err := st.ClaimArticle(context.Background(), "never-reserved")
	if err != nil {
		t.Fatalf("ClaimArticle() error = %v", err)
	}
	if claimed {
		t.Fatal("ClaimArticle() for a never-reserved id = true, want false")
	}
}

// TestClaimArticleRefreshesClaimedAtSoItLeavesStalePending is the
// re-claim-then-un-stale round trip SweepStaleArticleStudies relies on: once
// a sweep successfully claims a stale row to retry it, that row must not be
// reported stale again by the very next sweep tick.
func TestClaimArticleRefreshesClaimedAtSoItLeavesStalePending(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	reserved, err := st.ReserveArticle(ctx, "BBC", "Reclaim", "https://example.com/reclaim-test", "reclaim snippet")
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	backdateClaim(t, st, reserved.ID, time.Hour)

	staleBefore, err := st.StalePending(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("StalePending() error = %v", err)
	}
	var foundBefore bool
	for _, a := range staleBefore {
		if a.ID == reserved.ID {
			foundBefore = true
		}
	}
	if !foundBefore {
		t.Fatalf("StalePending() before claim = %+v, want it to include %s", staleBefore, reserved.ID)
	}

	claimed, err := st.ClaimArticle(ctx, reserved.ID)
	if err != nil || !claimed {
		t.Fatalf("ClaimArticle() = (%v, %v), want (true, nil)", claimed, err)
	}

	staleAfter, err := st.StalePending(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("StalePending() error = %v", err)
	}
	for _, a := range staleAfter {
		if a.ID == reserved.ID {
			t.Fatalf("StalePending() after ClaimArticle still includes %s, want the claim to refresh claimed_at", reserved.ID)
		}
	}
}

func TestCreateInstanceListAndGetRoundTrip(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	article := mustSaveArticle(t, st, "https://example.com/instance-roundtrip")

	created, err := st.CreateInstance(ctx, "alex-instances", article.ID)
	if err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}
	if created.Answered || created.SelectedIndex != -1 {
		t.Errorf("CreateInstance() = %+v, want a fresh unanswered instance", created)
	}
	if created.Article.URL != article.URL || len(created.Article.Choices) != 4 {
		t.Errorf("CreateInstance() Article = %+v, want the joined article populated", created.Article)
	}

	got, err := st.Get(ctx, "alex-instances", created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("Get() = %+v, want %+v", got, created)
	}

	list, err := st.List(ctx, "alex-instances")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("List() = %+v, want exactly the created instance", list)
	}
}

func TestGetMissingOrOtherUser(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	article := mustSaveArticle(t, st, "https://example.com/get-other-user")
	created, err := st.CreateInstance(ctx, "alex-owner", article.ID)
	if err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}

	got, err := st.Get(ctx, "someone-else", created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != "" {
		t.Fatalf("Get() for a different user = %+v, want zero Instance", got)
	}
}

func TestUsedURLsReflectsDrawnArticles(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	userID := "alex-used-urls"

	before, err := st.UsedURLs(ctx, userID)
	if err != nil {
		t.Fatalf("UsedURLs() error = %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("UsedURLs() before any draw = %v, want empty", before)
	}

	article := mustSaveArticle(t, st, "https://example.com/used-urls-test")
	if _, err := st.CreateInstance(ctx, userID, article.ID); err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}

	after, err := st.UsedURLs(ctx, userID)
	if err != nil {
		t.Fatalf("UsedURLs() error = %v", err)
	}
	if !after[article.URL] {
		t.Errorf("UsedURLs() = %v, want it to include %q", after, article.URL)
	}
}

func TestAnswerComputesCorrectnessServerSideAndIsOneShot(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	article := mustSaveArticle(t, st, "https://example.com/answer-test")
	inst, err := st.CreateInstance(ctx, "alex-answer", article.ID)
	if err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}

	answered, err := st.Answer(ctx, "alex-answer", inst.ID, article.CorrectIndex)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if !answered.Answered || !answered.Correct || answered.SelectedIndex != article.CorrectIndex {
		t.Fatalf("Answer() with the correct index = %+v, want Answered/Correct true", answered)
	}

	// A second Answer call — even with a different (wrong) index — must not
	// overwrite the first recorded outcome (see Store.Answer's one-shot doc).
	again, err := st.Answer(ctx, "alex-answer", inst.ID, article.CorrectIndex+1)
	if err != nil {
		t.Fatalf("Answer() #2 error = %v", err)
	}
	if again.SelectedIndex != article.CorrectIndex || !again.Correct {
		t.Errorf("Answer() #2 = %+v, want the original outcome unchanged", again)
	}
}

func TestAnswerWithWrongChoice(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	article := mustSaveArticle(t, st, "https://example.com/answer-wrong-test")
	inst, err := st.CreateInstance(ctx, "alex-answer-wrong", article.ID)
	if err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}

	wrongIndex := (article.CorrectIndex + 1) % len(article.Choices)
	answered, err := st.Answer(ctx, "alex-answer-wrong", inst.ID, wrongIndex)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if answered.Correct {
		t.Fatalf("Answer() with a wrong index = %+v, want Correct = false", answered)
	}
}

func TestDeleteRemovesInstanceOnly(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	article := mustSaveArticle(t, st, "https://example.com/delete-test")
	inst, err := st.CreateInstance(ctx, "alex-delete", article.ID)
	if err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}

	if err := st.Delete(ctx, "alex-delete", inst.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	got, err := st.Get(ctx, "alex-delete", inst.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != "" {
		t.Fatalf("Get() after Delete = %+v, want zero Instance", got)
	}

	// The shared Article row must survive — other learners' instances may
	// still reference it.
	stillCached, ok, err := st.GetArticle(ctx, article.ID)
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if !ok || stillCached.ID != article.ID {
		t.Fatalf("GetArticle() after deleting the instance = (%+v, %v), want the article still cached", stillCached, ok)
	}
}
