package transport

import "strconv"

// MessageAudioKey is the ttsstore.Store cache key for one chat turn's
// read-aloud audio — either the learner's own line (pronunciation practice)
// or the assistant's reply, both playable from the same 🔊 control (see
// StudyControl.tsx). Unlike ArticleAudioKey, every message is unique to its
// own conversation (no cross-learner sharing), and — unlike an article's
// summary — is never known ahead of generation time, so there's no
// equivalent pre-generation hook: httpserver's message audio handler
// generates (and caches) it the first time a learner actually asks to hear
// it. role disambiguates the user's and assistant's lines at the same turn
// number (see store.Turn) — they're two different pieces of text.
func MessageAudioKey(sessionID string, turn int, role string) string {
	return "message:" + sessionID + ":" + strconv.Itoa(turn) + ":" + role
}
