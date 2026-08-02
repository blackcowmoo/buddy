import { useMemo, useRef, useState } from "react";
import type { Correction } from "../lib/protocol";
import { useDismiss } from "../hooks/useDismiss";
import { issueLabel, CorrectionCard } from "./GrammarControl";

interface FeedbackTurn {
  turn: number;
  text: string;
  correction: Correction;
}

// Session-wide counterpart to GrammarControl: instead of one popover per
// message, this is a single button (placed in the chat header) that lists
// every turn's feedback collected so far, so a learner can review recurring
// mistakes mid-conversation instead of only one bubble at a time. Owns its
// own open state (like GrammarControl/StudyControl) since it isn't tied to
// any one message row.
export function FeedbackSummary({ turns }: { turns: FeedbackTurn[] }) {
  const [open, setOpen] = useState(false);
  const panelRef = useRef<HTMLDivElement>(null);
  useDismiss(open, panelRef, () => setOpen(false));

  const issueCounts = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const t of turns) {
      for (const iss of t.correction.issues ?? []) {
        counts[iss.type] = (counts[iss.type] ?? 0) + 1;
      }
    }
    return counts;
  }, [turns]);

  return (
    <div className="feedback-summary" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn feedback-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-label="피드백 모아보기"
        title="피드백 모아보기"
        onClick={() => setOpen((o) => !o)}
      >
        📋
      </button>
      {open && (
        <div className="study-panel feedback-panel" role="menu">
          {turns.length === 0 ? (
            <div className="feedback-empty">아직 피드백이 없어요 👍</div>
          ) : (
            <>
              <div className="feedback-summary-header">
                지금까지 {turns.length}개 메시지에 피드백이 있어요
              </div>
              {Object.keys(issueCounts).length > 0 && (
                <div className="feedback-summary-counts">
                  {Object.entries(issueCounts).map(([type, count]) => (
                    <span key={type} className={`badge ${type}`}>
                      {issueLabel(type)} {count}
                    </span>
                  ))}
                </div>
              )}
              {turns.map((t) => (
                <div key={t.turn} className="feedback-entry">
                  <div className="feedback-original">{t.text}</div>
                  <CorrectionCard c={t.correction} />
                </div>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}
