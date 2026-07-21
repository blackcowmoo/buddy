// Package protocol defines the WebSocket wire types shared between the
// pipeline (producer) and the transport layer (writer). It is a leaf package
// with no internal imports, which keeps pipeline and transport decoupled.
//
// The frontend mirror lives in apps/web/src/lib/protocol.ts — keep them in sync.
//
// Framing:
//   - client -> server: a BINARY frame is one complete utterance
//     (mono 16 kHz s16le PCM), segmented by the browser's VAD. TEXT frames are
//     JSON control messages (ClientMsg).
//   - server -> client: TEXT frames only, JSON ServerEvent.
package protocol

// ---- server -> client -------------------------------------------------------

type EventType string

const (
	EvReady                EventType = "ready"                 // handshake
	EvFinal                EventType = "final_transcript"      // fast STT, locked
	EvAssistantDelta       EventType = "assistant_delta"       // streamed reply token
	EvAssistantDone        EventType = "assistant_done"        // full reply (browser speaks this)
	EvRefined              EventType = "refined_transcript"    // slow STT re-transcription
	EvCorrection           EventType = "correction"            // grammar/vocab feedback
	EvUserTranslation      EventType = "user_translation"      // native-language translation of the user's turn
	EvAssistantTranslation EventType = "assistant_translation" // native-language translation of the assistant's reply
	EvError                EventType = "error"
)

// Input source values for ServerEvent.Source / store.Turn.Source — how the
// learner produced a turn, spoken (transcribed by STT) or typed.
const (
	SourceVoice = "voice"
	SourceText  = "text"
)

type ServerEvent struct {
	Type EventType `json:"type"`
	// Turn correlates fast-track and refine-track messages for one utterance.
	// Turn 0 is a reserved sentinel for the connection-opening greeting (see
	// pipeline.StartConversation) — it never comes from session.NextTurn, so
	// real turns always start at 1.
	Turn int `json:"turn"`
	// Text carries the event's payload text — token/full text for the
	// transcript/assistant events, or the translated sentence for
	// EvUserTranslation/EvAssistantTranslation.
	Text string `json:"text,omitempty"`

	// Source says how the learner produced this turn: "voice" (spoken,
	// transcribed by STT) or "text" (typed). Only set on EvFinal/EvRefined —
	// there's no ambiguity to record on any other event type.
	Source string `json:"source,omitempty"`

	// Session carries the resolved session (chat room) ID. Only set on
	// EvReady — the client uses it to fetch/replay this room later via
	// GET /api/sessions/{id} and to resume it with ?session=<id> on a
	// future connection.
	Session string `json:"session,omitempty"`

	// Only on EvCorrection.
	Correction *Correction `json:"correction,omitempty"`
}

type Correction struct {
	Original  string  `json:"original"`
	Corrected string  `json:"corrected"`
	Issues    []Issue `json:"issues"`
}

type Issue struct {
	Type                   string `json:"type"`                   // "grammar" | "vocabulary" | "phrasing" | "context"
	Span                   string `json:"span"`                   // the problematic text
	Suggestion             string `json:"suggestion"`             // the fix
	Explanation            string `json:"explanation"`            // why it's wrong, in English, short and kind
	ExplanationTranslation string `json:"explanationTranslation"` // native-language translation of Explanation
}

// ---- client -> server (TEXT control frames) ---------------------------------

type ClientMsg struct {
	Type string `json:"type"` // "text" (typed input)
	Text string `json:"text,omitempty"`
}
