import { useCallback, useEffect, useRef } from "react";

interface PollOptions<T> {
  intervalMs: number;
  maxAttempts?: number;
  fetchResult: () => Promise<T>;
  // Return true to keep watching. Callers decide whether a missing result
  // means retry or stop, since the background jobs have different policies.
  onResult: (result: T) => boolean;
}

// Assign a fresh token when changing rooms/jobs; several independent polls
// may share it. Invalidation stops watching and ignores late responses,
// while the server's durable job keeps running after navigation or unmount.
export function usePollScaffold() {
  const tokenRef = useRef<object | null>(null);
  const timersRef = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());

  const startPoll = useCallback(<T,>(token: object, {
    intervalMs, maxAttempts = Infinity, fetchResult, onResult,
  }: PollOptions<T>) => {
    let attempts = 0;
    const schedule = () => {
      const id = setTimeout(async () => {
        timersRef.current.delete(id);
        if (tokenRef.current !== token) return;
        const result = await fetchResult();
        if (tokenRef.current !== token) return;
        attempts++;
        const keepPolling = onResult(result);
        if (keepPolling && attempts < maxAttempts) schedule();
      }, intervalMs);
      timersRef.current.add(id);
    };
    schedule();
  }, []);

  useEffect(() => {
    const timers = timersRef.current;
    return () => {
      for (const id of timers) clearTimeout(id);
      timers.clear();
      tokenRef.current = null;
    };
  }, []);

  return { tokenRef, startPoll };
}
