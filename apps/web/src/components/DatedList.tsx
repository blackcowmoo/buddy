import { Fragment, type ReactNode } from "react";
import { formatDateDivider, shouldShowDateDivider } from "../lib/time";

// Keep the caller's ordering and pagination intact. A divider marks a
// change in local calendar day, including when older history is appended.
export function DatedList<T extends { id: string; createdAt: number }>({ items, children }: {
  items: readonly T[];
  children: (item: T) => ReactNode;
}) {
  return items.map((item, index) => (
    <Fragment key={item.id}>
      {shouldShowDateDivider(items[index - 1]?.createdAt, item.createdAt) && (
        <div className="date-divider"><span>{formatDateDivider(item.createdAt)}</span></div>
      )}
      {children(item)}
    </Fragment>
  ));
}
