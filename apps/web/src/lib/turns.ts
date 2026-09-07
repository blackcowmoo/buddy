import type { Correction, InputSource } from "./protocol";
import type { TurnRecord } from "./sessions";

export interface Msg {
  turn: number;
  role: "user" | "assistant";
  text: string;
  refined?: boolean;
  // How the learner produced this turn — absent for assistant turns.
  source?: InputSource;
  // Unix seconds this turn first appeared — captured client-side the moment
  // a live event creates the row (final_transcript / the first
  // assistant_delta), or hydrated from store.Turn.CreatedAt on reload.
  // Optional so a turn hydrated from an older record (no createdAt in that
  // fixture/row) just skips its divider/time instead of showing junk.
  timestamp?: number;
}

function isPendingPlaceholder(t: TurnRecord): boolean {
  return t.role === "assistant" && !t.text;
}

// Everything the UI tracks per turn beyond the transcript text itself
// (msgs), keyed by turn number the same way msgs is. A user turn and its
// paired assistant reply share one turn number, hence the role-prefixed
// translation fields instead of one generic pair.
export interface TurnMeta {
  correction?: Correction;
  correctionPending?: boolean;
  // True when the grammar-check pass itself errored rather than finding
  // nothing to flag — see GrammarControl, which shows a distinct icon for
  // this instead of leaving the pending spinner stuck or silently showing
  // nothing (the old, indistinguishable-from-"already correct" behavior).
  correctionFailed?: boolean;
  correctionUnread?: boolean;
  userTranslation?: string;
  userTranslationPending?: boolean;
  assistantTranslation?: string;
  assistantTranslationPending?: boolean;
}

// Builds the TurnMeta patch for one already-hydrated (non-placeholder) turn
// from its correction/translation state — shared by enterChat and
// loadOlderTurns. `recentlyActive` extends correctionPending to a hydrated
// user turn with no correctionStatus at all (see enterChat's own comment);
// loadOlderTurns always passes false, since its older pages predate the
// "recently active" window enterChat cares about. `pending` reports whether
// this turn is still missing a result worth polling for — enterChat uses it
// to decide whether to start pollMissingFeedback; loadOlderTurns doesn't
// poll its older pages at all, so it ignores this field.
function hydrateTurnMeta(t: TurnRecord, recentlyActive: boolean): { meta: TurnMeta; pending: boolean } {
  const meta: TurnMeta = {};
  let pending = false;
  if (t.correction) {
    meta.correction = t.correction;
    meta.correctionUnread = !!t.correctionUnread;
    if (t.correctionStage === "chat" || t.correctionStatus === "pending" || t.correctionStatus === "processing") {
      // A persisted Chat preview is already readable while its Analysis /
      // Judge stages keep running in the background.
      meta.correctionPending = true;
      pending = true;
    }
  } else if (t.correctionStatus === "failed") {
    // Durably recorded as failed — the reaper (internal/asyncjob) still
    // retries it from scratch on its own, so keep polling while showing the
    // failed state instead of a spinner.
    meta.correctionFailed = true;
    pending = true;
  } else if (t.correctionStatus === "pending" || t.correctionStatus === "processing") {
    meta.correctionPending = true;
    pending = true;
  } else if (t.role === "user" && t.text && recentlyActive) {
    // Missing correction on a hydrated user turn: correct() runs detached
    // from the connection (see pipeline.HandleText) so it keeps going and
    // persists even after the learner leaves the room — show the hourglass
    // as still in-progress instead of dropping it, and poll until the
    // result lands (see pollMissingFeedback).
    meta.correctionPending = true;
    pending = true;
  }
  const savedTranslation = t.translation || (t.role === "user" ? t.correction?.translation : undefined);
  if (savedTranslation) {
    if (t.role === "user") meta.userTranslation = savedTranslation;
    else meta.assistantTranslation = savedTranslation;
  } else if (t.text) {
    // Missing translation on a hydrated turn: the server queues backfill
    // for it the moment this fetch lands (see
    // httpserver.sessionDetailHandler), so show it as in-progress rather
    // than silently absent, and poll until it lands.
    if (t.role === "user") meta.userTranslationPending = true;
    else meta.assistantTranslationPending = true;
    pending = true;
  }
  return { meta, pending };
}

export interface HydratedTurnPage {
  messages: Msg[];
  metadata: Record<number, TurnMeta>;
  pending: boolean;
  awaitingReply: boolean;
}

// Converts one API page into the two state collections used by App. Keeping
// placeholder handling and same-turn metadata merging here prevents initial
// hydration and older-page hydration from drifting apart.
export function hydrateTurnPage(turns: TurnRecord[], recentlyActive = false): HydratedTurnPage {
  const messages: Msg[] = [];
  const metadata: Record<number, TurnMeta> = {};
  let pending = false;
  let awaitingReply = false;

  for (const turn of turns) {
    if (isPendingPlaceholder(turn)) {
      if (turn.replyStatus === "pending" || turn.replyStatus === "processing") {
        pending = true;
        awaitingReply = true;
      }
      continue;
    }
    messages.push({
      turn: turn.turn,
      role: turn.role,
      text: turn.text,
      refined: turn.refined,
      source: turn.source,
      timestamp: turn.createdAt,
    });
    const hydrated = hydrateTurnMeta(turn, recentlyActive);
    metadata[turn.turn] = { ...metadata[turn.turn], ...hydrated.meta };
    pending ||= hydrated.pending;
  }

  return { messages, metadata, pending, awaitingReply };
}

export function upsertAssistant(m: Msg[], turn: number, patch: (prev: string) => string): Msg[] {
  // Scanned back-to-front rather than with findIndex: this runs once per
  // streamed token (assistant_delta), and the row being appended to is
  // always among the newest — a forward scan would re-walk the whole loaded
  // transcript (which grows with every loadOlderTurns page) per token.
  let i = -1;
  for (let j = m.length - 1; j >= 0; j--) {
    if (m[j].turn === turn && m[j].role === "assistant") {
      i = j;
      break;
    }
  }
  if (i >= 0) {
    const copy = [...m];
    copy[i] = { ...copy[i], text: patch(copy[i].text) };
    return copy;
  }
  return [
    ...m,
    { turn, role: "assistant", text: patch(""), timestamp: Math.floor(Date.now() / 1000) },
  ];
}

// Whether a Correction actually flags something worth showing, vs. the
// sentence coming back unchanged with no issues (the "clean" case).
export function correctionHasIssues(c: Correction): boolean {
  return (
    c.corrected.trim().toLowerCase() !== c.original.trim().toLowerCase() ||
    (c.issues?.length ?? 0) > 0
  );
}
