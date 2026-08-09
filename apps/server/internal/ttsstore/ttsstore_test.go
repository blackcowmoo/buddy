package ttsstore

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/s3util"
	"buddy/server/internal/store"
)

const testBucket = "buddy-tts-cache-test"

// sharedStore backs every container test below, started once in TestMain —
// see internal/recording/s3_test.go's identical rationale.
var (
	sharedStore    *Store
	sharedStoreErr error
)

func TestMain(m *testing.M) {
	os.Exit(runContainerTests(m))
}

func runContainerTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	mysqlC, err := tcmysql.Run(ctx, "mysql:8.0",
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
	defer func() { _ = mysqlC.Terminate(context.Background()) }()

	minioC, err := tcminio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z")
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	defer func() { _ = minioC.Terminate(context.Background()) }()

	mysqlHost, err := mysqlC.Host(ctx)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	mysqlPort, err := mysqlC.MappedPort(ctx, "3306/tcp")
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	profileStore, err := store.NewMySQL(store.MySQLConfig{
		RWHost:   mysqlHost,
		Port:     int(mysqlPort.Num()),
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

	endpoint, err := minioC.ConnectionString(ctx)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	endpoint = "http://" + endpoint

	if err := createTestBucket(ctx, endpoint, minioC.Username, minioC.Password); err != nil {
		sharedStoreErr = err
		return m.Run()
	}

	st, err := New(ctx, Config{
		Endpoint:  endpoint,
		PathStyle: true,
		AccessKey: minioC.Username,
		SecretKey: minioC.Password,
		Bucket:    testBucket,
	}, rw, ro)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}

	sharedStore = st
	return m.Run()
}

func createTestBucket(ctx context.Context, endpoint, user, pass string) error {
	client, err := s3util.NewClient(endpoint, true, user, pass)
	if err != nil {
		return err
	}
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(testBucket)})
	return err
}

func requireStore(t *testing.T) *Store {
	t.Helper()
	if sharedStoreErr != nil {
		t.Skipf("mysql/minio testcontainers unavailable (no/unreachable Docker?): %v", sharedStoreErr)
	}
	return sharedStore
}

func TestPutThenOpenRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "article:round-trip", "v1", []byte("fake-mp3-bytes")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	rc, err := st.Open(ctx, "article:round-trip", "v1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "fake-mp3-bytes" {
		t.Fatalf("Open() body = %q, want %q", got, "fake-mp3-bytes")
	}
}

func TestOpenMissingKeyReturnsErrNotFound(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	_, err := st.Open(ctx, "article:never-generated", "v1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open() error = %v, want ErrNotFound", err)
	}
}

func TestPutOverwritesExistingKey(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "message:overwrite", "v1", []byte("first")); err != nil {
		t.Fatalf("Put() #1 error = %v", err)
	}
	if err := st.Put(ctx, "message:overwrite", "v1", []byte("second-and-longer")); err != nil {
		t.Fatalf("Put() #2 error = %v", err)
	}

	rc, err := st.Open(ctx, "message:overwrite", "v1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "second-and-longer" {
		t.Fatalf("Open() body = %q, want the overwritten value %q", got, "second-and-longer")
	}
}

// TestOpenWithDifferentVersionActsAsNotFoundAndDeletesTheStaleEntry guards
// the whole point of versioning: a settings change (e.g. TTSVolumeMultiplier)
// changes internal/tts.Kokoro.Version(), so an entry cached under the old
// version must be treated as absent — and, unlike a plain cache miss, the
// stale object/row is actively deleted (not left for SweepExpired, which
// could be up to TTL away) so it doesn't linger unreachable in S3.
func TestOpenWithDifferentVersionActsAsNotFoundAndDeletesTheStaleEntry(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "article:versioned", "vol1.0", []byte("quiet-version")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	_, err := st.Open(ctx, "article:versioned", "vol2.0")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open() with a different version error = %v, want ErrNotFound", err)
	}

	// The stale entry must actually be gone, not just skipped this once —
	// otherwise every future request pays the delete-detection cost again.
	var count int
	if err := st.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE cache_key = ?`, "article:versioned").Scan(&count); err != nil {
		t.Fatalf("query row count: %v", err)
	}
	if count != 0 {
		t.Fatalf("row count for the stale entry = %d, want 0 (deleted)", count)
	}

	// Regenerating under the new version and re-Put()ing must work cleanly
	// (no leftover unique-key conflict from the old row).
	if err := st.Put(ctx, "article:versioned", "vol2.0", []byte("louder-version")); err != nil {
		t.Fatalf("Put() after version invalidation error = %v", err)
	}
	rc, err := st.Open(ctx, "article:versioned", "vol2.0")
	if err != nil {
		t.Fatalf("Open() after regenerating at the new version error = %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "louder-version" {
		t.Fatalf("Open() body = %q, want %q", got, "louder-version")
	}
}

func TestSweepExpiredRemovesOnlyEntriesOlderThanTTL(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "article:sweep-old", "v1", []byte("old")); err != nil {
		t.Fatalf("Put() old error = %v", err)
	}
	// Backdate it directly — waiting out a real TTL in a test isn't practical.
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET created_at = ? WHERE cache_key = ?`,
		time.Now().Add(-100*24*time.Hour).Unix(), "article:sweep-old"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := st.Put(ctx, "article:sweep-fresh", "v1", []byte("fresh")); err != nil {
		t.Fatalf("Put() fresh error = %v", err)
	}

	n, err := st.SweepExpired(ctx, TTL)
	if err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepExpired() removed %d entries, want exactly 1 (the backdated one)", n)
	}

	if _, err := st.Open(ctx, "article:sweep-old", "v1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("sweep-old still openable after sweep: err = %v, want ErrNotFound", err)
	}
	if _, err := st.Open(ctx, "article:sweep-fresh", "v1"); err != nil {
		t.Errorf("sweep-fresh should survive the sweep: Open() error = %v", err)
	}
}

func TestSweepExpiredNoopWhenNothingExpired(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "article:sweep-noop", "v1", []byte("data")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	n, err := st.SweepExpired(ctx, TTL)
	if err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("SweepExpired() removed %d entries, want 0", n)
	}
}
