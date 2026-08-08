// Playback-speed preference for read-aloud audio (chat's per-message 🔊
// button and "오늘의 아티클"'s read-aloud, see App.tsx's playAudio/
// ArticleQuiz.tsx's handleRead) — one global rate, set once via the
// hamburger menu (see TopBar's MenuPanel) and applied to every play,
// instead of a per-tap choice from a popover. The old popover (several rate
// buttons behind each message's 🔊 tap) rendered below the button and could
// end up clipped behind the chat's own fixed layout near the bottom of the
// screen; a single global preference needs no popover at all. Pure client
// preference, so it lives in localStorage rather than the server, same as
// the auto-read-aloud toggle below.
import { readStored, writeStored } from "./storedValue";

const STORAGE_KEY = "buddy.tts.playbackRate";
const AUTO_READ_ALOUD_KEY = "buddy.tts.autoReadAloud";

export const NATIVE_RATE = 1;
// Applied as the <audio> element's own playbackRate — 0.5-2.0 is comfortably
// inside what browsers support and where speech stays intelligible; the same
// pitch-corrected time-stretching behind HTMLMediaElement.playbackRate
// starts sounding artifact-y well outside it.
const MIN_RATE = 0.5;
const MAX_RATE = 2;
const DEFAULT_RATE = NATIVE_RATE;

// Preset choices shown as tappable pills in the settings menu — covers the
// practice-pace range most learners want without needing a numeric input.
export const RATE_PRESETS = [0.5, 0.6, 0.7, 0.8, 0.9, NATIVE_RATE, 1.25, 1.5];

export function isValidRate(n: unknown): n is number {
  return typeof n === "number" && Number.isFinite(n) && n >= MIN_RATE && n <= MAX_RATE;
}

/** Reads the configured playback rate, falling back to the native 1x default if unset or corrupt. */
export function loadPlaybackRate(): number {
  return readStored(
    STORAGE_KEY,
    (raw) => {
      const parsed: unknown = JSON.parse(raw);
      return isValidRate(parsed) ? parsed : undefined;
    },
    DEFAULT_RATE,
  );
}

/** Persists the playback rate preference, falling back to the default for an out-of-range value. */
export function savePlaybackRate(rate: number): void {
  writeStored(STORAGE_KEY, JSON.stringify(isValidRate(rate) ? rate : DEFAULT_RATE));
}

/**
 * Whether a new assistant reply should read itself aloud automatically as
 * soon as it arrives (see App.tsx's assistant_done handler), vs. only ever
 * playing when the learner taps a message's own 🔊 button. Defaults off —
 * same default as the old client-side "음성 활성화" step this replaces, back
 * when enabling voice also meant downloading a model; now that read-aloud
 * is generated server-side on demand (see lib/sessions.ts's
 * messageAudioURL), there's no load step left, just this one preference.
 */
export function loadAutoReadAloud(): boolean {
  return readStored(AUTO_READ_ALOUD_KEY, (raw) => JSON.parse(raw) === true, false);
}

/** Persists the auto-read-aloud preference. */
export function saveAutoReadAloud(enabled: boolean): void {
  writeStored(AUTO_READ_ALOUD_KEY, JSON.stringify(enabled));
}
