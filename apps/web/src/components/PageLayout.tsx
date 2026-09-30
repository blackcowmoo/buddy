import { useId, useRef, type ReactNode, type UIEventHandler } from "react";
import { useViewScrollTop } from "../lib/listView";

// Keep the header and optional composer outside the single scrolling region.
// Pages supply content; they should not recreate the viewport grid.
export function AppShell({ header, footer, children }: {
  header: ReactNode;
  footer?: ReactNode;
  children: ReactNode;
}) {
  const shellRef = useRef<HTMLDivElement>(null);
  return (
    <div className="app" ref={shellRef}>
      <button
        type="button"
        className="skip-content"
        onClick={() => shellRef.current?.querySelector("main")?.focus()}
      >
        본문으로 건너뛰기
      </button>
      {header}
      {children}
      {footer}
    </div>
  );
}

export function PageContent({ viewKey = "list", onScroll, className = "", children }: {
  viewKey?: string;
  onScroll?: UIEventHandler<HTMLElement>;
  className?: string;
  children: ReactNode;
}) {
  const pageRef = useViewScrollTop<HTMLElement>(viewKey);
  return (
    <main ref={pageRef} className={`page-content ${className}`.trim()} tabIndex={-1} onScroll={onScroll}>
      {children}
    </main>
  );
}

export function PageSection({ title, description, actions, children, className = "" }: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const titleId = useId();
  return (
    <section className={`page-section ${className}`.trim()} aria-labelledby={titleId}>
      <div className="section-heading">
        <div className="section-heading-copy">
          <h2 id={titleId}>{title}</h2>
          {description && <p>{description}</p>}
        </div>
        {actions && <div className="section-heading-actions">{actions}</div>}
      </div>
      <div className="page-section-content">{children}</div>
    </section>
  );
}

export function PageToolbar({ children, label = "주요 작업", className = "" }: {
  children: ReactNode;
  label?: string;
  className?: string;
}) {
  return <div className={`page-toolbar ${className}`.trim()} role="group" aria-label={label}>{children}</div>;
}

type BackButtonProps = { children?: ReactNode } & (
  | { href: string; onClick?: never; disabled?: never }
  | { href?: never; onClick: () => void; disabled?: boolean }
);

export function BackButton({ children = "목록으로", ...props }: BackButtonProps) {
  const content = <><span aria-hidden="true">←</span>{children}</>;
  return props.href !== undefined
    ? <a className="ghost page-back" href={props.href}>{content}</a>
    : <button type="button" className="ghost page-back" onClick={props.onClick} disabled={props.disabled}>{content}</button>;
}
