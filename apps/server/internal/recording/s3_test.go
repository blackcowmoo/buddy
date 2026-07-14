package recording

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/store"
)

const testBucket = "buddy-recordings-test"

// sharedStore backs every container test below, started once in TestMain —
// same rationale as internal/store's own mysql_test.go: real S3/SQL
// semantics (gzip round trip, per-user scoping, the JOIN-free two-table
// lookup in Open) need real backends, but a fresh pair of containers per test
// would needlessly multiply CI time since every test scopes its own writes
// by a unique user ID.
var (
	sharedStore    *S3Store
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
		// so container tests skip themselves instead of failing the suite, but
		// still run the pure-function tests in wav_test.go.
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

	st, err := NewS3(ctx, S3Config{
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
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(user, pass, "")),
	)
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(testBucket)})
	return err
}

func requireStore(t *testing.T) *S3Store {
	t.Helper()
	if sharedStoreErr != nil {
		t.Skipf("mysql/minio testcontainers unavailable (no/unreachable Docker?): %v", sharedStoreErr)
	}
	return sharedStore
}

func TestSaveThenListRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	pcm := make([]byte, 32000) // 1s @ 16kHz mono 16-bit
	for i := range pcm {
		pcm[i] = byte(i)
	}

	rec, err := st.Save(ctx, "alex-list", pcm, 16000)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if rec.DurationMS != 1000 {
		t.Errorf("DurationMS = %d, want 1000", rec.DurationMS)
	}
	if rec.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0", rec.SizeBytes)
	}

	list, err := st.List(ctx, "alex-list")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("List() = %+v, want exactly rec %+v", list, rec)
	}
}

func TestListOrdersMostRecentFirst(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	pcm := make([]byte, 100)

	first, err := st.Save(ctx, "alex-order", pcm, 16000)
	if err != nil {
		t.Fatalf("Save() #1 error = %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // created_at has 1s resolution (UNIX seconds)
	second, err := st.Save(ctx, "alex-order", pcm, 16000)
	if err != nil {
		t.Fatalf("Save() #2 error = %v", err)
	}

	list, err := st.List(ctx, "alex-order")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("List() order = %+v, want [second, first]", list)
	}
}

func TestOpenReturnsDecompressibleAudio(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	rec, err := st.Save(ctx, "alex-open", pcm, 16000)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, body, err := st.Open(ctx, "alex-open", rec.ID)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer body.Close()
	if got.ID != rec.ID || got.DurationMS != rec.DurationMS {
		t.Errorf("Open() metadata = %+v, want %+v", got, rec)
	}

	gz, err := gzip.NewReader(body)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	wav, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	if !bytes.Equal(wav[44:], pcm) {
		t.Errorf("decompressed WAV payload = %v, want %v", wav[44:], pcm)
	}
}

func TestOpenRejectsWrongUser(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	rec, err := st.Save(ctx, "owner", []byte{1, 2, 3, 4}, 16000)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, _, err := st.Open(ctx, "someone-else", rec.ID); err == nil {
		t.Fatal("Open() with the wrong userID should error, got nil")
	}
}

func TestListUnknownUserReturnsEmpty(t *testing.T) {
	st := requireStore(t)
	list, err := st.List(context.Background(), "nobody-has-recordings")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() = %+v, want empty", list)
	}
}
