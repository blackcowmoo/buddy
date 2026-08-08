import { useCallback, useEffect, useRef } from "react";

// Shared plumbing behind every "poll until some background job finishes"
// loop in this app (App.tsx's pollMissingFeedback/pollStudySummary/
// pollQuizStatus, WordReview.tsx's auto-add-word poll, ArticleQuiz.tsx's
// article-study poll):
//
// - tokenRef identifies the most recent poll chain a caller cares about, so
//   a slow fetch that resolves after the caller has moved on to something
//   else (left the room, started a new draw) doesn't apply its now-stale
//   result — callers set tokenRef.current to a fresh `{}` when starting a
//   new chain and compare against it on every tick.
// - schedulePoll is the only way a tick should schedule its own next
//   attempt: it tracks the timer so the unmount cleanup below can cancel it.
//   A tick already in flight can't be cancelled, only ignored (via the
//   token), but this stops every not-yet-fired one — without it, a poll
//   chain would keep running past unmount, up to maxAttempts × intervalMs
//   worth of fetches after the component using it is gone.
export function usePollScaffold() {
  const tokenRef = useRef<object | null>(null);
  const timersRef = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());

  const schedulePoll = useCallback((tick: () => void, ms: number) => {
    const id = setTimeout(() => {
      timersRef.current.delete(id);
      tick();
    }, ms);
    timersRef.current.add(id);
  }, []);

  useEffect(() => {
    const timers = timersRef.current;
    return () => {
      for (const id of timers) clearTimeout(id);
      timers.clear();
      tokenRef.current = null;
    };
  }, []);

  return { tokenRef, schedulePoll };
}
