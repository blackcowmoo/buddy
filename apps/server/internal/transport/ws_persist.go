package transport

import (
	"context"
	"log"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/wordreview"
)

// persistEvent writes a copy of ev's payload to durable per-session
// transcript storage. It's deliberately narrow and off the hot path:
// EvAssistantDelta fires many times per turn as tokens stream, so only
// events that represent a finished piece of state trigger a write, and each
// write runs in its own goroutine so a slow database never adds latency to
// the live conversation the learner is watching. Writes use
// context.Background(), not the connection's context, so a turn's result
// still lands even if the client disconnects or barges in right as it
// completes (same reasoning as pipeline.compact's use of Background).
//
// Turn 0 (the opening greeting, see pipeline.StartConversation) is written
// here like any other turn: store.MySQLStore.SaveTurn inserts into
// buddy_turns unconditionally and also creates the *session row* right away
// (title placeholder'd from the greeting text), so a greeting-only room is
// already visible to ListSessions/SessionDetail — the learner can find it
// and delete it even if they never reply. If the learner's turn 1 does land,
// SaveTurn replaces that placeholder title with their own first message.
func persistEvent(pipe *pipeline.Pipeline, st store.Store, words wordreview.Store, wordVerifyQueue *asyncjob.Queue, userID, sessionID string, ev protocol.ServerEvent) {
	switch ev.Type {
	case protocol.EvFinal:
		go saveTurn(st, userID, sessionID, ev.Turn, "user", ev.Text, false, ev.Source)
	case protocol.EvRefined:
		go saveTurn(st, userID, sessionID, ev.Turn, "user", ev.Text, true, ev.Source)
	case protocol.EvAssistantDone:
		go saveTurn(st, userID, sessionID, ev.Turn, "assistant", ev.Text, false, "")
	case protocol.EvCorrection:
		if ev.Failed {
			// Only durable when a job was actually reserved for this turn
			// (see store.ReserveCorrectionJob) — a plain UPDATE with no
			// matching row is a harmless no-op, same as saveCorrection below
			// when the queue-backed path isn't configured.
			go failCorrectionJob(st, userID, sessionID, ev.Turn)
			return
		}
		if ev.Correction == nil {
			return
		}
		// In a deployment with no durable correction queue configured,
		// this is the only place a correction (and the words captured from
		// it — see captureCorrectionWords) ever gets persisted; when a
		// queue is configured this runs alongside CorrectionJobHandler's own
		// save, which is fine — both are idempotent (see word_capture.go).
		go func(c protocol.Correction) {
			saveCorrection(st, userID, sessionID, ev.Turn, c)
			captureCorrectionWords(context.Background(), pipe, words, wordVerifyQueue, userID, c)
		}(*ev.Correction)
	case protocol.EvUserTranslation:
		go saveTranslation(st, userID, sessionID, ev.Turn, "user", ev.Text)
	case protocol.EvAssistantTranslation:
		go saveTranslation(st, userID, sessionID, ev.Turn, "assistant", ev.Text)
	}
}

func saveTurn(st store.Store, userID, sessionID string, turn int, role, text string, refined bool, source string) {
	if err := st.SaveTurn(context.Background(), userID, sessionID, turn, role, text, refined, source); err != nil {
		log.Printf("store: save turn %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

func saveCorrection(st store.Store, userID, sessionID string, turn int, c protocol.Correction) {
	if err := st.SaveCorrection(context.Background(), userID, sessionID, turn, c); err != nil {
		log.Printf("store: save correction %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

// failCorrectionJob mirrors saveCorrection for the failure path — a second,
// less-detailed FailJob write on top of CorrectionJobHandler's own (see
// analysis_jobs.go) when the queue-backed path is configured, and the only
// one at all when it isn't (a harmless no-op there, same as saveCorrection
// above with no reservation to match).
func failCorrectionJob(st store.Store, userID, sessionID string, turn int) {
	if err := st.FailJob(context.Background(), userID, sessionID, turn, "correction", "analysis failed"); err != nil {
		log.Printf("store: fail correction job %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}

func saveTranslation(st store.Store, userID, sessionID string, turn int, role, translation string) {
	if err := st.SaveTranslation(context.Background(), userID, sessionID, turn, role, translation); err != nil {
		log.Printf("store: save translation %s/%s#%d: %v", userID, sessionID, turn, err)
	}
}
