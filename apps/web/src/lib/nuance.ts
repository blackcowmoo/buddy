import { fetchJSON, postJSON, requestOK } from "./fetchJSON";

interface NuanceWord {
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
interface NuanceContent {
  meaning: string;
  distinction: string;
  caveat: string;
  words: NuanceWord[];
  questions: NuanceQuestion[];
}
interface NuanceProgress {
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
    // previousStage is the question's stage going into the graded answer, so
    // a reset (stage 0) is visible even though stage now already holds the
    // outcome of that answer. Feedback saved before the server reported it
    // omits the field.
    feedback?: { questionId: string; selected: string; correct: boolean; previousStage?: number };
  };
}
export interface NuanceAction {
  kind: "start" | "answer" | "next";
  revision: number;
  questionId?: string;
  selected?: string;
  repeat?: boolean;
}
export interface NuanceReviewItem {
  lessonId: string;
  questionId: string;
}
interface NuanceReviewBatch {
  items: NuanceReviewItem[];
  lessons: NuanceLesson[];
}
export const fetchNuanceLessons = () => fetchJSON<NuanceLesson[] | null>("api/nuance", null);
export const fetchNuanceLesson = (id: string) => fetchJSON<NuanceLesson | null>(`api/nuance/${encodeURIComponent(id)}`, null);
// words is the optional pair the learner wants to compare; omit it (or send
// anything the server cannot validate) for a random draw.
export const drawNuanceLesson = (words?: string[]) =>
  postJSON<NuanceLesson | null>("api/nuance/draw", words?.length ? { words } : {}, null);
// The server owns the review batch: GET replays the saved batch, POST draws a
// fresh one. Items never travel through the URL, so large batches cannot
// overflow the browser and the order cannot be tampered with.
export const fetchNuanceReview = () => fetchJSON<NuanceReviewBatch | null>("api/nuance/review", null);
export const startNuanceReview = () => postJSON<NuanceReviewBatch | null>("api/nuance/review", {}, null);
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
// Repeating a correct context holds its stage and reuses the interval that
// just elapsed, so the action only means something when there is a step
// forward to give up. A miss resets the stage to 0 (see the server's shared
// NextSchedule), so after a miss — or on a first-ever attempt — the learner
// already has the reset progress and repeating would only cost the stage the
// answer earned. Withhold it there, as the word review page withholds its
// forced-guess button at stage 0.
export function canRepeatQuestion(lesson: NuanceLesson): boolean {
  const feedback = lesson.state.feedback;
  return !!feedback?.correct && (feedback.previousStage ?? 0) > 0;
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
