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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/s3util"
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
	client, err := s3util.NewClient(endpoint, true, user, pass)
	if err != nil {
		return err
	}
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

	rec, err := st.Save(ctx, "alex-list", "sess-list", uuid.NewString(), pcm, 16000)
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
	if list[0].SessionID != "sess-list" {
		t.Errorf("SessionID = %q, want sess-list", list[0].SessionID)
	}
}

func TestListOrdersMostRecentFirst(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	pcm := make([]byte, 100)

	first, err := st.Save(ctx, "alex-order", "sess-order", uuid.NewString(), pcm, 16000)
	if err != nil {
		t.Fatalf("Save() #1 error = %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // created_at has 1s resolution (UNIX seconds)
	second, err := st.Save(ctx, "alex-order", "sess-order", uuid.NewString(), pcm, 16000)
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

	rec, err := st.Save(ctx, "alex-open", "sess-open", uuid.NewString(), pcm, 16000)
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
	rec, err := st.Save(ctx, "owner", "sess-owner", uuid.NewString(), []byte{1, 2, 3, 4}, 16000)
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

func TestDeleteRemovesRowAndObject(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	rec, err := st.Save(ctx, "alex-delete", "sess-delete", uuid.NewString(), []byte{1, 2, 3, 4}, 16000)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	deleted, err := st.Delete(ctx, "alex-delete", rec.ID)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if deleted.ID != rec.ID || deleted.SessionID != "sess-delete" {
		t.Errorf("Delete() = %+v, want the deleted recording (id=%q, session=sess-delete)", deleted, rec.ID)
	}

	list, err := st.List(ctx, "alex-delete")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() after delete = %+v, want empty", list)
	}
	if _, _, err := st.Open(ctx, "alex-delete", rec.ID); err == nil {
		t.Fatal("Open() after delete should error (S3 object should be gone too), got nil")
	}
}

func TestDeleteIsNoopForUnknownID(t *testing.T) {
	st := requireStore(t)
	deleted, err := st.Delete(context.Background(), "alex-delete", "no-such-id")
	if err != nil {
		t.Fatalf("Delete() error = %v, want nil (no-op)", err)
	}
	if deleted.ID != "" {
		t.Errorf("Delete() = %+v, want zero Recording for an unknown id", deleted)
	}
}

// TestDeleteDoesNotAffectOtherUsers mirrors the recording package's
// user-isolation guarantee (see TestOpenRejectsWrongUser): a delete scoped to
// one user must never remove another user's recording, even if the IDs were
// somehow guessed or collided.
func TestDeleteDoesNotAffectOtherUsers(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	victim, err := st.Save(ctx, "victim-delete", "sess-victim", uuid.NewString(), []byte{1, 2, 3, 4}, 16000)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := st.Delete(ctx, "attacker-delete", victim.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, _, err := st.Open(ctx, "victim-delete", victim.ID); err != nil {
		t.Fatalf("victim's recording should survive attacker's delete, Open() error = %v", err)
	}
}

func TestDeleteBySessionRemovesOnlyThatSessionsRecordings(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "alex-cascade"
	inSession, err := st.Save(ctx, userID, "sess-a", uuid.NewString(), []byte{1, 2, 3, 4}, 16000)
	if err != nil {
		t.Fatalf("Save(sess-a) error = %v", err)
	}
	otherSession, err := st.Save(ctx, userID, "sess-b", uuid.NewString(), []byte{5, 6, 7, 8}, 16000)
	if err != nil {
		t.Fatalf("Save(sess-b) error = %v", err)
	}

	if err := st.DeleteBySession(ctx, userID, "sess-a"); err != nil {
		t.Fatalf("DeleteBySession() error = %v", err)
	}

	if _, _, err := st.Open(ctx, userID, inSession.ID); err == nil {
		t.Fatal("Open() for the deleted session's recording should error, got nil")
	}
	if _, _, err := st.Open(ctx, userID, otherSession.ID); err != nil {
		t.Fatalf("other session's recording should survive, Open() error = %v", err)
	}
}

func TestDeleteBySessionIsNoopWhenNoneMatch(t *testing.T) {
	st := requireStore(t)
	if err := st.DeleteBySession(context.Background(), "alex-cascade", "no-such-session"); err != nil {
		t.Fatalf("DeleteBySession() error = %v, want nil (no-op)", err)
	}
}
