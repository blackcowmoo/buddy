// Package ttsstore caches generated TTS audio (see internal/tts's Kokoro
// client) in S3, keyed by an opaque string the caller controls — one shared
// cache for every server-side TTS call site in this app ("오늘의 아티클"
// read-aloud and, on demand, chat message read-aloud — see
// internal/transport), rather than a bespoke S3+MySQL pairing per feature.
// Regenerating the same text's audio on every playback is pure waste: the
// whole point of moving generation server-side (see internal/tts's doc
// comment) is that one generation serves every future request for that
// exact text, not just the learner who happened to trigger it first.
//
// A row's TTL is enforced by SweepExpired/RunSweepLoop, not by Open/Put
// themselves — a swept row's S3 object and index row are simply gone, so
// Open behaves exactly like "never generated" and the caller regenerates.
// Deleting after a few months rather than keeping every generated clip
// forever trades a rare regeneration (a few seconds, see internal/tts) for
// not paying S3 storage/bandwidth indefinitely for audio nobody's replayed
// in months.
package ttsstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"buddy/server/internal/mysqlerr"
	"buddy/server/internal/s3util"
)

// table carries a buddy_ prefix for the same reason as internal/recording's
// buddy_recordings: the database is shared with other services.
const table = "buddy_tts_cache"

// ErrNotFound is returned by Open when key has no cached audio — either it
// was never generated, or SweepExpired has since removed it. Callers treat
// this identically to "never generated": regenerate and Put.
var ErrNotFound = errors.New("ttsstore: not found")

// Cache is the minimal surface callers like internal/transport.ArticleAudio
// need — Put/Open alone — so they (and their own tests) can depend on this
// interface instead of the concrete *Store, the same "swap for a fake in
// tests, real store in production" shape as internal/tts.Speaker. version
// is a fingerprint of whatever generation settings affect the audio itself
// (see internal/tts.Kokoro.Version — voice, volume, ...): Open treats an
// entry generated under a different version as absent, so a settings
// change (e.g. a volume boost) invalidates every previously cached clip
// instead of quieter old copies being served indefinitely.
type Cache interface {
	Put(ctx context.Context, key, version string, audio []byte) error
	Open(ctx context.Context, key, version string) (io.ReadCloser, error)
}

// Config mirrors internal/recording.S3Config deliberately — see that type's
// doc comment for why this app uses one static-credential shape (the S3_*
// env vars) rather than the AWS default credential chain.
type Config struct {
	Endpoint     string
	PathStyle    bool
	AccessKey    string
	SecretKey    string
	Bucket       string
	StorageClass string
}

// Store is S3 object bytes + a MySQL index (cache_key -> s3_key, so
// SweepExpired can find and delete both the row and the object it points
// at without listing the bucket).
type Store struct {
	s3           *s3.Client
	bucket       string
	storageClass types.StorageClass
	rw, ro       *sql.DB
}

// New builds a Store and ensures buddy_tts_cache exists.
func New(ctx context.Context, cfg Config, rw, ro *sql.DB) (*Store, error) {
	client, err := s3util.NewClient(cfg.Endpoint, cfg.PathStyle, cfg.AccessKey, cfg.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("ttsstore: %w", err)
	}

	const schema = `CREATE TABLE IF NOT EXISTS ` + table + ` (
		cache_key  VARCHAR(255) NOT NULL,
		s3_key     VARCHAR(512) NOT NULL,
		version    VARCHAR(64)  NOT NULL DEFAULT '',
		size_bytes BIGINT       NOT NULL,
		created_at BIGINT       NOT NULL,
		PRIMARY KEY (cache_key),
		KEY idx_created (created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := rw.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("ttsstore: schema: %w", err)
	}
	// version was added after buddy_tts_cache first shipped — see
	// internal/newsarticle/mysql.go's published_at column for why an
	// idempotent ALTER (swallowing "already there") is needed alongside
	// CREATE TABLE IF NOT EXISTS rather than instead of it.
	if err := mysqlerr.ApplyAdditive(func() error {
		_, err := rw.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN version VARCHAR(64) NOT NULL DEFAULT '' AFTER s3_key`)
		return err
	}, mysqlerr.DupFieldName); err != nil {
		return nil, fmt.Errorf("ttsstore: migrate version: %w", err)
	}

	return &Store{
		s3:           client,
		bucket:       cfg.Bucket,
		storageClass: types.StorageClass(cfg.StorageClass),
		rw:           rw,
		ro:           ro,
	}, nil
}

// s3Key derives an S3 object key from a cache key. Every caller in this app
// controls both the cache key format ("article:"+id, "message:"+id — see
// internal/transport) and the fact that internal/tts always generates mp3,
// so a fixed "tts/"+key+".mp3" shape is enough; there's no need for a
// caller-supplied extension/content-type.
func s3Key(key string) string {
	return "tts/" + key + ".mp3"
}

// Put uploads audio under key, tagged with version, replacing any existing
// entry for it (the normal case right after a TTL-expired or stale-version
// key was regenerated). Content-Type is always audio/mpeg — see s3Key's doc
// comment.
func (s *Store) Put(ctx context.Context, key, version string, audio []byte) error {
	sk := s3Key(key)
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(sk),
		Body:        bytes.NewReader(audio),
		ContentType: aws.String("audio/mpeg"),
	}
	if s.storageClass != "" {
		input.StorageClass = s.storageClass
	}
	if _, err := s.s3.PutObject(ctx, input); err != nil {
		return fmt.Errorf("ttsstore: put object: %w", err)
	}

	_, err := s.rw.ExecContext(ctx, `
		INSERT INTO `+table+` (cache_key, s3_key, version, size_bytes, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE s3_key = VALUES(s3_key), version = VALUES(version), size_bytes = VALUES(size_bytes), created_at = VALUES(created_at)
	`, key, sk, version, len(audio), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("ttsstore: index: %w", err)
	}
	return nil
}

// Open returns the cached audio for key, or ErrNotFound if there isn't any
// (never generated, since swept — see the package doc — or generated under
// a different version, e.g. an old, quieter TTSVolumeMultiplier). A
// version mismatch actively deletes the stale entry (S3 object + row)
// rather than leaving it to be found again — Put will re-index the same
// cache_key under the new version once the caller regenerates, so nothing
// would reclaim the old object otherwise. Reads via the ro handle: a cache
// miss just means the caller regenerates, so replica lag here costs an
// occasional avoidable regeneration, not correctness.
func (s *Store) Open(ctx context.Context, key, version string) (io.ReadCloser, error) {
	var sk, gotVersion string
	err := s.ro.QueryRowContext(ctx, `SELECT s3_key, version FROM `+table+` WHERE cache_key = ?`, key).Scan(&sk, &gotVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ttsstore: lookup: %w", err)
	}
	if gotVersion != version {
		if err := s.deleteEntry(ctx, key, sk); err != nil {
			log.Printf("ttsstore: delete stale version %q (had %q, want %q): %v", key, gotVersion, version, err)
		}
		return nil, ErrNotFound
	}

	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(sk),
	})
	if err != nil {
		return nil, fmt.Errorf("ttsstore: get object: %w", err)
	}
	return out.Body, nil
}

// deleteEntry removes one cache_key's S3 object and row — the version-miss
// path in Open, and (mirroring the same shape) SweepExpired's bulk sweep.
func (s *Store) deleteEntry(ctx context.Context, key, s3Key string) error {
	if err := s3util.DeleteAll(ctx, s.s3, s.bucket, []string{s3Key}); err != nil {
		return fmt.Errorf("ttsstore: delete object: %w", err)
	}
	if _, err := s.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE cache_key = ?`, key); err != nil {
		return fmt.Errorf("ttsstore: delete row: %w", err)
	}
	return nil
}

// SweepExpired deletes every cached entry Put more than ttl ago: its S3
// object and its buddy_tts_cache row. Returns how many were removed, for
// RunSweepLoop's logging. Reads via rw (not ro) so a Put from moments ago —
// still possibly ahead of a lagging replica — is never mistaken for one
// that's actually expired.
func (s *Store) SweepExpired(ctx context.Context, ttl time.Duration) (int, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	rows, err := s.rw.QueryContext(ctx, `SELECT cache_key, s3_key FROM `+table+` WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("ttsstore: sweep: lookup: %w", err)
	}
	var keys, s3Keys []string
	for rows.Next() {
		var k, sk string
		if err := rows.Scan(&k, &sk); err != nil {
			rows.Close()
			return 0, fmt.Errorf("ttsstore: sweep: scan: %w", err)
		}
		keys = append(keys, k)
		s3Keys = append(s3Keys, sk)
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		return 0, fmt.Errorf("ttsstore: sweep: rows: %w", closeErr)
	}
	if len(keys) == 0 {
		return 0, nil
	}

	if err := s3util.DeleteAll(ctx, s.s3, s.bucket, s3Keys); err != nil {
		return 0, fmt.Errorf("ttsstore: sweep: %w", err)
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	if _, err := s.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE cache_key IN (`+placeholders+`)`, args...); err != nil {
		return 0, fmt.Errorf("ttsstore: sweep: delete rows: %w", err)
	}
	return len(keys), nil
}

// TTL/SweepInterval bound RunSweepLoop. TTL matches this feature's explicit
// spec (regenerate audio not replayed in three months rather than storing
// it forever); the interval is coarse — unlike article-study's orphan
// sweep, a few extra hours of storage before a stale entry gets swept isn't
// a correctness concern, so there's no reason to poll aggressively.
const (
	TTL           = 90 * 24 * time.Hour
	SweepInterval = 6 * time.Hour
)

// RunSweepLoop runs SweepExpired every SweepInterval until ctx is canceled
// — started unconditionally at server startup alongside the other sweep
// loops (see cmd/server/main.go), a no-op (0 deleted) whenever nothing has
// aged past TTL yet.
func RunSweepLoop(ctx context.Context, store *Store) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := store.SweepExpired(context.Background(), TTL)
			if err != nil {
				log.Printf("ttsstore: sweep: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("ttsstore: sweep: removed %d expired entries", n)
			}
		}
	}
}
