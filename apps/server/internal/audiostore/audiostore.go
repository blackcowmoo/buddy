// Package audiostore streams raw audio bytes to an S3-compatible bucket for
// temporary storage (e.g. a Ceph RGW cluster). Ceph deployments commonly need
// path-style addressing (bucket in the URL path, not a virtual-host subdomain)
// and a specific storage class to steer the object onto the right backing
// disk pool — both are exposed as config here rather than assumed, since AWS
// S3 itself defaults to virtual-host style and a "STANDARD" class that don't
// apply to every Ceph install.
package audiostore

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// region is never surfaced to callers: Ceph (unlike AWS S3) doesn't route on
// it, so it's a fixed, arbitrary value purely to satisfy the SDK's request
// signing, not a piece of deployment configuration.
const region = "us-east-1"

type Config struct {
	Endpoint  string // Ceph RGW (or any S3-compatible) endpoint, e.g. https://ceph.example.com
	PathStyle bool   // true: http://endpoint/bucket/key instead of http://bucket.endpoint/key
	AccessKey string
	SecretKey string
	Bucket    string

	// StorageClass selects which backing disk pool/tier the object lands on
	// (Ceph maps storage classes to placement targets). Empty leaves it up to
	// the bucket's default.
	StorageClass string
}

// Store uploads audio streams to one bucket via the S3 API.
type Store struct {
	client       *s3.Client
	uploader     *manager.Uploader
	bucket       string
	storageClass types.StorageClass
}

func New(cfg Config) (*Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("audiostore: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = cfg.PathStyle
	})

	return &Store{
		client:       client,
		uploader:     manager.NewUploader(client),
		bucket:       cfg.Bucket,
		storageClass: types.StorageClass(cfg.StorageClass),
	}, nil
}

// SaveStream uploads r to key under the configured bucket. It streams the
// data through to S3 rather than buffering it whole in memory, so it's safe
// to feed with data still arriving over a live connection.
func (s *Store) SaveStream(ctx context.Context, key string, r io.Reader) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if s.storageClass != "" {
		input.StorageClass = s.storageClass
	}
	if _, err := s.uploader.Upload(ctx, input); err != nil {
		return fmt.Errorf("audiostore: upload %s: %w", key, err)
	}
	return nil
}

// Delete removes one object backed up under userID/sessionID/id.pcm (see
// transport.Handler.backupAudio) — used to cascade deleting a single
// recording (internal/recording.Store.Delete) to its matching temporary
// backup, since both are saved under the same caller-supplied id for exactly
// this purpose. Deleting a nonexistent key is not an error (S3's
// DeleteObject is idempotent), so this is naturally a no-op if id has no
// backup — e.g. audio backup was disabled when this utterance was recorded.
func (s *Store) Delete(ctx context.Context, userID, sessionID, id string) error {
	key := userID + "/" + sessionID + "/" + id + ".pcm"
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("audiostore: delete %s: %w", key, err)
	}
	return nil
}

// DeleteBySession removes every object backed up under userID/sessionID's
// key prefix (see transport.Handler.backupAudio, which writes keys as
// userID/sessionID/<uuid>.pcm) — used to cascade a chat room deletion to its
// temporary audio backups, the same way internal/recording.S3Store's
// DeleteBySession cascades to the durable recordings archive. A no-op if
// nothing matches the prefix.
//
// Objects are deleted one at a time (DeleteObject, not the batch
// DeleteObjects API) for the same reason internal/recording.S3Store does:
// several S3-compatible targets reject DeleteObjects' XML body outright
// without a Content-MD5 the SDK doesn't always attach.
func (s *Store) DeleteBySession(ctx context.Context, userID, sessionID string) error {
	prefix := userID + "/" + sessionID + "/"

	var keys []string
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("audiostore: delete by session: list: %w", err)
		}
		for _, obj := range out.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}

	for _, key := range keys {
		if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		}); err != nil {
			return fmt.Errorf("audiostore: delete by session: object %q: %w", key, err)
		}
	}
	return nil
}
