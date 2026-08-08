import { KokoroTTS } from "kokoro-js";

// The Audio Session API (https://github.com/w3c/audio-session) isn't in
// lib.dom.d.ts yet — Safari is currently the only implementer. Declared
// locally rather than pulled in as a dependency for one property.
declare global {
  interface Navigator {
    audioSession?: { type: "auto" | "playback" | "transient" | "transient-solo" | "ambient" | "play-and-record" };
  }
}

// kokoro-82M runs fully in the browser (WebGPU, WASM fallback). The model is
// ~80–300 MB depending on dtype, so it is lazy-loaded on first use.
const MODEL_ID = "onnx-community/Kokoro-82M-v1.0-ONNX";

// A few known-good voices. See the kokoro-js model card for the full list
// (af_* American female, am_* American male, bf_*/bm_* British, etc.).
export type KokoroVoice = "af_heart" | "af_bella" | "af_sarah" | "am_adam" | "am_echo";

// generate() runs the whole passage through phonemization + a single ONNX
// forward pass with no progress signal and no internal timeout — on the
// WASM fallback (no WebGPU) that has been observed to take minutes on a
// phone, or apparently never resolve at all, leaving the UI stuck on
// "재생 중…" forever with no error. Bounding it means a slow/stuck device at
// least surfaces as a retryable failure instead of hanging indefinitely.
export const GENERATION_TIMEOUT_MS = 45_000;

// Despite the model itself running fully on-device, kokoro-js fetches each
// voice's style vector directly from HuggingFace at *generation* time (not
// during load()) — see its `generate_from_ids()` -> internal voice loader,
// which does its own cache-first `caches.open("kokoro-voices")` lookup and
// falls back to `fetch()` with no timeout. A slow or unreachable network at
// that point hangs generate() forever with no error, which is
// indistinguishable from GENERATION_TIMEOUT_MS eventually firing except
// that it means read-aloud never actually works on that connection. This
// app only ever uses the "af_heart" voice, so pre-seeding that exact cache
// entry from a same-origin copy — before load() resolves — removes the
// HuggingFace dependency entirely for the common case; kokoro-js's own
// lookup finds it and never touches the network. If a different voice is
// ever wired up, its file would need seeding too.
const VOICE_CACHE_NAME = "kokoro-voices";
const VOICE_CACHE_URL = "https://huggingface.co/onnx-community/Kokoro-82M-v1.0-ONNX/resolve/main/voices/af_heart.bin";
const LOCAL_VOICE_URL = "/tts-voices/af_heart.bin";

async function seedVoiceCache(): Promise<void> {
  if (!("caches" in globalThis)) return;
  try {
    const cache = await caches.open(VOICE_CACHE_NAME);
    if (await cache.match(VOICE_CACHE_URL)) return; // already seeded or already fetched for real
    const res = await fetch(LOCAL_VOICE_URL);
    if (res.ok) await cache.put(VOICE_CACHE_URL, res);
  } catch {
    // Best-effort: Cache Storage can be unavailable (e.g. private browsing)
    // or the local fetch can fail — kokoro-js's own fetch-from-HuggingFace
    // fallback still runs in that case, same as before this existed.
  }
}

// Minimal valid 1-sample 8-bit PCM WAV (44-byte header + 1 silent byte).
// unlock() needs a *real* source: an <audio> with no src rejects play()
// immediately on iOS Safari ("no supported source") without ever entering a
// playing state, so it doesn't count as a genuine gesture-backed play and
// grants nothing for the later async .play() call.
const SILENT_WAV_URL = (() => {
  const bytes = new Uint8Array([
    0x52, 0x49, 0x46, 0x46, 0x25, 0x00, 0x00, 0x00, 0x57, 0x41, 0x56, 0x45, 0x66, 0x6d, 0x74,
    0x20, 0x10, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x40, 0x1f, 0x00, 0x00, 0x40, 0x1f,
    0x00, 0x00, 0x01, 0x00, 0x08, 0x00, 0x64, 0x61, 0x74, 0x61, 0x01, 0x00, 0x00, 0x00, 0x80,
  ]);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return `data:audio/wav;base64,${btoa(binary)}`;
})();

export class KokoroSpeaker {
  private ttsPromise: Promise<KokoroTTS> | null = null;
  private queue: Promise<void> = Promise.resolve();
  private audioEl: HTMLAudioElement | null = null;
  voice: KokoroVoice = "af_heart";

  get loaded() {
    return this.ttsPromise !== null;
  }

  /**
   * Call synchronously from inside a click handler, before any `await` —
   * WebKit only grants an <audio> element playback permission while it's
   * still on the call stack of a real user gesture. Model loading and
   * speech generation both take long enough that by the time synth() would
   * otherwise create a fresh Audio(), that window has closed: iOS Safari
   * silently drops the .play() call (no error, no sound) rather than
   * rejecting it loudly. Priming one element here, synchronously, and
   * reusing it later is the standard workaround — a later .play() on the
   * *same* element stays permitted even from async code.
   *
   * Also explicitly requests the "ambient" audio session type (Safari-only;
   * a no-op elsewhere) so read-aloud mixes with whatever the learner is
   * already playing (podcast, music) instead of pausing it — the other
   * option, "playback", ignores the hardware ring/silent switch but is
   * exclusive and would stop the learner's music, which is the opposite of
   * what's wanted here. The trade-off is that, like any ambient sound,
   * playback stays silent while the ring/silent switch is on.
   */
  unlock() {
    if (!this.audioEl) {
      const el = new Audio(SILENT_WAV_URL);
      el.play().catch(() => {});
      el.pause();
      this.audioEl = el;
    }
    const session = navigator.audioSession;
    if (session) session.type = "ambient";
  }

  private takeAudioEl(): HTMLAudioElement {
    if (!this.audioEl) this.audioEl = new Audio();
    return this.audioEl;
  }

  load(onProgress?: (p: number) => void): Promise<KokoroTTS> {
    if (!this.ttsPromise) {
      const webgpu = "gpu" in navigator;
      const opts = {
        dtype: webgpu ? "fp32" : "q8",
        device: webgpu ? "webgpu" : "wasm",
        progress_callback: (info: { progress?: number }) => {
          if (onProgress && info?.progress != null) onProgress(info.progress);
        },
      };
      // Cast: kokoro-js option types are looser than this typed subset.
      const model = KokoroTTS.from_pretrained(MODEL_ID, opts as never);
      // Runs alongside the (much larger) model download rather than
      // blocking it — resolving load() only once both are done means the
      // voice is already cache-seeded by the time speak() calls generate().
      this.ttsPromise = Promise.all([seedVoiceCache(), model]).then(([, tts]) => tts);
    }
    return this.ttsPromise;
  }

  /**
   * Speak text at the given speed (1 = native speed). This is passed straight
   * through to the model's own `speed` generation parameter rather than
   * resampling the finished waveform, so slow speech stays natural instead
   * of sounding stretched. Calls are queued so replies never overlap.
   *
   * The returned promise reflects this call's own outcome — it rejects if
   * synth() throws (e.g. playback blocked, generation failure) — so callers
   * can tell a failed read-aloud apart from a successful one instead of the
   * UI silently cycling through loading -> speaking -> idle with no sound.
   * A separate internal queue (always resolving) is what serializes calls,
   * so one failure doesn't wedge the ones queued after it.
   *
   * `onPlaybackStart`, if given, fires right as audio actually starts
   * playing — i.e. once generation (phonemize + tokenize + the ONNX forward
   * pass, which is most of this call's latency and has no progress signal
   * of its own) has finished. Callers with a "speaking" indicator should
   * flip to it only here, not for the whole call, otherwise it lies about
   * what's happening during the (often much longer) generation phase.
   */
  speak(text: string, speed = 1, onPlaybackStart?: () => void): Promise<void> {
    const result = this.queue.then(() => this.synth(text, speed, onPlaybackStart));
    this.queue = result.catch(() => {});
    return result;
  }

  private async synth(text: string, speed: number, onPlaybackStart?: () => void) {
    const tts = await this.load();
    const audio = await withTimeout(
      tts.generate(text, { voice: this.voice, speed }),
      GENERATION_TIMEOUT_MS,
      "TTS generation timed out",
    );
    const url = URL.createObjectURL(audio.toBlob());
    try {
      onPlaybackStart?.();
      await play(this.takeAudioEl(), url);
    } finally {
      URL.revokeObjectURL(url);
    }
  }
}

function play(el: HTMLAudioElement, url: string): Promise<void> {
  return new Promise((resolve, reject) => {
    el.onended = () => resolve();
    el.onerror = () => reject(new Error("audio playback failed"));
    el.src = url;
    void el.play().catch(reject);
  });
}

function withTimeout<T>(promise: Promise<T>, ms: number, message: string): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(message)), ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      },
    );
  });
}
