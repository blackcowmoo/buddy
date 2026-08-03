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
// SQL semantics (the UNIQUE-key upsert in SaveArticle, the join in List/Get)
// need a real database, but a fresh container per test would needlessly
// multiply CI time since every test below scopes its own writes by a unique
// user ID or article URL.
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

func mustSaveArticle(t *testing.T, st *MySQLStore, url string) Article {
	t.Helper()
	a, err := st.SaveArticle(context.Background(), Article{
		Source:       "BBC",
		Title:        "Test headline",
		URL:          url,
		Summary:      "A short English study paragraph about the test headline.",
		Choices:      []string{"정확한 해석", "틀린 해석 1", "틀린 해석 2", "틀린 해석 3"},
		CorrectIndex: 0,
		Explanation:  "정확한 해석이 원문의 의미를 그대로 담고 있기 때문이다.",
	})
	if err != nil {
		t.Fatalf("SaveArticle() error = %v", err)
	}
	return a
}

func TestSaveArticleDedupesByURL(t *testing.T) {
	st := requireStore(t)
	url := "https://example.com/dedupe-test"

	first := mustSaveArticle(t, st, url)

	second, err := st.SaveArticle(context.Background(), Article{
		Source: "NPR", Title: "A different title", URL: url,
		Summary: "A different summary entirely.",
		Choices: []string{"a", "b", "c", "d"}, CorrectIndex: 2,
		Explanation: "different",
	})
	if err != nil {
		t.Fatalf("SaveArticle() #2 error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("SaveArticle() #2 ID = %q, want the same cached row %q", second.ID, first.ID)
	}
	if second.Source != "BBC" || second.CorrectIndex != 0 {
		t.Errorf("SaveArticle() #2 = %+v, want the original cached fields kept, not overwritten", second)
	}

	found, ok, err := st.FindArticleByURL(context.Background(), url)
	if err != nil {
		t.Fatalf("FindArticleByURL() error = %v", err)
	}
	if !ok || found.ID != first.ID {
		t.Fatalf("FindArticleByURL() = (%+v, %v), want the cached row", found, ok)
	}
}

func TestFindArticleByURLMissing(t *testing.T) {
	st := requireStore(t)
	_, ok, err := st.FindArticleByURL(context.Background(), "https://example.com/never-saved")
	if err != nil {
		t.Fatalf("FindArticleByURL() error = %v", err)
	}
	if ok {
		t.Fatal("FindArticleByURL() ok = true, want false for a URL never saved")
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
	stillCached, ok, err := st.FindArticleByURL(ctx, article.URL)
	if err != nil {
		t.Fatalf("FindArticleByURL() error = %v", err)
	}
	if !ok || stillCached.ID != article.ID {
		t.Fatalf("FindArticleByURL() after deleting the instance = (%+v, %v), want the article still cached", stillCached, ok)
	}
}
