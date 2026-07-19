// Package s3util holds the S3-compatible-client construction and
// delete-by-key-list logic shared by internal/recording (durable recordings
// archive) and internal/audiostore (temporary utterance backups) — both
// packages archive audio to the same kind of Ceph RGW/MinIO/S3 endpoint with
// the same static-credential shape.
package s3util

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// region is never surfaced to callers: Ceph/MinIO-style S3-compatible
// endpoints don't route on it, so it's a fixed, arbitrary value purely to
// satisfy the SDK's request signing, not a piece of deployment configuration.
const region = "us-east-1"

// NewClient builds an S3-compatible client from static credentials. endpoint
// empty means "use real AWS S3's default endpoint resolution"; pathStyle
// true means http://endpoint/bucket/key instead of http://bucket.endpoint/key
// (what Ceph RGW/MinIO require).
func NewClient(endpoint string, pathStyle bool, accessKey, secretKey string) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("s3util: aws config: %w", err)
	}

	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	}), nil
}

// DeleteAll removes every key from bucket, concurrently — cascading a chat
// room deletion to however many utterances it archived shouldn't cost one
// network round trip per utterance serially. Not the batch DeleteObjects API:
// several S3-compatible targets (e.g. this repo's own MinIO-backed tests)
// reject its XML body outright without a Content-MD5 the SDK doesn't always
// attach.
func DeleteAll(ctx context.Context, client *s3.Client, bucket string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	errs := make([]error, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			}); err != nil {
				errs[i] = fmt.Errorf("object %q: %w", key, err)
			}
		}(i, key)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
