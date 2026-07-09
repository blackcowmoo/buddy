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
	EvReady          EventType = "ready"              // handshake
	EvPartial        EventType = "partial_transcript" // fast STT, may change
	EvFinal          EventType = "final_transcript"   // fast STT, locked
	EvAssistantDelta EventType = "assistant_delta"    // streamed reply token
	EvAssistantDone  EventType = "assistant_done"     // full reply (browser speaks this)
	EvRefined        EventType = "refined_transcript" // slow STT re-transcription
	EvCorrection     EventType = "correction"         // grammar/vocab feedback
	EvError          EventType = "error"
)

type ServerEvent struct {
	Type EventType `json:"type"`
	// Turn correlates fast-track and refine-track messages for one utterance.
	Turn int    `json:"turn"`
	Text string `json:"text,omitempty"`

	// Only on EvCorrection.
	Correction *Correction `json:"correction,omitempty"`
}

type Correction struct {
	Original  string  `json:"original"`
	Corrected string  `json:"corrected"`
	Issues    []Issue `json:"issues"`
}

type Issue struct {
	Type        string `json:"type"`        // "grammar" | "vocabulary" | "phrasing"
	Span        string `json:"span"`        // the problematic text
	Suggestion  string `json:"suggestion"`  // the fix
	Explanation string `json:"explanation"` // short, learner-friendly
}

// ---- client -> server (TEXT control frames) ---------------------------------

type ClientMsg struct {
	Type string `json:"type"` // "text" (typed input) | "reset"
	Text string `json:"text,omitempty"`
}
