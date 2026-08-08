// Playback-speed presets for the per-message TTS buttons (see App.tsx), and
// the auto-read-aloud toggle. Pure client preferences, so both live in
// localStorage rather than the server.
import { readStored, writeStored } from "./storedValue";

const STORAGE_KEY = "buddy.tts.extraRates";
const AUTO_READ_ALOUD_KEY = "buddy.tts.autoReadAloud";

export const NATIVE_RATE = 1;
export const MAX_EXTRA_RATES = 2;
const DEFAULT_EXTRA_RATES = [0.5, 0.8];
// Applied as the <audio> element's own playbackRate (see App.tsx's
// playMessage/ArticleQuiz.tsx's handleRead) — 0.5-2.0 is comfortably inside
// what browsers support and where speech stays intelligible; the same
// pitch-corrected time-stretching behind HTMLMediaElement.playbackRate
// starts sounding artifact-y well outside it.
const MIN_RATE = 0.5;
const MAX_RATE = 2;

export function isValidExtraRate(n: unknown): n is number {
  return typeof n === "number" && Number.isFinite(n) && n >= MIN_RATE && n <= MAX_RATE && n !== NATIVE_RATE;
}

function sanitize(rates: unknown[]): number[] {
  return [...new Set(rates.filter(isValidExtraRate))].slice(0, MAX_EXTRA_RATES);
}

/** Reads the extra playback speeds, falling back to the defaults if unset or corrupt. */
export function loadExtraRates(): number[] {
  return readStored(
    STORAGE_KEY,
    (raw) => {
      const parsed: unknown = JSON.parse(raw);
      return Array.isArray(parsed) ? sanitize(parsed) : undefined;
    },
    DEFAULT_EXTRA_RATES,
  );
}

/** Persists the extra playback speeds, deduped and capped at MAX_EXTRA_RATES. */
export function saveExtraRates(rates: number[]): void {
  writeStored(STORAGE_KEY, JSON.stringify(sanitize(rates)));
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
