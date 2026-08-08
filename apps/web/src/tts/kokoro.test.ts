import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { generate } = vi.hoisted(() => ({
  generate: vi.fn().mockResolvedValue({ toBlob: () => new Blob() }),
}));

vi.mock("kokoro-js", () => ({
  KokoroTTS: { from_pretrained: vi.fn().mockResolvedValue({ generate }) },
}));

import { KokoroTTS } from "kokoro-js";
import { GENERATION_TIMEOUT_MS, KokoroSpeaker } from "./kokoro";

// jsdom (used by the page-level tests) doesn't implement
// HTMLMediaElement.play(), and those tests mock KokoroSpeaker away entirely
// — so the unlock/reuse behavior itself, the actual fix for the iOS Safari
// silent-playback bug, needs its own coverage against a hand-rolled Audio
// stub instead.
class FakeAudio {
  src = "";
  loop = false;
  onended: (() => void) | null = null;
  onerror: (() => void) | null = null;
  play = vi.fn().mockResolvedValue(undefined);
  pause = vi.fn();
}

let audioInstances: FakeAudio[] = [];

beforeEach(() => {
  audioInstances = [];
  vi.stubGlobal(
    "Audio",
    vi.fn().mockImplementation(function (this: FakeAudio, src?: string) {
      const el = Object.assign(this, new FakeAudio());
      if (src) el.src = src;
      audioInstances.push(el);
      return el;
    }),
  );
  vi.stubGlobal("URL", {
    createObjectURL: vi.fn().mockReturnValue("blob:mock-url"),
    revokeObjectURL: vi.fn(),
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("KokoroSpeaker.unlock", () => {
  it("primes the reusable playback element by playing then pausing it synchronously", () => {
    const speaker = new KokoroSpeaker();
    speaker.unlock();

    expect(audioInstances).toHaveLength(1);
    expect(audioInstances[0].play).toHaveBeenCalled();
    expect(audioInstances[0].pause).toHaveBeenCalled();
  });

  it("primes the element with a real (silent) source, not an empty one", () => {
    // Regression guard: an <audio> with no src rejects play() immediately on
    // iOS Safari without ever entering a playing state, so it doesn't count
    // as a genuine gesture-backed play and the unlock is a no-op.
    const speaker = new KokoroSpeaker();
    speaker.unlock();

    expect(audioInstances[0].src).toMatch(/^data:audio\/wav;base64,/);
  });

  it("is a no-op on a second call, so the same primed element keeps being reused", () => {
    const speaker = new KokoroSpeaker();
    speaker.unlock();
    speaker.unlock();

    expect(audioInstances).toHaveLength(1);
  });

  it("requests the ambient audio session type so read-aloud mixes with other apps' audio", () => {
    // Regression guard: the alternative, "playback", ignores the hardware
    // ring/silent switch but is exclusive — it pauses whatever the learner
    // is already playing (music, a podcast), which isn't wanted here.
    const audioSession = { type: "auto" };
    vi.stubGlobal("navigator", { ...navigator, audioSession });

    const speaker = new KokoroSpeaker();
    speaker.unlock();

    expect(audioSession.type).toBe("ambient");
  });

  it("does nothing when the Audio Session API isn't available (non-Safari browsers)", () => {
    const speaker = new KokoroSpeaker();
    expect(() => speaker.unlock()).not.toThrow();
  });
});

describe("KokoroSpeaker.load", () => {
  const VOICE_CACHE_URL =
    "https://huggingface.co/onnx-community/Kokoro-82M-v1.0-ONNX/resolve/main/voices/af_heart.bin";

  it("seeds the voice cache from a same-origin copy instead of leaving kokoro-js to fetch it from HuggingFace", async () => {
    // Regression guard: kokoro-js fetches the voice's style vector from
    // HuggingFace at generation time, with no timeout — a slow/unreachable
    // network there hangs generate() forever with no error. Pre-seeding its
    // cache lookup from our own bundled copy avoids that network dependency
    // for the one voice this app actually uses.
    const put = vi.fn().mockResolvedValue(undefined);
    const match = vi.fn().mockResolvedValue(undefined); // nothing cached yet
    const open = vi.fn().mockResolvedValue({ match, put });
    vi.stubGlobal("caches", { open });
    const fetchMock = vi.fn().mockResolvedValue({ ok: true });
    vi.stubGlobal("fetch", fetchMock);

    const speaker = new KokoroSpeaker();
    await speaker.load();

    expect(open).toHaveBeenCalledWith("kokoro-voices");
    expect(fetchMock).toHaveBeenCalledWith("/tts-voices/af_heart.bin");
    expect(put).toHaveBeenCalledWith(VOICE_CACHE_URL, expect.anything());
  });

  it("skips re-fetching when the voice is already cached", async () => {
    const put = vi.fn();
    const match = vi.fn().mockResolvedValue({}); // already cached
    const open = vi.fn().mockResolvedValue({ match, put });
    vi.stubGlobal("caches", { open });
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const speaker = new KokoroSpeaker();
    await speaker.load();

    expect(fetchMock).not.toHaveBeenCalled();
    expect(put).not.toHaveBeenCalled();
  });

  it("still resolves when Cache Storage isn't available (e.g. private browsing)", async () => {
    vi.stubGlobal("caches", undefined);

    const speaker = new KokoroSpeaker();

    await expect(speaker.load()).resolves.toBeDefined();
  });

  it("still resolves when seeding itself throws", async () => {
    vi.stubGlobal("caches", { open: vi.fn().mockRejectedValue(new Error("blocked")) });

    const speaker = new KokoroSpeaker();

    await expect(speaker.load()).resolves.toBeDefined();
  });

  it("forces the WASM device on mobile even when the WebGPU API is present", async () => {
    // Regression guard: `"gpu" in navigator` only means the API exists, not
    // that it works — mobile WebGPU is documented as unreliable enough that
    // other kokoro-js deployments disable it on mobile outright, and this
    // app has no fallback if a WebGPU run stalls mid-generation.
    vi.stubGlobal("navigator", {
      ...navigator,
      gpu: {},
      userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15",
    });

    const speaker = new KokoroSpeaker();
    await speaker.load();

    expect(vi.mocked(KokoroTTS.from_pretrained)).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({ device: "wasm", dtype: "q8" }),
    );
  });

  it("uses WebGPU on desktop when the API is present", async () => {
    vi.stubGlobal("navigator", {
      ...navigator,
      gpu: {},
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15",
    });

    const speaker = new KokoroSpeaker();
    await speaker.load();

    expect(vi.mocked(KokoroTTS.from_pretrained)).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({ device: "webgpu", dtype: "fp32" }),
    );
  });
});

describe("KokoroSpeaker.speak", () => {
  it("plays through the element unlock() already primed, instead of creating a fresh one", async () => {
    const speaker = new KokoroSpeaker();
    speaker.unlock();
    const primed = audioInstances[0];

    const done = speaker.speak("hello");
    await vi.waitFor(() => expect(primed.src).toBe("blob:mock-url"));
    primed.onended?.();
    await done;

    // Regression guard: a fresh, un-primed Audio() created after the async
    // model load/generate would silently fail to play on iOS Safari (see
    // KokoroSpeaker.unlock's doc comment) — reusing the gesture-primed
    // element is the actual fix.
    expect(audioInstances).toHaveLength(1);
    expect(primed.play).toHaveBeenCalledTimes(2); // once to unlock, once to actually play
  });

  it("calls onPlaybackStart only once generation has finished and audio is about to play", async () => {
    // Regression guard: generation (phonemize + tokenize + the ONNX forward
    // pass) is most of speak()'s latency and has no progress signal of its
    // own — a caller that flips a "speaking" indicator for the whole call
    // lies about what's happening while generation is still running.
    let resolveGenerate: (v: { toBlob: () => Blob }) => void = () => {};
    generate.mockReturnValueOnce(new Promise((resolve) => (resolveGenerate = resolve)));
    const onPlaybackStart = vi.fn();
    const speaker = new KokoroSpeaker();

    const done = speaker.speak("hello", 1, onPlaybackStart);
    await vi.waitFor(() => expect(generate).toHaveBeenCalled());
    expect(onPlaybackStart).not.toHaveBeenCalled();
    expect(audioInstances).toHaveLength(0);

    resolveGenerate({ toBlob: () => new Blob() });
    await vi.waitFor(() => expect(audioInstances).toHaveLength(1));
    expect(onPlaybackStart).toHaveBeenCalledTimes(1);

    audioInstances[0].onended?.();
    await done;
  });

  it("falls back to creating its own element when unlock() was never called", async () => {
    const speaker = new KokoroSpeaker();

    const done = speaker.speak("hello");
    await vi.waitFor(() => expect(audioInstances).toHaveLength(1));
    audioInstances[0].onended?.();
    await done;

    expect(audioInstances[0].src).toBe("blob:mock-url");
  });

  it("rejects the caller's promise when playback fails, instead of silently resolving", async () => {
    // Regression guard: speak() used to catch synth() failures internally
    // and resolve the returned promise regardless, so a caller had no way
    // to tell a failed read-aloud (blocked play(), generation error, ...)
    // apart from a successful one — the UI just showed "playing" then went
    // back to idle with no sound and no error.
    const speaker = new KokoroSpeaker();

    const done = speaker.speak("hello");
    await vi.waitFor(() => expect(audioInstances).toHaveLength(1));
    audioInstances[0].onerror?.();

    await expect(done).rejects.toThrow("audio playback failed");
  });

  it("rejects with a timeout instead of hanging forever when generation never resolves", async () => {
    // Regression guard: generate() has no internal timeout and, on some
    // mobile WASM setups, has been observed to never resolve at all — the
    // UI used to get stuck on "재생 중…" forever with no error and no way
    // to retry short of reloading the page.
    vi.useFakeTimers();
    generate.mockReturnValueOnce(new Promise(() => {})); // never resolves

    const speaker = new KokoroSpeaker();
    const done = speaker.speak("hello");
    const assertion = expect(done).rejects.toThrow("TTS generation timed out");

    await vi.advanceTimersByTimeAsync(GENERATION_TIMEOUT_MS);
    await assertion;

    vi.useRealTimers();
  });

  it("keeps processing later calls after an earlier one fails", async () => {
    const speaker = new KokoroSpeaker();

    const first = speaker.speak("hello").catch(() => {});
    await vi.waitFor(() => expect(audioInstances).toHaveLength(1));
    audioInstances[0].onerror?.();
    await first;

    // The element is reused across calls, so its `src` is already set from
    // the first (failed) call by this point — wait for the second call's
    // own play() invocation rather than for `src`, otherwise firing
    // onended below can race and resolve against the stale first call.
    const playCallsBeforeSecond = audioInstances[0].play.mock.calls.length;
    const second = speaker.speak("world");
    await vi.waitFor(() =>
      expect(audioInstances[0].play.mock.calls.length).toBeGreaterThan(playCallsBeforeSecond),
    );
    audioInstances[0].onended?.();
    await expect(second).resolves.toBeUndefined();
  });
});
