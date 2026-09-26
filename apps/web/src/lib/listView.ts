import { useLayoutEffect, useRef } from "react";

// List endpoints should normally return their newest rows first, but keeping
// that UX rule at the rendering boundary prevents one backend/store ordering
// difference from putting a newly created item below the fold.
export function newestFirst<T>(items: readonly T[], timestamp: (item: T) => number): T[] {
  return [...items].sort((a, b) => timestamp(b) - timestamp(a));
}

// Every page owns its scrolling element (the document itself never scrolls).
// Reset when the user changes between list/detail views, but not when polling,
// deletion, or another in-place data refresh updates the current view.
export function useViewScrollTop<T extends HTMLElement>(viewKey: string) {
  const ref = useRef<T>(null);
  useLayoutEffect(() => {
    if (ref.current) ref.current.scrollTop = 0;
  }, [viewKey]);
  return ref;
}
