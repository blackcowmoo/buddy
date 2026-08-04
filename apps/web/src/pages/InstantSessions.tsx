import { Fragment, useEffect, useState } from "react";
import { confirmThenDelete } from "../lib/confirmDelete";
import { deleteSession, fetchInstantSessions, type SessionSummary } from "../lib/sessions";
import { formatDateDivider, formatMessageTime, isSameDay } from "../lib/time";

// Every "인스턴트 대화" room (see App.tsx's markInstant) lives here instead of
// the main room list — one exchange each, so this page exists to review a
// room's title/time, reopen it (read-only, since it's always already ended
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
      <header className="topbar">
        <div className="brand">
          <h1>인스턴트 대화</h1>
        </div>
        {/* Relative link (not "/"): resolves against the current page URL,
            same reasoning as PCMRecorder's worklet URL, so this still works
            under a ROOT_PATH prefix like "/pr/14/instant". */}
        <a className="ghost icon-btn" href="." aria-label="대화로 돌아가기" title="대화로 돌아가기">
          ←
        </a>
      </header>

      <main className="convo instant-sessions-list">
        {!loaded && <p className="hint">불러오는 중…</p>}
        {loaded && (
          <p className="hint instant-sessions-hint">
            지금까지 총 {sessions.length}번 했어요. 잘못 응답한 대화는 삭제하면 학습 데이터에서 제외돼요.
          </p>
        )}
        {loaded && sessions.length === 0 && <p className="hint">아직 인스턴트 대화가 없습니다.</p>}
        {sessions.map((s, i) => {
          const prev = sessions[i - 1];
          const showDivider = !prev || !isSameDay(prev.createdAt, s.createdAt);
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
