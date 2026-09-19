import { useId } from "react";

export function LearningIntro({ eyebrow, title, description, steps }: {
  eyebrow: string;
  title: string;
  description: string;
  steps?: string[];
}) {
  const titleId = useId();
  return (
    <section className="learning-intro" aria-labelledby={titleId}>
      <p className="learning-eyebrow">{eyebrow}</p>
      <h2 id={titleId}>{title}</h2>
      <p className="learning-description">{description}</p>
      {steps && <ol className="learning-steps" aria-label="학습 순서">
        {steps.map((step) => <li key={step}>{step}</li>)}
      </ol>}
    </section>
  );
}

export function EmptyState({ title, description, href, action }: {
  title: string;
  description: string;
  href?: string;
  action?: string;
}) {
  return (
    <div className="empty-state">
      <p className="empty-state-title">{title}</p>
      <p>{description}</p>
      {href && action && <a className="text-link" href={href}>{action} <span aria-hidden="true">→</span></a>}
    </div>
  );
}
