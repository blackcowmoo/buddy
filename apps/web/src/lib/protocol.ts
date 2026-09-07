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
  // Duplicates the turn-level display translation so a translation-only
  // Judge revision still participates in preview/final unread comparison.
  translation?: string;
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
  // Short native-language gloss of `answer` alone (its meaning in this
  // sentence, not a translation of the whole sentence) — shown next to
  // `prompt` before the learner answers, so a blanked English sentence
  // (which alone could fit many different words) becomes guessable.
  // Optional only because a quiz persisted before this field existed
  // decodes without it — see the "퀴즈 다시 만들기" reset button.
  answerMeaning?: string;
  // Close synonyms/alternate forms of `answer` that also correctly fill the
  // blank — QuizPanel accepts any of these as correct too. Absent/empty for
  // a question with no reasonable substitute, or one persisted before this
  // field existed.
  acceptableAnswers?: string[];
  translation: string; // native-language translation of the full, correct sentence
  explanation: string; // why `answer` specifically fits this sentence's meaning/nuance, in English
  explanationTranslation: string; // native-language translation of explanation
}

// One candidate English word/phrase suggested for a learner's
// native-language description of a word they can't recall (see
// lib/wordSearch.ts's suggestWords) — mirrors protocol.WordSuggestion
// server-side.
export interface WordSuggestion {
  word: string; // the candidate English word or phrase
  meaning: string; // brief native-language gloss
  example: string; // one example English sentence using word
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
  // On correction events: false/absent is the quick Chat preview; true is
  // the terminal Judge result after Analysis has refined that same draft.
  final?: boolean;
  // True when that terminal result differs from the Chat preview. The server
  // computes this before either asynchronous persistence write can race.
  changed?: boolean;
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
