package transport

import (
	"bytes"
	"context"
	"log"
)

// backupAudio streams one utterance's raw PCM to the configured temporary
// store (see internal/audiostore.Store) as a best-effort disposable backup.
// It runs on its own context.Background() timeout rather than the turn's
// context, so a barge-in that cancels the turn doesn't truncate the upload —
// and it only logs on failure, since losing this backup must never affect
// the live conversation.
//
// The key is prefixed with sessionID (not just userID) so
// AudioSaver.DeleteBySession can find and remove every backup belonging to a
// room once its chat session is deleted — otherwise these temporary backups
// would outlive the conversation they belong to indefinitely. id is the same
// one passed to internal/recording.Store.Save for this utterance (see the
// ServeHTTP call site), so AudioSaver.Delete can also remove this one backup
// when just its matching recording — not the whole session — is deleted.
func (h *Handler) backupAudio(userID, sessionID, id string, pcm []byte) {
	key := userID + "/" + sessionID + "/" + id + ".pcm"
	ctx, cancel := context.WithTimeout(context.Background(), audioSaveTimeout)
	defer cancel()
	if err := h.audio.SaveStream(ctx, key, bytes.NewReader(pcm)); err != nil {
		log.Printf("audiostore: backup %s: %v", key, err)
	}
}
