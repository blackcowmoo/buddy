import type { ReactNode } from "react";

// Titles get their own line; status and time can wrap independently without
// squeezing the title or pushing the row's delete action out of view.
export function HistoryItemContent({ title, description, badges, meta }: {
  title: string;
  description?: string;
  badges?: ReactNode;
  meta: ReactNode;
}) {
  return (
    <span className="history-item-content">
      <span className="title" title={title}>{title}</span>{" "}
      {description && <><span className="history-item-description">{description}</span>{" "}</>}
      <span className="history-item-meta">
        {badges}{" "}
        <span className="time">{meta}</span>
      </span>
    </span>
  );
}
