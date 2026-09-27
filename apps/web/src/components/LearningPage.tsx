import type { ReactNode, UIEventHandler } from "react";
import { useViewScrollTop } from "../lib/listView";
import { SubPageHeader } from "./SubPageHeader";

// Each learning page has one scroll surface. Only navigation resets it;
// polling and loading older history must preserve the reader's position.
export function LearningPage({ title, viewKey = "list", onScroll, children }: {
  title: string;
  viewKey?: string;
  onScroll?: UIEventHandler<HTMLElement>;
  children: ReactNode;
}) {
  const pageRef = useViewScrollTop<HTMLElement>(viewKey);
  return (
    <div className="app">
      <SubPageHeader title={title} />
      <main ref={pageRef} className="convo learning-page" onScroll={onScroll}>
        {children}
      </main>
    </div>
  );
}
