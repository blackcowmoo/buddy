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

func TestSchemaIncludesArticleInstanceSelectionColumn(t *testing.T) {
	st := requireStore(t)
	var count int
	err := st.ro.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?
	`, instancesTable, "selected_options_json").Scan(&count)
	if err != nil {
		t.Fatalf("check selected_options_json schema: %v", err)
	}
	if count != 1 {
		t.Fatalf("selected_options_json column count = %d, want 1", count)
	}
}

// testSubQuestions returns a fixed, valid 2-entry SubQuestions slice — the
// minimum articleQuizMinSubQuestions requires — for tests that just need
// *a* valid quiz to build an Instance against, not to exercise its content.
func testSubQuestions() []SubQuestion {
	return []SubQuestion{
		{Prompt: "질문 1", Options: []string{"정답 1", "오답 1"}, CorrectOptionIndex: 0, Explanation: "설명 1"},
		{Prompt: "질문 2", Options: []string{"정답 2", "오답 2"}, CorrectOptionIndex: 1, Explanation: "설명 2"},
	}
}

// correctOptions returns the all-correct selection for qs — Answer(ctx,
// ..., correctOptions(article.SubQuestions)) is what a learner who
// answered every sub-question right would have submitted.
func correctOptions(qs []SubQuestion) []int {
	out := make([]int, len(qs))
	for i, q := range qs {
		out[i] = q.CorrectOptionIndex
	}
	return out
}

// mustSaveArticle reserves a fresh Article row and immediately completes it
// with study content — the two-step ReserveArticle + CompleteArticle
// sequence httpserver.articleDrawHandler and asyncjob.KindArticleStudy
// perform separately, collapsed into one helper for tests that just want an
// already-StatusDone Article to build an Instance against.
func mustSaveArticle(t *testing.T, st *MySQLStore, url string) Article {
	t.Helper()
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Test headline", url, "A short test snippet.", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	completed, err := st.CompleteArticle(context.Background(), reserved.ID,
		"A short English study paragraph about the test headline.",
		"테스트 헤드라인에 관한 짧은 영어 학습 문단입니다.",
		testSubQuestions())
	if err != nil {
		t.Fatalf("CompleteArticle() error = %v", err)
	}
	return completed
}

func TestReserveArticleDedupesByURL(t *testing.T) {
	st := requireStore(t)
	url := "https://example.com/dedupe-test"

	first := mustSaveArticle(t, st, url)

	second, err := st.ReserveArticle(context.Background(), "NPR", "A different title", url, "A different snippet.", time.Time{})
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
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Brand new", "https://example.com/fresh-reserve", "A fresh snippet.", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if reserved.Status != StatusPending {
		t.Fatalf("ReserveArticle() Status = %q, want %q for a never-seen URL", reserved.Status, StatusPending)
	}
	if reserved.Summary != "" || len(reserved.SubQuestions) != 0 {
		t.Errorf("ReserveArticle() = %+v, want empty study content while pending", reserved)
	}
}

func TestCompleteArticleIsNoopIfNoLongerPending(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Race", "https://example.com/complete-race", "A race snippet.", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	first, err := st.CompleteArticle(context.Background(), reserved.ID, "first summary", "첫 번역", testSubQuestions())
	if err != nil {
		t.Fatalf("CompleteArticle() #1 error = %v", err)
	}
	otherSubQuestions := []SubQuestion{
		{Prompt: "다른 질문", Options: []string{"w", "x"}, CorrectOptionIndex: 1, Explanation: "e2"},
		{Prompt: "또 다른 질문", Options: []string{"y", "z"}, CorrectOptionIndex: 0, Explanation: "e3"},
	}
	second, err := st.CompleteArticle(context.Background(), reserved.ID, "second summary", "두 번째 번역", otherSubQuestions)
	if err != nil {
		t.Fatalf("CompleteArticle() #2 error = %v", err)
	}
	if second.Summary != first.Summary || len(second.SubQuestions) != len(first.SubQuestions) || second.SubQuestions[0].Prompt != first.SubQuestions[0].Prompt {
		t.Fatalf("CompleteArticle() #2 = %+v, want the first completion's content kept unchanged", second)
	}
}

func TestFailArticleMarksFailedOnlyWhilePending(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Failure", "https://example.com/fail-article", "A failure snippet.", time.Time{})
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
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Has description", "https://example.com/description-test", "The feed's own short snippet.", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if reserved.Description != "The feed's own short snippet." {
		t.Fatalf("ReserveArticle() Description = %q, want the snippet passed in", reserved.Description)
	}
}

// TestReserveArticlePersistsPublishedAt guards the round trip this feature's
// UI date display depends on: the source feed's own publish time must
// survive ReserveArticle -> GetArticle/List/Get, distinct from CreatedAt
// (when the row was reserved, not when the story was actually published).
func TestReserveArticlePersistsPublishedAt(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	published := time.Date(2024, 3, 15, 9, 30, 0, 0, time.UTC)

	reserved, err := st.ReserveArticle(ctx, "BBC", "Has a pub date", "https://example.com/published-at-test", "snippet", published)
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if !reserved.PublishedAt.Equal(published) {
		t.Fatalf("ReserveArticle() PublishedAt = %v, want %v", reserved.PublishedAt, published)
	}

	fetched, ok, err := st.GetArticle(ctx, reserved.ID)
	if err != nil || !ok {
		t.Fatalf("GetArticle() = (%+v, %v, %v)", fetched, ok, err)
	}
	if !fetched.PublishedAt.Equal(published) {
		t.Fatalf("GetArticle() PublishedAt = %v, want %v", fetched.PublishedAt, published)
	}
}

// TestReserveArticleZeroPublishedAtStaysZero guards the "feed had no
// parseable <pubDate>" case (see newsfeed.Candidate.PublishedAt's doc
// comment): a zero time.Time must round-trip as zero, not as some large
// negative unix timestamp from time.Time{}.Unix().
func TestReserveArticleZeroPublishedAtStaysZero(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	reserved, err := st.ReserveArticle(ctx, "BBC", "No pub date", "https://example.com/no-published-at-test", "snippet", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if !reserved.PublishedAt.IsZero() {
		t.Fatalf("ReserveArticle() PublishedAt = %v, want the zero time.Time", reserved.PublishedAt)
	}
}

// TestStalePendingExcludesFreshlyReservedRows guards against the sweep
// (transport.SweepStaleArticleStudies) firing on a draw that's simply still
// legitimately generating: a row's claimed_at starts equal to created_at, so
// it must not show up as stale immediately.
func TestStalePendingExcludesFreshlyReservedRows(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	fresh, err := st.ReserveArticle(ctx, "BBC", "Fresh", "https://example.com/stale-fresh", "fresh snippet", time.Time{})
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
	old, err := st.ReserveArticle(ctx, "BBC", "Old", "https://example.com/stale-old", "old snippet", time.Time{})
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
	reserved, err := st.ReserveArticle(ctx, "BBC", "Claim", "https://example.com/claim-test", "claim snippet", time.Time{})
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

	if _, err := st.CompleteArticle(ctx, reserved.ID, "s", "번역", testSubQuestions()); err != nil {
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
	reserved, err := st.ReserveArticle(ctx, "BBC", "Reclaim", "https://example.com/reclaim-test", "reclaim snippet", time.Time{})
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

// TestReopenIncompleteArticleWinsForADoneArticleWithNoSubQuestions guards
// the self-heal path (see httpserver.selfHealIncompleteArticle): an Article
// left StatusDone with no SubQuestions — the shape a row completed before
// the 4-choice -> N-sub-question redesign is stuck in — must be reopened so
// it can be regenerated. CompleteArticle itself doesn't validate an empty
// subQuestions slice (that's pipeline.validateArticleStudy's job, one layer
// up), so calling it directly with nil is exactly how to build this shape
// in a test without needing to touch the DB by hand.
func TestReopenIncompleteArticleWinsForADoneArticleWithNoSubQuestions(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	reserved, err := st.ReserveArticle(ctx, "BBC", "Stale", "https://example.com/reopen-empty", "snippet", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}
	if _, err := st.CompleteArticle(ctx, reserved.ID, "old summary", "오래된 번역", nil); err != nil {
		t.Fatalf("CompleteArticle() error = %v", err)
	}

	won, err := st.ReopenIncompleteArticle(ctx, reserved.ID)
	if err != nil {
		t.Fatalf("ReopenIncompleteArticle() error = %v", err)
	}
	if !won {
		t.Fatal("ReopenIncompleteArticle() = false, want true for a done article with no sub-questions")
	}

	found, ok, err := st.GetArticle(ctx, reserved.ID)
	if err != nil || !ok {
		t.Fatalf("GetArticle() = (%+v, %v, %v)", found, ok, err)
	}
	if found.Status != StatusPending {
		t.Fatalf("Status after reopen = %q, want %q", found.Status, StatusPending)
	}
}

// TestReopenIncompleteArticleIsNoopForARealDoneArticle guards against ever
// clobbering a genuinely completed quiz back to pending.
func TestReopenIncompleteArticleIsNoopForARealDoneArticle(t *testing.T) {
	st := requireStore(t)
	article := mustSaveArticle(t, st, "https://example.com/reopen-real")

	won, err := st.ReopenIncompleteArticle(context.Background(), article.ID)
	if err != nil {
		t.Fatalf("ReopenIncompleteArticle() error = %v", err)
	}
	if won {
		t.Fatal("ReopenIncompleteArticle() = true, want false for an article that already has real sub-questions")
	}

	found, _, err := st.GetArticle(context.Background(), article.ID)
	if err != nil {
		t.Fatalf("GetArticle() error = %v", err)
	}
	if found.Status != StatusDone {
		t.Fatalf("Status = %q, want unchanged %q", found.Status, StatusDone)
	}
}

// TestReopenIncompleteArticleIsNoopWhilePending guards against interfering
// with a normal, still-in-flight (never-yet-completed) generation.
func TestReopenIncompleteArticleIsNoopWhilePending(t *testing.T) {
	st := requireStore(t)
	reserved, err := st.ReserveArticle(context.Background(), "BBC", "Still pending", "https://example.com/reopen-pending", "snippet", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}

	won, err := st.ReopenIncompleteArticle(context.Background(), reserved.ID)
	if err != nil {
		t.Fatalf("ReopenIncompleteArticle() error = %v", err)
	}
	if won {
		t.Fatal("ReopenIncompleteArticle() = true, want false for an article that's still pending")
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
	if created.Answered || len(created.SelectedOptions) != 0 {
		t.Errorf("CreateInstance() = %+v, want a fresh unanswered instance", created)
	}
	if created.Article.URL != article.URL || created.Article.Translation != article.Translation || len(created.Article.SubQuestions) != len(testSubQuestions()) {
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

// TestCreateInstanceListAndGetAllowMissingArticleTranslation guards the draw
// path: the instance is created while article study generation is still
// pending, so translation is legitimately SQL NULL until the async job
// completes. Joined instance reads must preserve that as an empty string.
func TestCreateInstanceListAndGetAllowMissingArticleTranslation(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	article, err := st.ReserveArticle(ctx, "BBC", "Pending translation", "https://example.com/pending-translation", "snippet", time.Time{})
	if err != nil {
		t.Fatalf("ReserveArticle() error = %v", err)
	}

	created, err := st.CreateInstance(ctx, "alex-pending-translation", article.ID)
	if err != nil {
		t.Fatalf("CreateInstance() error = %v", err)
	}
	if created.Article.Translation != "" {
		t.Fatalf("CreateInstance() Translation = %q, want empty while generation is pending", created.Article.Translation)
	}

	got, err := st.Get(ctx, "alex-pending-translation", created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Article.Translation != "" {
		t.Fatalf("Get() Translation = %q, want empty for SQL NULL", got.Article.Translation)
	}

	list, err := st.List(ctx, "alex-pending-translation")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Article.Translation != "" {
		t.Fatalf("List() = %+v, want one instance with empty translation", list)
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

	allCorrect := correctOptions(article.SubQuestions)
	answered, err := st.Answer(ctx, "alex-answer", inst.ID, allCorrect)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if !answered.Answered || !answered.Correct || len(answered.SelectedOptions) != len(allCorrect) {
		t.Fatalf("Answer() with every correct option = %+v, want Answered/Correct true", answered)
	}

	// A second Answer call — even with different (wrong) selections — must
	// not overwrite the first recorded outcome (see Store.Answer's one-shot
	// doc).
	wrong := make([]int, len(allCorrect))
	for i, v := range allCorrect {
		wrong[i] = 1 - v
	}
	again, err := st.Answer(ctx, "alex-answer", inst.ID, wrong)
	if err != nil {
		t.Fatalf("Answer() #2 error = %v", err)
	}
	if !again.Correct {
		t.Errorf("Answer() #2 = %+v, want the original (correct) outcome unchanged", again)
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

	// Every sub-question right except the first one, which is deliberately
	// wrong — Correct must require ALL of them, not just most.
	selections := correctOptions(article.SubQuestions)
	selections[0] = 1 - selections[0]
	answered, err := st.Answer(ctx, "alex-answer-wrong", inst.ID, selections)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if answered.Correct {
		t.Fatalf("Answer() with one wrong sub-question = %+v, want Correct = false", answered)
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
