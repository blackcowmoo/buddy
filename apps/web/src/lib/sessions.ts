import { fetchJSON, requestOK } from "./fetchJSON";
import type { Correction, InputSource, QuizQuestion, StudySummarySentence } from "./protocol";

// Mirrors store.SessionMeta / store.Turn (apps/server/internal/store/store.go).
export interface SessionSummary {
  id: string;
  title: string;
  createdAt: number;
  updatedAt: number;
  // Permanently true once the learner has confirmed "end this conversation"
  // (see endSession) — the room is read-only from then on, immediately:
  // ended flips before the wrap-up below even starts generating (see
  // studySummaryStatus). Optional (like the fields below) only so
  // hand-built test fixtures that predate this field don't all need
  // updating — a real API response always sets it.
  ended?: boolean;
  // The persisted study wrap-up, saved once studySummaryStatus reaches
  // "done" (see EndConversationControl in App.tsx) — each sentence in
  // English with a paired native-language translation; absent/empty until
  // then.
  studySummary?: StudySummarySentence[];
  // The end-of-conversation wrap-up job's status ("pending" | "done" |
  // "failed"), set the instant endSession freezes the room and updated by
  // the background job that actually generates studySummary (see
  // store.SessionMeta.StudySummaryStatus) — independent of whether this
  // client is even still around to see it land. Absent for a session that
  // hasn't been ended, or one ended before this became an async job (its
  // studySummary was already generated synchronously, so there's nothing
  // left to poll for) — EndConversationControl treats both the same as
  // "done".
  studySummaryStatus?: "pending" | "done" | "failed";
  // The pre-generated practice quiz (see store.SessionMeta.Quiz) — absent/
  // empty until quizStatus reaches "done". Generated alongside studySummary,
  // from the same flagged issues, right when the conversation ends, so
  // EndConversationControl's "퀴즈 풀기" button reads this instead of
  // triggering an LLM call itself.
  quiz?: QuizQuestion[];
  // Mirrors studySummaryStatus but for the quiz pre-generation job (see
  // store.SessionMeta.QuizStatus) — a separate status because the two jobs
  // run independently, in parallel, not one after the other.
  quizStatus?: "pending" | "done" | "failed";
  // Whether the learner has studied this session's quiz — either by
  // finishing every question (right or wrong; this isn't a "got it right"
  // flag), or (when quiz is empty) by acknowledging it via "내가 읽었음"
  // instead (see markQuizCompleted). A one-way checkmark: never cleared once
  // set. Drives the room list's "studied this" badge, and — once true —
  // EndConversationControl hides "퀴즈 풀기" in favor of "퀴즈 다시 만들기".
  quizCompleted?: boolean;
  // Final Judge corrections in this room that differ from the Chat preview
  // and have not yet been opened. Displayed only on the relevant room row.
  unreadCorrections?: number;
}

export interface TurnRecord {
  turn: number;
  role: "user" | "assistant";
  text: string;
  refined: boolean;
  // How the learner produced this turn — absent for assistant turns.
  source?: InputSource;
  correction?: Correction;
  translation?: string;
  // Unix seconds this turn was first saved. Optional only so older test
  // fixtures / hand-built records don't need updating — real API responses
  // always set it (see store.Turn.CreatedAt).
  createdAt?: number;
  // This turn's durable reply-job status ("pending" | "processing" | "done"
  // | "failed"), set only for an assistant turn whose reply was generated
  // via the async job queue (see store.Turn.ReplyStatus). Absent for a user
  // turn, and for an assistant turn that predates this feature or was never
  // queued — both mean "nothing to poll for."
  replyStatus?: "pending" | "processing" | "done" | "failed";
  // Mirrors replyStatus for the grammar-correction job on a user turn (see
  // store.Turn.CorrectionStatus). "failed" is what lets a reload tell "the
  // analysis errored" apart from "it ran and found nothing" (correction
  // present, empty issues) — both look the same in `correction` alone.
  correctionStatus?: "pending" | "processing" | "done" | "failed";
  correctionStage?: "chat" | "judge";
  correctionUnread?: boolean;
}

export interface SessionDetail {
  session: SessionSummary;
  turns: TurnRecord[];
  // Whether older turns exist beyond this page (see sessionDetailHandler's
  // ?before=/?limit= query params) — the cue to fetch another page when the
  // learner scrolls to the top of the transcript instead of assuming
  // `turns` is the whole history.
  hasMore: boolean;
}

// Mirrors the JSON shape written by httpserver.sessionCompactionHandler.
export interface SessionCompaction {
  summary: string;
  recentMessages: number;
  totalTurns: number;
}

// Fetches the caller's own chat rooms, most recently active first. Instant/
// "오늘의 한 문장" rooms (see markInstant) are excluded — fetchInstantSessions
// is their own separate list. Returns null on failure so the main room list
// can distinguish "there are no conversations yet" from "the conversations
// could not be loaded" and offer a retry instead of showing a false-empty
// state.
export async function fetchSessions(): Promise<SessionSummary[] | null> {
  return fetchJSON<SessionSummary[] | null>("api/sessions", null);
}

// Fetches the caller's own instant/"오늘의 한 문장" rooms, most recently active
// first — the mirror image of fetchSessions' exclusion, for that feature's
// own dedicated list page (pages/InstantSessions.tsx). That page keeps its
// existing [] fallback and previous empty-state-on-failure behavior.
export async function fetchInstantSessions(): Promise<SessionSummary[]> {
  return fetchJSON<SessionSummary[]>("api/instant-sessions", []);
}

// Flags a brand-new room as an instant/"오늘의 한 문장" conversation — called
// once, right after the "ready" WS event hands the client the server-minted
// session ID for a room opened in quick mode (see App.tsx). Returns whether
// the request succeeded; the caller treats this as fire-and-forget either
// way (see the comment at its call site).
export async function markInstant(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}/instant`, { method: "POST" });
}

// Fetches one page of a room's transcript for replay, most recent turns
// first — opts.before (a turn-number cursor, for fetching the page older
// than one already loaded) and opts.limit (page size) mirror
// sessionDetailHandler's query params. Omitting opts.limit gets the
// server's default page (defaultSessionPageLimit); opts.limit: 0
// deliberately asks for the *whole* transcript instead (see
// pollMissingFeedback, which needs every turn, not just the latest page) —
// so limit/before are only left off the query string when actually
// undefined, not just falsy. Returns null on any failure — network error,
// non-200 (including a 404 for someone else's session ID), or bad JSON.
export async function fetchSessionDetail(
  id: string,
  opts?: { before?: number; limit?: number },
): Promise<SessionDetail | null> {
  const params = new URLSearchParams();
  if (opts?.before !== undefined) params.set("before", String(opts.before));
  if (opts?.limit !== undefined) params.set("limit", String(opts.limit));
  const query = params.toString();
  return fetchJSON<SessionDetail | null>(
    `api/sessions/${encodeURIComponent(id)}${query ? `?${query}` : ""}`,
    null,
  );
}

// Fetches how much of a room's conversation is still sent to the LLM
// verbatim vs. folded into its rolling summary (see internal/session's
// compaction — nothing here is ever dropped from the transcript itself,
// only from the copy sent to the model). Returns null on any failure.
export async function fetchSessionCompaction(id: string): Promise<SessionCompaction | null> {
  return fetchJSON<SessionCompaction | null>(
    `api/sessions/${encodeURIComponent(id)}/compaction`,
    null,
  );
}

// Marks an ended session's quiz as studied (see
// httpserver.sessionQuizCompleteHandler) — called once a learner finishes
// every quiz question, right or wrong (see QuizPanel), or, for a session
// whose pre-generated quiz came back with no questions at all, when the
// learner taps "내가 읽었음" instead (see EndConversationControl). Drives the
// room list's "studied this" badge (see quizCompleted on SessionSummary)
// once the room is left and the list refreshes. Returns whether the request
// itself succeeded.
export async function markQuizCompleted(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}/quiz/complete`, { method: "POST" });
}

// Acknowledges a final correction after its feedback panel is actually
// opened. The server owns the read state so it stays consistent across
// reloads/devices instead of disappearing from local React state alone.
export async function markCorrectionRead(id: string, turn: number): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}/corrections/${turn}/read`, { method: "POST" });
}

// Forces an ended session's pre-generated quiz to regenerate from scratch
// (see httpserver.sessionQuizResetHandler) — the "퀴즈 다시 만들기" button in
// EndConversationControl. Unlike restudySession, the server accepts this
// even when the existing quiz already has real questions: the whole point
// is replacing them with a fresh set (e.g. one written before
// answerMeaning/acceptableAnswers existed), not just recovering a stuck
// empty result. Returns whether the reset request itself succeeded, not
// whether the regenerated quiz has landed yet — callers still poll
// quizStatus for that, same as endSession.
export async function resetQuiz(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}/quiz/reset`, { method: "POST" });
}

// Deletes one chat room and its transcript (the server also cascades to any
// recordings archived under it — see httpserver.sessionDeleteHandler).
// Returns whether the request succeeded, so the caller can decide what to do
// on failure (e.g. leave the room in the list) instead of assuming success.
export async function deleteSession(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// Confirms "end this conversation": freezes the room read-only immediately
// (see httpserver.sessionEndHandler) and kicks off the study wrap-up as a
// background job from there — this call returns as soon as the room is
// frozen, not once the wrap-up (or its fold into the learner's
// cross-session profile) is actually done; see studySummaryStatus on the
// session, which the frontend polls to show that separately. Returns
// whether the freeze itself succeeded.
export async function endSession(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}/end`, { method: "POST" });
}

// Forces an ended session's study-summary wrap-up to regenerate when it
// landed showing "no issues" (see httpserver.sessionRestudyHandler) — the
// "다시 확인하기" button in EndConversationControl, shown only in that exact
// state, since the server itself refuses this call unless studySummaryStatus
// is already "done" with an empty summary: GenerateStudySummary occasionally
// returns a valid-but-empty result despite real issues in the transcript, and
// that state has no automatic way back on its own. Returns whether the
// restart request itself succeeded, not whether the regenerated summary has
// landed yet — callers still poll studySummaryStatus for that, same as
// endSession.
export async function restudySession(id: string): Promise<boolean> {
  return requestOK(`api/sessions/${encodeURIComponent(id)}/restudy`, { method: "POST" });
}

// URL for one chat turn's read-aloud audio — the learner's own line or the
// assistant's reply (role disambiguates the two sharing a turn number, see
// store.Turn) — generated on demand (the first time a learner asks to hear
// this specific message, not eagerly for every reply) and cached
// server-side, so a plain <audio src> works the same way
// recordingAudioURL's/articleAudioURL's does: point at it directly and
// call play(), same as App.tsx's playMessage. Unlike "오늘의 아티클"'s
// read-aloud, this can't be pre-generated — each line is unique to its own
// conversation, not known until the learner actually picks it.
export function messageAudioURL(sessionId: string, turn: number, role: "user" | "assistant"): string {
  return `api/sessions/${encodeURIComponent(sessionId)}/messages/${turn}/audio?role=${role}`;
}
