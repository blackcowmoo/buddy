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

	if err := st.Put(ctx, "article:round-trip", []byte("fake-mp3-bytes")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	rc, err := st.Open(ctx, "article:round-trip")
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

	_, err := st.Open(ctx, "article:never-generated")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open() error = %v, want ErrNotFound", err)
	}
}

func TestPutOverwritesExistingKey(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "message:overwrite", []byte("first")); err != nil {
		t.Fatalf("Put() #1 error = %v", err)
	}
	if err := st.Put(ctx, "message:overwrite", []byte("second-and-longer")); err != nil {
		t.Fatalf("Put() #2 error = %v", err)
	}

	rc, err := st.Open(ctx, "message:overwrite")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "second-and-longer" {
		t.Fatalf("Open() body = %q, want the overwritten value %q", got, "second-and-longer")
	}
}

func TestSweepExpiredRemovesOnlyEntriesOlderThanTTL(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "article:sweep-old", []byte("old")); err != nil {
		t.Fatalf("Put() old error = %v", err)
	}
	// Backdate it directly — waiting out a real TTL in a test isn't practical.
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET created_at = ? WHERE cache_key = ?`,
		time.Now().Add(-100*24*time.Hour).Unix(), "article:sweep-old"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := st.Put(ctx, "article:sweep-fresh", []byte("fresh")); err != nil {
		t.Fatalf("Put() fresh error = %v", err)
	}

	n, err := st.SweepExpired(ctx, TTL)
	if err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepExpired() removed %d entries, want exactly 1 (the backdated one)", n)
	}

	if _, err := st.Open(ctx, "article:sweep-old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("sweep-old still openable after sweep: err = %v, want ErrNotFound", err)
	}
	if _, err := st.Open(ctx, "article:sweep-fresh"); err != nil {
		t.Errorf("sweep-fresh should survive the sweep: Open() error = %v", err)
	}
}

func TestSweepExpiredNoopWhenNothingExpired(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if err := st.Put(ctx, "article:sweep-noop", []byte("data")); err != nil {
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
