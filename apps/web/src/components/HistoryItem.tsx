import type { ComponentProps } from "react";
import { HistoryItemContent } from "./HistoryItemContent";

type HistoryItemProps = ComponentProps<typeof HistoryItemContent> & {
  onDelete: () => void;
  deleteLabel: string;
} & (
  | { href: string; onOpen?: never; openLabel?: never; disabled?: never }
  | { href?: never; onOpen: () => void; openLabel?: string; disabled?: boolean }
);

// Keep deletion beside the open control, so it cannot trigger navigation.
// Links retain native browser navigation; in-page workflows use buttons.
export function HistoryItem({ href, onOpen, openLabel, disabled, onDelete, deleteLabel, ...content }: HistoryItemProps) {
  return (
    <div className="session-row">
      {href !== undefined ? (
        <a className="session-item" href={href}><HistoryItemContent {...content} /></a>
      ) : (
        <button type="button" className="session-item" onClick={onOpen} aria-label={openLabel} disabled={disabled}>
          <HistoryItemContent {...content} />
        </button>
      )}
      <button type="button" className="ghost icon-btn session-delete" onClick={onDelete}
        aria-label={deleteLabel} title={deleteLabel} disabled={disabled}>
        🗑
      </button>
    </div>
  );
}
