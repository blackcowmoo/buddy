import { useState } from "react";
import type { Theme } from "../lib/theme";
import { NATIVE_RATE, RATE_PRESETS } from "../lib/ttsSettings";
import { MAX_INTERLOCUTOR_STYLE_LEN } from "../lib/settings";
import { ThemeSwitch } from "./ThemeSwitch";

// Read-only view onto the learner's persistent cross-session profile (see
// settings.ts/store.Store.GetLearnerProfile) — recurring mistakes,
// interests, proficiency trend, folded in from every ended session's study
// summary and carried into every future session's system prompt regardless
// of which chat room it's in. Tucked behind its own toggle rather than shown
// inline: unlike the interlocutor-style form right above it, this is
// LLM-authored prose with no length cap, so showing it by default could
// dominate the menu. Local, unlifted open state is enough — MenuPanel itself
// unmounts (and so does this) whenever the hamburger menu closes.
function LearnerProfileControl({ profile, loadError }: { profile: string; loadError: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="learner-profile">
      <button
        type="button"
        className="ghost menu-item"
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="true"
        aria-expanded={open}
      >
        🧠 학습 프로필 보기
      </button>
      {open && (
        <div className="learner-profile-panel">
          <div className="learner-profile-hint">
            지금까지 마친 대화들을 요약해 다음 대화에 반영하는 내용이에요.
          </div>
          {loadError ? (
            <span className="style-load-error">불러오지 못했습니다. 새로고침 후 다시 시도해주세요.</span>
          ) : profile ? (
            <div className="learner-profile-text">{profile}</div>
          ) : (
            <div className="learner-profile-empty">아직 정리된 내용이 없어요. 대화를 마치면 쌓여요.</div>
          )}
        </div>
      )}
    </div>
  );
}

interface ChatMenuProps {
  autoReadAloud: boolean;
  onToggleAutoReadAloud: () => void;
  playbackRate: number;
  onSetPlaybackRate: (rate: number) => void;
}

// Shared by both the room list and the chat header (see App's two return
// branches) — only the brand content (back button + status dot vs. just the
// title) and whether chat-only MenuPanel items are shown differ between them.
export function TopBar({
  brand,
  actions,
  menuOpen,
  onToggleMenu,
  menuRef,
  ...menuPanelProps
}: {
  brand: React.ReactNode;
  actions?: React.ReactNode;
  menuOpen: boolean;
  onToggleMenu: () => void;
  menuRef: React.RefObject<HTMLDivElement | null>;
} & React.ComponentProps<typeof MenuPanel>) {
  return (
    <header className="topbar">
      <div className="brand">{brand}</div>
      <div className="menu" ref={menuRef}>
        {actions}
        <button
          className="ghost icon-btn"
          onClick={onToggleMenu}
          aria-label="Menu"
          aria-haspopup="true"
          aria-expanded={menuOpen}
          title="Menu"
        >
          ☰
        </button>
        {menuOpen && <MenuPanel {...menuPanelProps} />}
      </div>
    </header>
  );
}

// Identity, theme, and PR-preview nav — reachable via TopBar before a room is
// ever opened. `chat` carries the items that only make sense once a room is
// open (voice/playback speed) — omitted on the list.
function MenuPanel({
  email,
  theme,
  onThemeChange,
  prInput,
  onPrInputChange,
  prError,
  onGoToPath,
  onGoToRecordings,
  onGoToInstant,
  onGoToWords,
  onGoToMatch,
  onGoToArticle,
  wordDueCount,
  styleInput,
  onStyleInputChange,
  styleSaving,
  styleSaved,
  styleLoadError,
  styleSaveError,
  onSubmitStyle,
  learnerProfile,
  chat,
}: {
  email: string | null;
  theme: Theme;
  onThemeChange: (t: Theme) => void;
  prInput: string;
  onPrInputChange: (v: string) => void;
  prError: boolean;
  onGoToPath: (e: React.FormEvent) => void;
  onGoToRecordings: () => void;
  onGoToInstant: () => void;
  onGoToWords: () => void;
  onGoToMatch: () => void;
  onGoToArticle: () => void;
  wordDueCount: number;
  styleInput: string;
  onStyleInputChange: (v: string) => void;
  styleSaving: boolean;
  styleSaved: boolean;
  styleLoadError: boolean;
  styleSaveError: string | null;
  onSubmitStyle: (e: React.FormEvent) => void;
  learnerProfile: string;
  chat?: ChatMenuProps;
}) {
  return (
    <div className="menu-panel" role="menu">
      <div className="menu-row user-info">
        <span className="user-email">{email ?? "익명 사용자"}</span>
      </div>
      <div className="menu-divider" />
      {chat && (
        <>
          <div className="menu-row">
            <button
              type="button"
              className={chat.autoReadAloud ? "pill ok" : "ghost"}
              onClick={chat.onToggleAutoReadAloud}
              aria-pressed={chat.autoReadAloud}
            >
              {chat.autoReadAloud ? "🔊 답장 자동 읽기 켜짐" : "답장 자동 읽기 꺼짐"}
            </button>
          </div>
          <div className="menu-row tts-settings">
            <div className="tts-settings-label">재생 속도</div>
            <div className="rate-presets" role="group" aria-label="재생 속도 선택">
              {RATE_PRESETS.map((r) => (
                <button
                  key={r}
                  type="button"
                  className={r === chat.playbackRate ? "rate-preset-btn active" : "rate-preset-btn"}
                  aria-pressed={r === chat.playbackRate}
                  onClick={() => chat.onSetPlaybackRate(r)}
                >
                  {r === NATIVE_RATE ? `🔊 ${r}x` : `${r}x`}
                </button>
              ))}
            </div>
          </div>
          <div className="menu-divider" />
        </>
      )}
      <div className="menu-row">
        <ThemeSwitch theme={theme} onChange={onThemeChange} />
      </div>
      <div className="menu-divider" />
      <form className="style-form" onSubmit={onSubmitStyle}>
        <label htmlFor="interlocutor-style">대화 상대 스타일</label>
        <textarea
          id="interlocutor-style"
          value={styleInput}
          onChange={(e) => onStyleInputChange(e.target.value)}
          placeholder="예: 면접관처럼 질문해줘 / 전문가처럼 답변해줘"
          rows={2}
          disabled={styleLoadError}
        />
        <div className="style-form-row">
          <button type="submit" disabled={styleSaving || styleLoadError}>
            {styleSaving ? "저장 중…" : "저장"}
          </button>
          <span
            className={
              Array.from(styleInput.trim()).length > MAX_INTERLOCUTOR_STYLE_LEN
                ? "style-char-count style-char-count-over"
                : "style-char-count"
            }
          >
            {Array.from(styleInput.trim()).length}/{MAX_INTERLOCUTOR_STYLE_LEN}
          </span>
          {styleSaved && <span className="style-saved">저장됨</span>}
          {styleSaveError && <span className="style-save-error">{styleSaveError}</span>}
          {styleLoadError && (
            <span className="style-load-error">불러오지 못했습니다. 새로고침 후 다시 시도해주세요.</span>
          )}
        </div>
      </form>
      <div className="menu-divider" />
      <LearnerProfileControl profile={learnerProfile} loadError={styleLoadError} />
      <div className="menu-divider" />
      <button className="ghost menu-item" onClick={onGoToRecordings} role="menuitem">
        🎧 녹음 목록
      </button>
      <button className="ghost menu-item" onClick={onGoToInstant} role="menuitem">
        ⚡ 인스턴트 대화 목록
      </button>
      <button className="ghost menu-item" onClick={onGoToWords} role="menuitem">
        📚 단어 복습
        {wordDueCount > 0 && <span className="menu-badge">{wordDueCount}</span>}
      </button>
      <button className="ghost menu-item" onClick={onGoToMatch} role="menuitem">
        🎮 단어 매칭 게임
      </button>
      <button className="ghost menu-item" onClick={onGoToArticle} role="menuitem">
        📰 오늘의 아티클
      </button>
      <div className="menu-divider" />
      <form className="path-form" onSubmit={onGoToPath}>
        <label htmlFor="pr-path">PR 미리보기로 이동</label>
        <div className="path-row">
          <input
            id="pr-path"
            value={prInput}
            onChange={(e) => onPrInputChange(e.target.value)}
            placeholder="PR 번호 (예: 14)"
            inputMode="numeric"
            autoComplete="off"
          />
          <button type="submit">이동</button>
        </div>
        {prError && <p className="path-error">숫자만 입력하세요 (비워두면 메인으로 이동)</p>}
      </form>
    </div>
  );
}
