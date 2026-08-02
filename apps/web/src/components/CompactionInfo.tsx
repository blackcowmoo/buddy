import { usePopoverFetch } from "../hooks/usePopoverFetch";
import { fetchSessionCompaction } from "../lib/sessions";

// Debug view onto internal/session's compaction: shows the room's rolling
// summary and how many recent messages are still sent to the LLM verbatim,
// so a learner can confirm a long conversation is actually being compacted
// instead of just trusting it. Purely informational — nothing shown here is
// ever dropped from the room's own transcript (see FeedbackSummary/the
// message list above, which always replay the full history), only from the
// copy of the conversation sent to the model (see
// httpserver.sessionCompactionHandler). Fetches on open rather than
// eagerly, since this is a debug affordance, not something shown by default.
export function CompactionInfo({ sessionId }: { sessionId: string | null }) {
  const {
    open,
    toggle,
    loading,
    data: info,
    panelRef,
  } = usePopoverFetch(sessionId, fetchSessionCompaction);

  if (!sessionId) return null;

  return (
    <div className="compaction-info" ref={panelRef}>
      <button
        type="button"
        className="ghost icon-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-label="대화 압축 상태 보기"
        title="대화 압축 상태 보기"
        onClick={toggle}
      >
        🗜️
      </button>
      {open && (
        <div className="study-panel compaction-panel" role="menu">
          {loading && <div className="compaction-loading">불러오는 중…</div>}
          {!loading && info && (
            <>
              <div className="compaction-summary-header">
                전체 {info.totalTurns}턴 중 최근 {info.recentMessages}개 메시지는 그대로 전달되고,
                이전 대화는 아래처럼 요약되어 있어요. (기록 자체는 그대로 남아있어요.)
              </div>
              <div className="compaction-summary-text">
                {info.summary || "아직 압축된 내용이 없어요."}
              </div>
            </>
          )}
          {!loading && !info && <div className="compaction-empty">불러오지 못했어요.</div>}
        </div>
      )}
    </div>
  );
}
