import { useCallback, useRef, useState } from "react";
import { useDismiss } from "../hooks/useDismiss";

// Shared top bar for every sub-page reached from the room list (Recordings,
// WordReview, WordMatch, InstantSessions, ArticleQuiz, Writing): a title plus
// a compact menu back to the main page. The link is relative ("." not "/")
// so it resolves against the current page URL and still works under a
// ROOT_PATH prefix like "/pr/14/words".
export function SubPageHeader({ title }: { title: string }) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const closeMenu = useCallback(() => setMenuOpen(false), []);
  useDismiss(menuOpen, menuRef, closeMenu);

  return (
    <header className="topbar">
      <div className="brand">
        <h1>{title}</h1>
      </div>
      <div className="menu" ref={menuRef}>
        <button
          type="button"
          className="ghost icon-btn"
          onClick={() => setMenuOpen((open) => !open)}
          aria-label="메뉴"
          aria-haspopup="true"
          aria-expanded={menuOpen}
          title="메뉴"
        >
          ☰
        </button>
        {menuOpen && (
          <div className="menu-panel subpage-menu-panel" role="menu">
            <a className="ghost menu-item" href="." role="menuitem">
              <span aria-hidden="true">🏠</span> 메인으로
            </a>
          </div>
        )}
      </div>
    </header>
  );
}
