package recording

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

// table carries a buddy_ prefix for the same reason as internal/store's
// buddy_profiles: the database is shared with other services.
const table = "buddy_recordings"

// region is never surfaced to callers: like internal/audiostore's identical
// constant, Ceph/MinIO-style S3-compatible endpoints don't route on it, so
// it's a fixed, arbitrary value purely to satisfy the SDK's request signing,
// not a piece of deployment configuration.
const region = "us-east-1"

// S3Config mirrors internal/audiostore.Config deliberately: both packages
// archive the same WS utterance audio to the same kind of S3-compatible
// endpoint, so they share one static-credential shape (and, in practice, one
// set of S3_* environment variables — see config.Config) rather than one
// using the AWS default credential chain and the other static keys. A
// bespoke chain (env vars/shared config file/IRSA) sounds nice for real AWS
// S3, but this app's actual deployments hand out static keys via a secret
// store, the same as MYSQL_*/REDIS_* — that mismatch previously left this
// feature silently disabled (RecordingS3Bucket/BUDDY_S3_BUCKET was never the
// name anything actually injected).
type S3Config struct {
	Endpoint     string // optional S3-compatible endpoint (e.g. MinIO, Ceph RGW); empty for real AWS S3
	PathStyle    bool   // true: http://endpoint/bucket/key instead of http://bucket.endpoint/key
	AccessKey    string
	SecretKey    string
	Bucket       string
	StorageClass string // empty leaves it up to the bucket's default
}

// S3Store is the default Store: audio bytes in S3, metadata in MySQL. rw/ro
// are shared with internal/store's MySQLStore (see its DB() accessor) rather
// than a second connection pool to the same instance.
type S3Store struct {
	s3           *s3.Client
	bucket       string
	storageClass types.StorageClass
	rw, ro       *sql.DB
}

// newS3Client builds the AWS SDK client from cfg's static credentials —
// split out from NewS3 so the endpoint/path-style/credential wiring can be
// tested (internal/recording/s3_config_test.go) without a real database.
func newS3Client(cfg S3Config) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("recording: aws config: %w", err)
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	}), nil
}

// NewS3 builds an S3-backed Store and ensures the buddy_recordings table
// exists.
func NewS3(ctx context.Context, cfg S3Config, rw, ro *sql.DB) (*S3Store, error) {
	client, err := newS3Client(cfg)
	if err != nil {
		return nil, err
	}

	const schema = `CREATE TABLE IF NOT EXISTS ` + table + ` (
		id          VARCHAR(64)  NOT NULL,
		user_id     VARCHAR(255) NOT NULL,
		s3_key      VARCHAR(512) NOT NULL,
		duration_ms INT          NOT NULL,
		size_bytes  BIGINT       NOT NULL,
		created_at  BIGINT       NOT NULL,
		PRIMARY KEY (id),
		KEY idx_user_created (user_id, created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("recording: schema: %w", err)
	}

	return &S3Store{
		s3:           client,
		bucket:       cfg.Bucket,
		storageClass: types.StorageClass(cfg.StorageClass),
		rw:           rw,
		ro:           ro,
	}, nil
}

func (s *S3Store) Save(ctx context.Context, userID string, pcm []byte, sampleRate int) (Recording, error) {
	id := uuid.NewString()
	key := userID + "/" + id + ".wav.gz"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(encodeWAV(pcm, sampleRate)); err != nil {
		return Recording{}, fmt.Errorf("recording: gzip: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Recording{}, fmt.Errorf("recording: gzip: %w", err)
	}

	rec := Recording{
		ID:         id,
		UserID:     userID,
		CreatedAt:  time.Now(),
		DurationMS: durationMS(pcm, sampleRate),
		SizeBytes:  int64(buf.Len()),
	}

	input := &s3.PutObjectInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(buf.Bytes()),
		ContentType:     aws.String("audio/wav"),
		ContentEncoding: aws.String("gzip"), // lets a browser fetch decompress transparently
	}
	if s.storageClass != "" {
		input.StorageClass = s.storageClass
	}
	if _, err := s.s3.PutObject(ctx, input); err != nil {
		return Recording{}, fmt.Errorf("recording: put object: %w", err)
	}

	_, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+table+` (id, user_id, s3_key, duration_ms, size_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, rec.ID, rec.UserID, key, rec.DurationMS, rec.SizeBytes, rec.CreatedAt.Unix())
	if err != nil {
		return Recording{}, fmt.Errorf("recording: insert: %w", err)
	}
	return rec, nil
}

func (s *S3Store) List(ctx context.Context, userID string) ([]Recording, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, duration_ms, size_bytes, created_at FROM `+table+`
		WHERE user_id = ? ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("recording: list: %w", err)
	}
	defer rows.Close()

	var out []Recording
	for rows.Next() {
		var rec Recording
		var createdAt int64
		if err := rows.Scan(&rec.ID, &rec.DurationMS, &rec.SizeBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("recording: scan: %w", err)
		}
		rec.UserID = userID
		rec.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *S3Store) Open(ctx context.Context, userID, id string) (Recording, io.ReadCloser, error) {
	var rec Recording
	var s3Key string
	var createdAt int64
	err := s.ro.QueryRowContext(ctx, `
		SELECT s3_key, duration_ms, size_bytes, created_at FROM `+table+`
		WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&s3Key, &rec.DurationMS, &rec.SizeBytes, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, nil, fmt.Errorf("recording: %q not found", id)
	}
	if err != nil {
		return Recording{}, nil, fmt.Errorf("recording: lookup: %w", err)
	}
	rec.ID = id
	rec.UserID = userID
	rec.CreatedAt = time.Unix(createdAt, 0)

	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		return Recording{}, nil, fmt.Errorf("recording: get object: %w", err)
	}
	return rec, out.Body, nil
}

// Close is a no-op: the rw/ro pools are owned by internal/store's
// MySQLStore, which closes them.
func (s *S3Store) Close() error { return nil }
