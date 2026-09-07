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
	// Final distinguishes the quick Chat preview (false) from the terminal
	// Judge result (true). The preview is immediately useful but remains
	// visibly "refining" in the client; only a changed final result can become
	// unread learning feedback.
	Final bool `json:"final,omitempty"`
	// Changed is set on a terminal correction when Judge changed any visible
	// part of the Chat preview, including its translation. Computing this from
	// the in-memory stage outputs keeps unread state independent of async write
	// ordering.
	// Do not omit false: the browser needs an explicit unchanged decision when
	// a terminal event arrives without its preview (for example after a reconnect).
	Changed bool `json:"changed"`
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
	// Translation is also persisted separately on the turn for ordinary UI
	// rendering. Keeping it in the staged correction snapshot makes a
	// translation-only Judge revision count as unread refined feedback too.
	Translation string `json:"translation,omitempty"`
}

// WritingPrompt is a short Korean-to-English sentence exercise personalized
// from the learner profile.
type WritingPrompt struct {
	Korean string `json:"korean"`
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
	Prompt string `json:"prompt"` // English sentence with exactly one blank ("___") to fill in
	Answer string `json:"answer"` // the word/phrase that correctly fills the blank, in English
	// AnswerMeaning is a short native-language gloss of Answer alone (its
	// sense in this sentence, not a translation of the whole sentence) —
	// shown to the learner alongside Prompt before they answer. A blanked
	// English sentence alone can fit many different words, so without this
	// there's nothing to actually predict from; pairing the blank with what
	// it means narrows it down to one guessable word the same way a
	// dictionary-style gloss would.
	AnswerMeaning string `json:"answerMeaning"`
	// AcceptableAnswers lists close synonyms/alternate forms of Answer that
	// also correctly fill Prompt with the same meaning — QuizPanel accepts
	// any of these as correct too, since a learner shouldn't be marked wrong
	// for a word that fits just as naturally. Empty when Answer has no
	// reasonable substitute in this exact sentence. Never includes Answer
	// itself.
	AcceptableAnswers      []string `json:"acceptableAnswers,omitempty"`
	Translation            string   `json:"translation"`            // native-language translation of the full, correctly-filled sentence
	Explanation            string   `json:"explanation"`            // why Answer specifically (not just any AcceptableAnswers) fits this sentence's meaning/nuance, in English, short and kind
	ExplanationTranslation string   `json:"explanationTranslation"` // native-language translation of Explanation
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

// ArticleStudy is one LLM-synthesized "오늘의 아티클" study unit for a single
// news article: an English summary paragraph, plus a native-language
// reading-comprehension quiz testing whether the learner actually
// understood it, not just recognized a keyword — see
// pipeline.Pipeline.GenerateArticleStudy. Persisted as
// newsarticle.Article once generated (cached by article URL, so every later
// learner who draws the same story reads this exact result); this type is
// just the JSON shape the LLM call itself returns and parses into.
//
// The quiz is several independent 2-choice sub-questions (SubQuestions),
// not one 4-choice "which of these near-identical paragraphs is right"
// question: the latter let a learner spot the answer by diffing the
// choices against each other (whichever one reads slightly differently)
// without having read Summary at all. Each SubQuestion instead isolates
// ONE concrete, independently-checkable fact (an amount, a date, who did
// what), so there's nothing to diff — every sub-question has to be
// answered from actually having read Summary.
type ArticleStudy struct {
	Summary      string               `json:"summary"`     // English, self-contained
	Translation  string               `json:"translation"` // native-language translation of Summary
	SubQuestions []ArticleSubQuestion `json:"subQuestions"`
}

// ArticleSubQuestion tests ONE specific fact from ArticleStudy.Summary as a
// plain binary choice: exactly 2 candidate answers, one of them (at
// CorrectOptionIndex) matching Summary, the other a plausible-looking
// alteration of that same specific detail (a swapped number/currency/date,
// the opposite of what actually happened) — not an unrelated or
// nonsensical alternative.
type ArticleSubQuestion struct {
	// Prompt is a short native-language question stem naming which fact this
	// sub-question is about (e.g. "인수 금액은 얼마인가요?") — options alone
	// (e.g. just "파운드" / "달러") aren't self-explanatory without it.
	Prompt string `json:"prompt"`
	// Options has exactly 2 entries — short native-language candidate
	// answers to Prompt, not full-sentence paraphrases of Summary.
	Options            []string `json:"options"`
	CorrectOptionIndex int      `json:"correctOptionIndex"`
	// Explanation is a native-language note on why Options[CorrectOptionIndex]
	// is the accurate one — shown to the learner only after they answer.
	Explanation string `json:"explanation"`
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
