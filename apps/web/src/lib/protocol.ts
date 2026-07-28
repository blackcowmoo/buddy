// Mirror of apps/server/internal/protocol/protocol.go — keep in sync.

export type EventType =
  | "ready"
  | "pending_transcript"
  | "final_transcript"
  | "assistant_delta"
  | "assistant_done"
  | "refined_transcript"
  | "correction"
  | "user_translation"
  | "assistant_translation"
  | "error";

export interface Issue {
  type: string; // "grammar" | "vocabulary" | "phrasing" | "context"
  span: string;
  suggestion: string;
  explanation: string; // why it's wrong, in English
  explanationTranslation: string; // native-language translation of explanation
}

export interface Correction {
  original: string;
  corrected: string;
  issues: Issue[];
}

// One sentence of the end-of-conversation study wrap-up (see
// SessionSummary.studySummary in lib/sessions.ts), paired with its
// native-language translation — same English-first, translation-second
// shape as Issue.explanation/explanationTranslation.
export interface StudySummarySentence {
  english: string;
  translation: string;
}

// One fill-in-the-blank practice question, generated on demand from a
// session's flagged issues (see fetchSessionQuiz in lib/sessions.ts) —
// mirrors protocol.QuizQuestion server-side.
export interface QuizQuestion {
  prompt: string; // English sentence with exactly one "___" blank
  answer: string; // the word/phrase that correctly fills the blank
  translation: string; // native-language translation of the full, correct sentence
  explanation: string; // why this is the answer, in English
  explanationTranslation: string; // native-language translation of explanation
}

// How the learner produced a turn — spoken (transcribed by STT) or typed.
export type InputSource = "voice" | "text";

export interface ServerEvent {
  type: EventType;
  // 0 is reserved for the connection-opening greeting (see
  // pipeline.StartConversation server-side) — real turns start at 1.
  turn: number;
  text?: string;
  // Set on "final_transcript"/"refined_transcript" (a committed turn) and on
  // "pending_transcript" (a not-yet-committed voice draft, always "voice") —
  // there's no ambiguity to record on any other event type.
  source?: InputSource;
  // Resolved session (chat room) ID. Only set on "ready".
  session?: string;
  correction?: Correction;
  // Only on "correction": true when the analysis pass itself errored (LLM
  // call failed, or its output didn't parse) — as opposed to `correction`
  // present with an empty `issues` array, which means the pass ran fine and
  // found nothing to flag. `correction` is absent when this is true.
  failed?: boolean;
}

// source, when set to "voice", marks this "text" send as a learner-confirmed
// voice draft (see ServerEvent's "pending_transcript") rather than typed
// input — omitted/undefined means ordinary typed input.
export type ClientMsg = { type: "text"; text: string; source?: "voice" };
