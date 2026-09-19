import { Fragment, useEffect, useState } from "react";
import { EmptyState, LearningIntro } from "../components/LearningIntro";
import { confirmThenDelete } from "../lib/confirmDelete";
import { deleteSession, fetchInstantSessions, type SessionSummary } from "../lib/sessions";
import { formatDateDivider, formatMessageTime, shouldShowDateDivider } from "../lib/time";
import { LoadingHint } from "../components/LoadingHint";
import { SubPageHeader } from "../components/SubPageHeader";

// Every "인스턴트 대화" room (see App.tsx's markInstant) lives here instead of
// the main room list — one exchange each, so this page lets learners start
// a fresh instant conversation or review a room's title/time and reopen it
// (read-only, since it's always already ended
// by the time it shows up here — see App.tsx's quickEndingRef) to see its
// transcript and grammar feedback, and, if a reply came back wrong, delete
// the transcript outright (the one way to keep a bad exchange out of future
// study material — see deleteSession/sessionDeleteHandler).
// Grouped by day (see formatDateDivider), same convention the chat
// transcript itself uses, since these rooms are short and numerous rather
// than few and long. Like the main room list (fetchSessions), a failed
// fetch just renders as empty rather than a distinct error state —
// fetchInstantSessions can't tell the two apart either (same
// swallow-and-degrade contract as fetchJSON), so there's nothing more
// specific to show.
export function InstantSessions() {
  const [loaded, setLoaded] = useState(false);
  const [sessions, setSessions] = useState<SessionSummary[]>([]);

  useEffect(() => {
    fetchInstantSessions().then((list) => {
      setSessions(list);
      setLoaded(true);
    });
  }, []);

  const handleDelete = (id: string) =>
    confirmThenDelete("이 인스턴트 대화를 삭제할까요? 학습 데이터에서도 제외돼요.", deleteSession, id, setSessions);

  return (
    <div className="app">
      <SubPageHeader title="인스턴트 대화" />

      <main className="convo instant-sessions-list">
        <LearningIntro eyebrow="짧게 연습하고, 차근차근 돌아보기" title="한 문장도 좋은 연습이에요" description="한 번씩 나눈 짧은 대화를 모았어요. 대화를 열어 답변과 피드백을 다시 살펴보세요." />
        <div>
          <button
            type="button"
            className="new-chat"
            aria-describedby="instant-start-description"
            onClick={() => window.location.assign(".#instant/new")}
          >
            인스턴트 대화 시작
          </button>
          <p id="instant-start-description" className="action-description">한 문장으로 가볍게 연습해요</p>
        </div>
        {!loaded && <LoadingHint />}
        {loaded && (
          <p className="hint instant-sessions-hint">
            지금까지 총 {sessions.length}번 했어요. 잘못 응답한 대화는 삭제하면 학습 데이터에서 제외돼요.
          </p>
        )}
        {loaded && sessions.length === 0 && <EmptyState title="아직 인스턴트 대화가 없어요." description="위의 ‘인스턴트 대화 시작’을 눌러 문장 하나를 보내 보세요. 답변과 피드백을 받은 뒤 대화가 마무리돼요." />}
        {sessions.map((s, i) => {
          const prev = sessions[i - 1];
          const showDivider = shouldShowDateDivider(prev?.createdAt, s.createdAt);
          return (
            <Fragment key={s.id}>
              {showDivider && (
                <div className="date-divider">
                  <span>{formatDateDivider(s.createdAt)}</span>
                </div>
              )}
              <div className="session-row instant-session-row">
                {/* Relative href (not "/"), same reasoning as the back link
                    above: resolves to the chat page's own URL regardless of
                    ROOT_PATH, and the "#chat/<id>" hash is what App.tsx's
                    mount effect reads (see lib/roomHistory.parseRoomHash) to
                    open this exact room read-only instead of the list —
                    the one way back into a room's transcript/feedback once
                    you've left this page. */}
                <a className="session-item" href={`.#chat/${encodeURIComponent(s.id)}`}>
                  {s.quizCompleted && (
                    <span className="quiz-completed-badge" title="퀴즈까지 모두 완료했어요">
                      ✅
                    </span>
                  )}
                  {!!s.unreadCorrections && (
                    <span className="correction-unread-badge" title={`읽지 않은 정밀 피드백 ${s.unreadCorrections}개`}>
                      새 피드백 {s.unreadCorrections}
                    </span>
                  )}
                  <span className="title">{s.title}</span>
                  <span className="time">{formatMessageTime(s.createdAt)}</span>
                </a>
                <button
                  type="button"
                  className="ghost icon-btn session-delete"
                  onClick={() => void handleDelete(s.id)}
                  aria-label="인스턴트 대화 삭제"
                  title="인스턴트 대화 삭제"
                >
                  🗑
                </button>
              </div>
            </Fragment>
          );
        })}
      </main>
    </div>
  );
}
