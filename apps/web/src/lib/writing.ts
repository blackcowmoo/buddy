import { fetchJSON, postJSON, requestOK } from "./fetchJSON";
import type { Issue } from "./protocol";

type WritingStatus = "pending" | "done" | "failed";
// "" is the server's "no check bound to the stored answer" state; "pending"
// means a detached server-side check is running (or waiting to be re-run).
export type WritingCheckStatus = "" | "pending" | "done" | "failed";

export interface WritingPrompt {
  id: string;
  korean: string;
  status: WritingStatus;
  createdAt: number;
  // The persisted answer and its asynchronously graded result: reopening
  // the page restores both instead of starting from an empty editor.
  answer?: string;
  checkStatus?: WritingCheckStatus;
  corrected?: string;
  issues?: Issue[];
}

export function fetchWritingPrompts(): Promise<WritingPrompt[]> {
  return fetchJSON<WritingPrompt[]>("api/writing", []);
}

export function drawWritingPrompt(): Promise<WritingPrompt | null> {
  return postJSON<WritingPrompt | null>("api/writing/draw", {}, null);
}

export function fetchWritingPrompt(id: string): Promise<WritingPrompt | null> {
  return fetchJSON<WritingPrompt | null>(`api/writing/${encodeURIComponent(id)}`, null);
}

export function deleteWritingPrompt(id: string): Promise<boolean> {
	return requestOK(`api/writing/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// Submitting only persists the answer and starts the server-side check; the
// grade arrives via polling the prompt, so navigating away never cancels it.
export function checkWriting(promptId: string, answer: string): Promise<WritingPrompt | null> {
  return postJSON<WritingPrompt | null>("api/writing/check", { promptId, answer }, null);
}

export function recheckWriting(promptId: string): Promise<WritingPrompt | null> {
  return postJSON<WritingPrompt | null>(`api/writing/${encodeURIComponent(promptId)}/recheck`, {}, null);
}
