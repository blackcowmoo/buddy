import { fetchJSON, postJSON } from "./fetchJSON";
import type { Correction } from "./protocol";

export type WritingStatus = "pending" | "done" | "failed";
export interface WritingPrompt { id: string; korean: string; status: WritingStatus; createdAt: number }

export function fetchWritingPrompts(): Promise<WritingPrompt[]> {
  return fetchJSON<WritingPrompt[]>("api/writing", []);
}

export function drawWritingPrompt(): Promise<WritingPrompt | null> {
  return postJSON<WritingPrompt | null>("api/writing/draw", {}, null);
}

export function fetchWritingPrompt(id: string): Promise<WritingPrompt | null> {
  return fetchJSON<WritingPrompt | null>(`api/writing/${encodeURIComponent(id)}`, null);
}

export function checkWriting(prompt: string, answer: string): Promise<Correction | null> {
  return postJSON<Correction | null>("api/writing/check", { prompt, answer }, null);
}
