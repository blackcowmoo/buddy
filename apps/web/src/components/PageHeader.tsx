import { useId, type ReactNode, type RefObject } from "react";
import { MenuIcon } from "./MenuIcon";

export function PageHeader({
  brand,
  actions,
  menuOpen,
  onToggleMenu,
  menuRef,
  children,
}: {
  brand: ReactNode;
  actions?: ReactNode;
  menuOpen: boolean;
  onToggleMenu: () => void;
  menuRef: RefObject<HTMLDivElement | null>;
  children: ReactNode;
}) {
  const menuId = useId();
  return (
    <header className="topbar">
      <div className="brand">{brand}</div>
      <div className="menu" ref={menuRef}>
        {actions}
        <button
          type="button"
          className="ghost icon-btn"
          onClick={onToggleMenu}
          aria-label="메뉴"
          aria-haspopup="true"
          aria-expanded={menuOpen}
          aria-controls={menuOpen ? menuId : undefined}
          title="메뉴"
        >
          <MenuIcon />
        </button>
        {menuOpen && <div id={menuId} className="menu-panel" role="menu">{children}</div>}
      </div>
    </header>
  );
}
