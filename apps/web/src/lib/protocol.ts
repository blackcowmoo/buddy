// Mirror of apps/server/internal/protocol/protocol.go — keep in sync.

export type EventType =
  | "ready"
  | "partial_transcript"
  | "final_transcript"
  | "assistant_delta"
  | "assistant_done"
  | "refined_transcript"
  | "correction"
  | "error";

export interface Issue {
  type: string; // "grammar" | "vocabulary" | "phrasing"
  span: string;
  suggestion: string;
  explanation: string;
}

export interface Correction {
  original: string;
  corrected: string;
  issues: Issue[];
}

export interface ServerEvent {
  type: EventType;
  turn: number;
  text?: string;
  // Resolved session (chat room) ID. Only set on "ready".
  session?: string;
  correction?: Correction;
}

export type ClientMsg = { type: "text"; text: string };
