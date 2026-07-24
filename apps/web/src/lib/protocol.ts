// Mirror of apps/server/internal/protocol/protocol.go — keep in sync.

export type EventType =
  | "ready"
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

// How the learner produced a turn — spoken (transcribed by STT) or typed.
export type InputSource = "voice" | "text";

export interface ServerEvent {
  type: EventType;
  // 0 is reserved for the connection-opening greeting (see
  // pipeline.StartConversation server-side) — real turns start at 1.
  turn: number;
  text?: string;
  // Only set on "final_transcript"/"refined_transcript" — there's no
  // ambiguity to record on any other event type.
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

export type ClientMsg = { type: "text"; text: string };
