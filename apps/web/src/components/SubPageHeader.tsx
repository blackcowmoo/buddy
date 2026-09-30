import { useCallback, useRef, useState } from "react";
import { PageHeader } from "./PageHeader";
import { LearningMenuItems } from "./LearningMenuItems";
import { useDismiss } from "../hooks/useDismiss";
import { useLearningDueCounts } from "../hooks/useLearningDueCounts";

// The hamburger keeps the same learning destinations as the main screen and
// adds a relative home link, so every page remains one menu away from the
// complete app navigation under preview deployments such as "/pr/14/words".
function SubPageLearningMenu() {
  const { wordDueCount, nuanceDueCount } = useLearningDueCounts();
  return <LearningMenuItems wordDueCount={wordDueCount} nuanceDueCount={nuanceDueCount} />;
}

export function SubPageHeader({ title }: { title: string }) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const closeMenu = useCallback(() => setMenuOpen(false), []);
  useDismiss(menuOpen, menuRef, closeMenu);

  return (
    <PageHeader brand={<h1>{title}</h1>} menuOpen={menuOpen} onToggleMenu={() => setMenuOpen((open) => !open)} menuRef={menuRef}>
      <a className="ghost menu-item" href="." role="menuitem">
        <span aria-hidden="true">🏠</span> 메인으로
      </a>
      <div className="menu-divider" />
      <SubPageLearningMenu />
    </PageHeader>
  );
}
