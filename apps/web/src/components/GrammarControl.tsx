import type { Correction } from "../lib/protocol";
import { correctionHasIssues } from "../lib/turns";

// GrammarControl collapses the background grammar-check result into one
// small button, next to StudyControl's 🔊, instead of an always-visible card:
// it spins while correct() is still running for this turn, then opens a
// popover with the CorrectionCard (or a "no issues" message) on click.
// `failed` is a fourth, distinct state from pending/issues/clean: the
// analysis pass itself errored (see protocol.ServerEvent.Failed /
// store.Turn.CorrectionStatus) rather than running and finding nothing —
// without it, a failure was indistinguishable from "already correct" once
// the pending spinner cleared.
export function GrammarControl({
  index,
  pending,
  correction,
  failed,
  open,
  onToggle,
  panelRef,
}: {
  index: number;
  pending: boolean;
  correction?: Correction;
  failed?: boolean;
  open: boolean;
  onToggle: (index: number | null) => void;
  panelRef?: React.RefObject<HTMLDivElement | null>;
}) {
  if (!pending && !correction && !failed) return null; // no data (e.g. old session predating this feature)

  const hasIssues = !!correction && correctionHasIssues(correction);

  const glyph = pending ? "⏳" : failed ? "⚠" : hasIssues ? "✎" : "✓";
  const label = pending
    ? "문법 확인 중"
    : failed
      ? "문법 피드백 열기 (확인 실패, 자동으로 다시 시도해요)"
      : hasIssues
        ? "문법 피드백 열기"
        : "문법 피드백 열기 (문제 없음)";

  return (
    <div className="grammar-control" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn grammar-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-busy={pending}
        aria-label={label}
        disabled={pending}
        onClick={() => onToggle(open ? null : index)}
      >
        <span className={pending ? "spinning" : undefined}>{glyph}</span>
      </button>
      {open && (correction || failed) && (
        <div className="study-panel grammar-panel" role="menu">
          {failed ? (
            <div className="grammar-clean">문법 확인에 실패했어요. 자동으로 다시 시도할게요 🔁</div>
          ) : hasIssues ? (
            <CorrectionCard c={correction!} />
          ) : (
            <div className="grammar-clean">문법 문제가 없어요 👍</div>
          )}
        </div>
      )}
    </div>
  );
}

// Static UI labels for issue categories, in Korean. No LLM needed — the
// category set is fixed by the correction prompt.
const ISSUE_LABELS: Record<string, string> = {
  grammar: "문법",
  vocabulary: "어휘",
  phrasing: "표현",
  context: "문맥",
};

export function issueLabel(type: string): string {
  return ISSUE_LABELS[type] ?? type;
}

export function CorrectionCard({ c }: { c: Correction }) {
  const changed = c.corrected.trim() && c.corrected.trim() !== c.original.trim();
  return (
    <div className="correction">
      {changed && (
        <div className="fix">
          <span className="lbl">✎</span> {c.corrected}
        </div>
      )}
      {c.issues?.map((iss, i) => (
        <div key={i} className="issue">
          <span className={`badge ${iss.type}`}>{issueLabel(iss.type)}</span>
          <span className="span">{iss.span}</span> → <b>{iss.suggestion}</b>
          <div className="why">{iss.explanation}</div>
          <div className="why-translation">{iss.explanationTranslation}</div>
        </div>
      ))}
    </div>
  );
}
