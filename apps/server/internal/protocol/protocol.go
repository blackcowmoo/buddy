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
	EvReady EventType = "ready" // handshake
	// EvPendingTranscript carries a still-editable STT guess for one spoken
	// utterance, before the learner has committed to sending it — see
	// pipeline.HandleUtterance's doc comment for why voice input stops here
	// instead of committing straight to the session. May fire more than once
	// per utterance (an initial FAST guess, then a slower Judge-reconciled
	// upgrade if it differs) — the frontend just keeps the latest. Never
	// persisted: nothing here is part of the conversation until the learner
	// actually sends it, which arrives back as an ordinary "text" ClientMsg
	// (Source set to SourceVoice) and is handled exactly like typed input.
	EvPendingTranscript    EventType = "pending_transcript"
	EvFinal                EventType = "final_transcript"      // committed turn (typed, or a learner-confirmed voice draft)
	EvAssistantDelta       EventType = "assistant_delta"       // streamed reply token
	EvAssistantDone        EventType = "assistant_done"        // full reply (browser speaks this)
	EvRefined              EventType = "refined_transcript"    // legacy: upgraded an already-committed turn's text; no longer emitted live, kept for hydrating turns saved before EvPendingTranscript existed
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
	// transcribed by STT) or "text" (typed). Set on EvFinal/EvRefined (a
	// committed turn) and on EvPendingTranscript (a not-yet-committed voice
	// draft, always SourceVoice) — there's no ambiguity to record on any
	// other event type.
	Source string `json:"source,omitempty"`

	// Session carries the resolved session (chat room) ID. Only set on
	// EvReady — the client uses it to fetch/replay this room later via
	// GET /api/sessions/{id} and to resume it with ?session=<id> on a
	// future connection.
	Session string `json:"session,omitempty"`

	// Only on EvCorrection.
	Correction *Correction `json:"correction,omitempty"`
	// Failed is true on an EvCorrection whose analysis pass itself errored
	// (LLM call failed, or its output didn't parse) — as opposed to
	// Correction present with an empty Issues slice, which means the pass
	// ran fine and simply found nothing to flag. Correction is nil when
	// Failed is true. Without this, both a failed check and a clean
	// sentence looked identical to the frontend once the analysis "just
	// stopped" — see GrammarControl in apps/web/src/App.tsx.
	Failed bool `json:"failed,omitempty"`
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

// StudySummarySentence pairs one English sentence of the end-of-conversation
// study wrap-up (see pipeline.GenerateStudySummary) with its native-language
// translation — the same English-first, translation-second shape as
// Issue.Explanation/ExplanationTranslation, applied to a whole sentence
// instead of one issue's explanation.
type StudySummarySentence struct {
	English     string `json:"english"`
	Translation string `json:"translation"`
}

// QuizQuestion is one fill-in-the-blank practice item synthesized from a
// session's flagged issues (see pipeline.GenerateStudyQuiz) — generated only
// when a learner explicitly asks to practice, as a way to test whether
// GenerateStudySummary's prose wrap-up actually landed, rather than just
// re-reading it.
type QuizQuestion struct {
	Prompt                 string `json:"prompt"`                 // English sentence with exactly one blank ("___") to fill in
	Answer                 string `json:"answer"`                 // the word/phrase that correctly fills the blank, in English
	Translation            string `json:"translation"`            // native-language translation of the full, correctly-filled sentence
	Explanation            string `json:"explanation"`            // why this is the answer, in English, short and kind
	ExplanationTranslation string `json:"explanationTranslation"` // native-language translation of Explanation
}

// WordSuggestion is one candidate English word/phrase returned by
// pipeline.SuggestWords for a learner's native-language description of a
// word they can't recall mid-conversation — see httpserver's
// wordSuggestHandler.
type WordSuggestion struct {
	Word    string `json:"word"`    // the candidate English word or phrase
	Meaning string `json:"meaning"` // brief native-language gloss
	Example string `json:"example"` // one example English sentence using Word
}

// ---- client -> server (TEXT control frames) ---------------------------------

type ClientMsg struct {
	Type string `json:"type"` // "text" (typed input, or a learner-confirmed voice draft)
	Text string `json:"text,omitempty"`
	// Source distinguishes a learner-confirmed EvPendingTranscript draft
	// (SourceVoice) from ordinary typed input; empty (or any other value)
	// means SourceText. The frontend sets it when the composer's text still
	// came from a pending voice draft the learner reviewed and sent — see
	// EvPendingTranscript's doc comment.
	Source string `json:"source,omitempty"`
}
