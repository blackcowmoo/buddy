import { fetchJSON, postJSON, requestOK } from "./fetchJSON";

export interface NuanceWord {
  word: string;
  tone: string;
  description: string;
  example: string;
  translation: string;
}
export interface NuanceQuestion {
  id: string;
  context: string;
  sentence: string;
  translation: string;
  answer: string;
  explanation: string;
}
export interface NuanceContent {
  meaning: string;
  distinction: string;
  caveat: string;
  words: NuanceWord[];
  questions: NuanceQuestion[];
}
export interface NuanceProgress {
  stage: number;
  attempts: number;
  correct: number;
  lastReviewedAt: number;
  nextReviewAt: number;
}
export interface NuanceLesson {
  id: string;
  status: "pending" | "processing" | "done" | "failed";
  createdAt: number;
  revision: number;
  content?: NuanceContent;
  state: {
    progress: Record<string, NuanceProgress>;
    queue: string[];
    feedback?: { questionId: string; selected: string; correct: boolean };
  };
}
export interface NuanceAction {
  kind: "start" | "answer" | "next";
  revision: number;
  questionId?: string;
  selected?: string;
  repeat?: boolean;
}
export const fetchNuanceLessons = () => fetchJSON<NuanceLesson[] | null>("api/nuance", null);
export const fetchNuanceLesson = (id: string) => fetchJSON<NuanceLesson | null>(`api/nuance/${encodeURIComponent(id)}`, null);
export const drawNuanceLesson = () => postJSON<NuanceLesson | null>("api/nuance/draw", {}, null);
export const retryNuanceLesson = (id: string) => postJSON<NuanceLesson | null>(`api/nuance/${encodeURIComponent(id)}/retry`, {}, null);
export const practiceNuance = (id: string, action: NuanceAction) => postJSON<NuanceLesson | null>(`api/nuance/${encodeURIComponent(id)}/practice`, action, null);
export const deleteNuanceLesson = (id: string) => requestOK(`api/nuance/${encodeURIComponent(id)}`, { method: "DELETE" });

export function dueQuestions(lesson: NuanceLesson, now = Date.now() / 1000): number {
  return lesson.content?.questions.filter((q) => (lesson.state.progress[q.id]?.nextReviewAt ?? 0) <= now).length ?? 0;
}
export function nextReview(lesson: NuanceLesson): number | null {
  const times = lesson.content?.questions.map((q) => lesson.state.progress[q.id]?.nextReviewAt ?? 0);
  return times?.length ? Math.min(...times) : null;
}
export function lessonTitle(lesson: NuanceLesson): string {
  return lesson.content?.words.map((w) => w.word).join(" / ") || (lesson.status === "failed" ? "문제 생성 실패" : "새 비교 묶음을 만드는 중…");
}

// A draw's UUID distributes the initial positions. Rotation moves every choice
// on a retry, remains stable during reveal, and survives reopening on a device.
export function nuanceOptions(lesson: NuanceLesson, questionID: string, attempt: number): NuanceWord[] {
  const words = lesson.content?.words ?? [];
  if (!words.length) return [];
  let seed = 0;
  for (const char of `${lesson.id}/${questionID}`) seed = (Math.imul(seed, 31) + char.charCodeAt(0)) >>> 0;
  const offset = (seed + attempt) % words.length;
  return [...words.slice(offset), ...words.slice(0, offset)];
}
