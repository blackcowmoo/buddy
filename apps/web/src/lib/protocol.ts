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
  explanation: string;
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
}

export type ClientMsg = { type: "text"; text: string };
