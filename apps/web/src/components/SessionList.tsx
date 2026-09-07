import type { SessionSummary } from "../lib/sessions";
import { formatRelativeTime } from "../lib/time";

interface SessionListProps {
  sessions: SessionSummary[];
  loading: boolean;
  loadError: boolean;
  actionError: string | null;
  openingId: string | null;
  deletingId: string | null;
  onOpen: (id?: string, quick?: boolean) => void;
  onDelete: (id: string) => void;
  onRetry: () => void;
  onDismissError: () => void;
}

// The room-list view is intentionally presentation-only. App owns fetching,
// history navigation, and destructive actions; this component only maps that
// state to controls, so either side can be understood without the other.
export function SessionList({
  sessions,
  loading,
  loadError,
  actionError,
  openingId,
  deletingId,
  onOpen,
  onDelete,
  onRetry,
  onDismissError,
}: SessionListProps) {
  return (
    <main className="session-list">
      <button className="new-chat" onClick={() => onOpen()} disabled={openingId !== null}>
        + 새 대화
      </button>
      <button
        className="new-chat ghost quick-chat"
        onClick={() => onOpen(undefined, true)}
        disabled={openingId !== null}
      >
        ✏️ 인스턴트 대화
      </button>

      {loadError && (
        <div className="list-notice error" role="alert">
          <span>대화 목록을 불러오지 못했어요.</span>
          <button type="button" className="ghost" onClick={onRetry} disabled={loading}>
            {loading ? "불러오는 중…" : "다시 시도"}
          </button>
        </div>
      )}
      {actionError && (
        <div className="list-notice error" role="alert">
          <span>{actionError}</span>
          <button type="button" className="ghost" onClick={onDismissError}>
            닫기
          </button>
        </div>
      )}
      {loading && sessions.length === 0 && !loadError && (
        <p className="hint list-loading" role="status">
          <span className="spinning" aria-hidden="true">⏳</span> 대화 목록을 불러오는 중이에요…
        </p>
      )}
      {!loading && !loadError && sessions.length === 0 && (
        <p className="hint">아직 대화 기록이 없어요. 새 대화를 시작해보세요.</p>
      )}
      {sessions.length > 0 && (
        <ul>
          {sessions.map((session) => {
            const opening = openingId === session.id;
            const deleting = deletingId === session.id;
            return (
              <li key={session.id} className="session-row">
                <button
                  className="session-item"
                  onClick={() => onOpen(session.id)}
                  disabled={openingId !== null || deletingId !== null}
                  aria-busy={opening}
                >
                  {session.ended && (
                    <span className="ended-badge" title="종료된 대화 (읽기 전용)">🔒</span>
                  )}
                  {session.ended && session.studySummaryStatus === "pending" && (
                    <span className="study-summary-pending-badge" title="학습 피드백을 정리하는 중">
                      <span className="spinning">⏳</span> 정리 중
                    </span>
                  )}
                  {session.ended && session.quizCompleted && (
                    <span className="quiz-completed-badge" title="퀴즈까지 모두 완료했어요">
                      ✅ 학습 완료
                    </span>
                  )}
                  {!!session.unreadCorrections && (
                    <span
                      className="correction-unread-badge"
                      title={`읽지 않은 정밀 피드백 ${session.unreadCorrections}개`}
                    >
                      새 피드백 {session.unreadCorrections}
                    </span>
                  )}
                  <span className="title">{session.title}</span>
                  <span className="time">
                    {opening ? "불러오는 중…" : formatRelativeTime(session.updatedAt)}
                  </span>
                </button>
                <button
                  type="button"
                  className="ghost icon-btn session-delete"
                  onClick={() => onDelete(session.id)}
                  disabled={deletingId !== null || openingId !== null}
                  aria-label={deleting ? "대화 삭제 중" : "대화 삭제"}
                  title={deleting ? "삭제 중" : "대화 삭제"}
                >
                  <span className={deleting ? "spinning" : undefined}>{deleting ? "⏳" : "🗑"}</span>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </main>
  );
}
