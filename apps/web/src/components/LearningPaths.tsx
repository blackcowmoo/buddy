import { learningDestinations } from "../lib/navigation";
import { LearningIcon } from "./LearningIcon";

export function LearningPaths() {
  return (
    <nav className="learning-paths" aria-labelledby="learning-paths-title">
      <div className="section-heading">
        <div className="section-heading-copy">
          <h2 id="learning-paths-title">오늘은 무엇을 해 볼까요?</h2>
          <p>마음이 가는 연습부터 골라 보세요.</p>
        </div>
      </div>
      <div className="learning-path-grid">
        {learningDestinations.map(({ href, icon, label, description }) => (
          <a className="learning-path" href={href} key={href}>
            <span className="learning-path-icon"><LearningIcon path={icon} /></span>
            <span className="learning-path-copy"><strong>{label}</strong>{" "}<span className="learning-path-description">{description}</span></span>
            <span className="learning-path-arrow" aria-hidden="true">↗</span>
          </a>
        ))}
      </div>
    </nav>
  );
}
