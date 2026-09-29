import { useEffect, useRef, useState } from "react";
import { articleAudioURL } from "../lib/articles";
import { requestAmbientAudioSession } from "../lib/audioSession";
import { loadPlaybackRate } from "../lib/ttsSettings";

type PlaybackState = "idle" | "loading" | "speaking" | "error";
const labels: Record<PlaybackState, string> = {
  idle: "🔊 읽어주기",
  loading: "불러오는 중…",
  speaking: "재생 중…",
  error: "재생 실패, 다시 시도해주세요",
};

// Mounted with the reading view: leaving it discards playback state and
// cancels pending callbacks, so another article always starts idle.
export function ArticleReadAloud({ articleId }: { articleId: string }) {
  const [state, setState] = useState<PlaybackState>("idle");
  const audioRef = useRef<HTMLAudioElement>(null);
  const attemptRef = useRef(0);

  useEffect(() => {
    if (state !== "error") return;
    const timer = setTimeout(() => setState("idle"), 2000);
    return () => clearTimeout(timer);
  }, [state]);

  useEffect(() => {
    const audio = audioRef.current;
    return () => {
      attemptRef.current++;
      audio?.pause();
    };
  }, []);

  const play = () => {
    const audio = audioRef.current;
    if (!audio) return;
    const attempt = ++attemptRef.current;
    // Request the mixing policy synchronously in the click, before play().
    requestAmbientAudioSession();
    setState("loading");
    audio.playbackRate = loadPlaybackRate();
    audio.src = articleAudioURL(articleId);
    void audio.play().catch((error) => {
      if (attemptRef.current !== attempt) return;
      console.error("tts:", error);
      setState("error");
    });
  };

  const cancel = () => {
    attemptRef.current++;
    const audio = audioRef.current;
    if (audio) {
      audio.pause();
      audio.currentTime = 0;
    }
    setState("idle");
  };

  return <>
    <audio ref={audioRef} style={{ display: "none" }}
      onPlaying={() => setState("speaking")}
      onWaiting={() => setState("loading")}
      onEnded={() => setState("idle")}
      onError={() => setState("error")}
    />
    <button type="button" className="ghost article-read-aloud-btn" onClick={play} disabled={state !== "idle"}>
      {labels[state]}
    </button>
    {(state === "loading" || state === "speaking") && (
      <button type="button" className="ghost article-read-aloud-btn" onClick={cancel}>취소</button>
    )}
  </>;
}
