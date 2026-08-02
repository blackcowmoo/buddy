import { useCallback, useRef, useState } from "react";
import { useDismiss } from "./useDismiss";

// usePopoverFetch is the state machine behind the header's fetch-on-open
// popovers: a panel that owns its open state, dismisses like every other one
// (useDismiss), and only fetches when it's actually opened — an on-demand
// affordance, not something worth a request on every room entry. Data is
// cleared as the fetch starts so a reopen can't flash the previous room's
// (or previous point in this room's) result while the new one is in flight.
// Returns null data until the first fetch resolves; a failed fetch resolves
// to null too, which callers render as their own "couldn't load" message.
// EndConversationControl doesn't use this: it has nothing of its own to
// fetch on open — it just renders whatever the room's own SessionDetail
// already carries (ended/studySummary/studySummaryStatus), the same data
// enterChat already fetched to open the room in the first place.
export function usePopoverFetch<T>(sessionId: string | null, fetchData: (sessionId: string) => Promise<T | null>) {
  const [open, setOpen] = useState(false);
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  useDismiss(open, panelRef, () => setOpen(false));

  const toggle = useCallback(() => {
    if (!sessionId) return;
    setOpen((o) => !o);
    if (open) return; // was open, now closing — nothing to fetch
    setLoading(true);
    setData(null);
    void fetchData(sessionId).then((result) => {
      setData(result);
      setLoading(false);
    });
  }, [sessionId, open, fetchData]);

  return { open, toggle, loading, data, panelRef };
}
