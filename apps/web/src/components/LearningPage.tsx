import type { ReactNode, UIEventHandler } from "react";
import { AppShell, PageContent } from "./PageLayout";
import { SubPageHeader } from "./SubPageHeader";

// Each learning page has one scroll surface. Only navigation resets it;
// polling and loading older history must preserve the reader's position.
export function LearningPage({ title, viewKey = "list", onScroll, children }: {
  title: string;
  viewKey?: string;
  onScroll?: UIEventHandler<HTMLElement>;
  children: ReactNode;
}) {
  return (
    <AppShell header={<SubPageHeader title={title} />}>
      <PageContent viewKey={viewKey} className="learning-page" onScroll={onScroll}>
        {children}
      </PageContent>
    </AppShell>
  );
}
