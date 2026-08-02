// Playback-speed presets for the per-message TTS buttons (see App.tsx). Pure
// client preference, so it lives in localStorage rather than the server.
import { readStored, writeStored } from "./storedValue";

const STORAGE_KEY = "buddy.tts.extraRates";

export type TtsState = "idle" | "loading" | "ready" | "error";

export const NATIVE_RATE = 1;
export const MAX_EXTRA_RATES = 2;
const DEFAULT_EXTRA_RATES = [0.5, 0.8];
// Kokoro's `speed` generation parameter isn't clamped by the model itself,
// but 0.5-2.0 is the range its own demo exposes and where speech stays
// intelligible; outside it the model tends to produce artifacts.
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
