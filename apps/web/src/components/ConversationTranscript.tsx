import { Fragment, type RefObject, type UIEventHandler } from "react";
import type { Status } from "../lib/ws";
import { formatDateDivider, formatMessageTime, shouldShowDateDivider } from "../lib/time";
import type { Msg, TurnMeta } from "../lib/turns";
import { GrammarControl } from "./GrammarControl";
import { StudyControl } from "./StudyControl";

interface ConversationTranscriptProps {
  ended: boolean;
  status: Status;
  loadingMore: boolean;
  quickMode: boolean;
  messages: Msg[];
  turns: Record<number, TurnMeta>;
  openGrammarIndex: number | null;
  onToggleGrammar: (index: number | null) => void;
  grammarPanelRef: RefObject<HTMLDivElement | null>;
  onPlay: (turn: number, role: "user" | "assistant") => void;
  awaitingReply: boolean;
  scrollRef: RefObject<HTMLElement | null>;
  latestMessageRef: RefObject<HTMLDivElement | null>;
  onScroll: UIEventHandler<HTMLElement>;
}

// Transcript rendering is kept separate from App's connection and polling
// state machine. A message row depends only on persisted/live message state,
// which makes this the single place to inspect when changing chat display.
export function ConversationTranscript({
  ended,
  status,
  loadingMore,
  quickMode,
  messages,
  turns,
  openGrammarIndex,
  onToggleGrammar,
  grammarPanelRef,
  onPlay,
  awaitingReply,
  scrollRef,
  latestMessageRef,
  onScroll,
}: ConversationTranscriptProps) {
  return (
    <main className="convo" ref={scrollRef} onScroll={onScroll}>
      {!ended && status !== "open" && (
        <p
          className={`connection-notice ${status === "connecting" ? "" : "error"}`}
          role="status"
        >
          {status === "connecting"
            ? "대화에 연결하는 중이에요. 지금 작성해도 연결되면 자동으로 전송돼요."
            : "연결이 끊겼어요. 자동으로 다시 연결하고 있으며, 작성한 메시지는 연결 후 전송돼요."}
        </p>
      )}
      {loadingMore && (
        <p className="hint" role="status" aria-label="이전 대화 불러오는 중">
          <span className="spinning">⏳</span>
        </p>
      )}
      {quickMode && !ended && (
        <p className="hint quick-mode-hint" role="status">
          ✏️ 인스턴트 대화: 문장을 하나 보내면 답변과 피드백을 받고 바로 마무리돼요.
        </p>
      )}
      {messages.length === 0 && (
        <p className="hint">
          <strong>🎙</strong>을 누르고 영어로 말한 뒤 다시 누르세요. 인식된 문장을 확인·수정하고
          전송하면 됩니다. 아래에 직접 입력할 수도 있고, 메시지의 <strong>🔊</strong>을 누르면
          발음을 들을 수 있어요.
        </p>
      )}
      {messages.map((message, index) => {
        const meta = turns[message.turn];
        const translation = message.role === "user" ? meta?.userTranslation : meta?.assistantTranslation;
        const translationPending = message.role === "user"
          ? meta?.userTranslationPending
          : meta?.assistantTranslationPending;
        const showDivider = shouldShowDateDivider(messages[index - 1]?.timestamp, message.timestamp);
        const grammarOpen = openGrammarIndex === index;
        return (
          // A stable turn/role key lets older pages prepend without remounting
          // every message that was already visible.
          <Fragment key={`${message.turn}-${message.role}`}>
            {showDivider && (
              <div className="date-divider">
                <span>{formatDateDivider(message.timestamp as number)}</span>
              </div>
            )}
            <div className={`row ${message.role}`}>
              <div className="bubble">{message.text || <span className="cursor">▋</span>}</div>
              {message.timestamp != null && (
                <span className="msg-time">{formatMessageTime(message.timestamp)}</span>
              )}
              {translation ? (
                <div className="translation">{translation}</div>
              ) : (
                translationPending && (
                  <div className="translation translation-pending" role="status" aria-label="번역 중">
                    <span className="spinning">⏳</span>
                  </div>
                )
              )}
              {message.text && (
                <div className="msg-tools">
                  {message.role === "user" && message.source && (
                    <span
                      className="source-icon"
                      title={message.source === "voice" ? "음성으로 입력함" : "채팅으로 입력함"}
                    >
                      {message.source === "voice" ? "🎙" : "⌨️"}
                    </span>
                  )}
                  {message.role === "user" && message.refined && <span className="tag">refined</span>}
                  {message.role === "user" && (
                    <GrammarControl
                      index={index}
                      pending={!!meta?.correctionPending}
                      correction={meta?.correction}
                      failed={!!meta?.correctionFailed}
                      unread={!!meta?.correctionUnread}
                      open={grammarOpen}
                      onToggle={onToggleGrammar}
                      panelRef={grammarOpen ? grammarPanelRef : undefined}
                    />
                  )}
                  <StudyControl turn={message.turn} role={message.role} onPlay={onPlay} />
                </div>
              )}
            </div>
          </Fragment>
        );
      })}
      {awaitingReply && (
        <div className="row assistant">
          <div className="bubble typing" role="status" aria-label="답변 생성 중">
            <span className="dot" />
            <span className="dot" />
            <span className="dot" />
          </div>
        </div>
      )}
      <div ref={latestMessageRef} className="latest-message-anchor" aria-hidden="true" />
    </main>
  );
}
