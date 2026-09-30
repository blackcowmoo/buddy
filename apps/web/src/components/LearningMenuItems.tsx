import { learningDestinations, learningDestinationForPage } from "../lib/navigation";
import { currentPage } from "../lib/route";
import { LearningIcon } from "./LearningIcon";

export function LearningMenuItems({
  wordDueCount = 0,
  nuanceDueCount = 0,
}: {
  wordDueCount?: number;
  nuanceDueCount?: number;
}) {
  const activeDestination = learningDestinationForPage(currentPage(window.location.pathname));
  return (
    <>
      {learningDestinations.map((item) => {
        const dueCount = item.href === "words" ? wordDueCount : item.href === "nuance" ? nuanceDueCount : 0;
        return (
          <a key={item.href} className="ghost menu-item" href={item.href} role="menuitem" aria-current={activeDestination?.href === item.href ? "page" : undefined}>
            <span className="menu-item-icon"><LearningIcon path={item.icon} /></span>
            <span>{item.label}</span>
            {dueCount > 0 && <span className="menu-badge">{dueCount}</span>}
          </a>
        );
      })}
    </>
  );
}
