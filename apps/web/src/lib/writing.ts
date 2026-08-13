import { postJSON } from "./fetchJSON";
import type { Correction } from "./protocol";

export interface WritingPrompt { korean: string }

export function fetchWritingPrompt(): Promise<WritingPrompt | null> {
  return postJSON<WritingPrompt | null>("api/writing/prompt", {}, null);
}

export function checkWriting(prompt: string, answer: string): Promise<Correction | null> {
  return postJSON<Correction | null>("api/writing/check", { prompt, answer }, null);
}
