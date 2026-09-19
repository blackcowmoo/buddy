import type { SessionSummary } from "../lib/sessions";
import { formatRelativeTime } from "../lib/time";
import { EmptyState, LearningIntro } from "./LearningIntro";
import { LearningPaths } from "./LearningPaths";

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
      <LearningIntro eyebrow="조금씩, 나만의 속도로" title="오늘도 영어와 가까워져요" description="완벽한 문장이 아니어도 괜찮아요. 하고 싶은 이야기부터 시작해 보세요." />
      <div className="conversation-starts">
        <div>
          <button className="new-chat" aria-describedby="free-chat-description" onClick={() => onOpen()} disabled={openingId !== null}>
            + 새 대화
          </button>
          <p id="free-chat-description" className="action-description">Buddy와 자유롭게 이야기해요</p>
        </div>
        <div>
          <button
            className="new-chat ghost quick-chat"
            aria-describedby="quick-chat-description"
            onClick={() => onOpen(undefined, true)}
            disabled={openingId !== null}
          >
            ✏️ 인스턴트 대화
          </button>
          <p id="quick-chat-description" className="action-description">한 문장으로 가볍게 연습해요</p>
        </div>
      </div>

      <LearningPaths />

      <div className="section-heading history-heading">
        <h2>나의 대화 기록</h2>
        <a className="text-link" href="instant">인스턴트 기록 <span aria-hidden="true">→</span></a>
      </div>

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
        <EmptyState title="아직 대화 기록이 없어요" description="위의 ‘새 대화’를 눌러 첫 이야기를 나눠 보세요. 나눈 대화와 피드백은 여기에서 다시 볼 수 있어요." />
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
