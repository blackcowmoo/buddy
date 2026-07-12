// Package recording archives raw mic audio to S3 so it can be replayed
// later. It is a pure storage side-effect of a WS utterance — decoupled from
// internal/pipeline's conversation orchestration, and from what the learner's
// words were transcribed/replied to. Nothing reads these recordings back into
// the conversation yet; this package only writes and lists/serves them for
// the "recordings" page.
//
// Audio is never stored raw: each utterance (mono 16-bit PCM, see
// internal/protocol) is wrapped in a WAV header (internal/recording/wav.go)
// and gzip-compressed before it ever reaches S3. That's a deliberately
// low-effort compression choice — stdlib only, no codec dependency or
// subprocess to manage (unlike internal/stt's whisper.cpp) — good enough for
// archival speech audio, and it keeps the upload/download path simple: the
// gzip bytes are stored as the S3 object body with Content-Encoding: gzip, so
// serving them back (see internal/httpserver/recordings.go) is a byte-for-byte
// proxy — the browser's own HTTP stack decompresses on the way in, no server
// CPU spent decoding for playback.
package recording

import (
	"context"
	"io"
	"time"
)

// Recording is one archived utterance's metadata. The audio bytes themselves
// live in S3; Open fetches them separately.
type Recording struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	DurationMS int
	SizeBytes  int64 // compressed (gzip) size, i.e. what's actually stored
}

// Store persists and serves back recorded utterance audio, scoped per user.
type Store interface {
	// Save compresses pcm (mono 16-bit PCM at sampleRate) and archives it
	// under userID.
	Save(ctx context.Context, userID string, pcm []byte, sampleRate int) (Recording, error)

	// List returns userID's recordings, most recent first.
	List(ctx context.Context, userID string) ([]Recording, error)

	// Open returns one recording's metadata and its stored bytes: a gzip
	// stream of a WAV file. The caller is responsible for closing the
	// returned reader. Returns an error if id doesn't exist or doesn't
	// belong to userID — callers must not leak one user's audio to another.
	Open(ctx context.Context, userID, id string) (Recording, io.ReadCloser, error)

	Close() error
}
