// StudyControl plays a message's read-aloud audio, at the globally
// configured playback rate (see lib/ttsSettings.ts's loadPlaybackRate, set
// via TopBar's hamburger-menu settings) — one immediate tap, not a popover
// of rate choices: the old per-tap rate popover rendered below the button
// and could end up clipped behind the chat's own fixed layout near the
// bottom of the screen, especially on the newest message.
export function StudyControl({
  turn,
  role,
  onPlay,
}: {
  turn: number;
  role: "user" | "assistant";
  onPlay: (turn: number, role: "user" | "assistant") => void;
}) {
  return (
    <button
      type="button"
      className="ghost icon-btn study-btn"
      aria-label="읽어주기"
      onClick={() => onPlay(turn, role)}
    >
      🔊
    </button>
  );
}
