import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { generate } = vi.hoisted(() => ({
  generate: vi.fn().mockResolvedValue({ toBlob: () => new Blob() }),
}));

vi.mock("kokoro-js", () => ({
  KokoroTTS: { from_pretrained: vi.fn().mockResolvedValue({ generate }) },
}));

import { KokoroSpeaker } from "./kokoro";

// jsdom (used by the page-level tests) doesn't implement
// HTMLMediaElement.play(), and those tests mock KokoroSpeaker away entirely
// — so the unlock/reuse behavior itself, the actual fix for the iOS Safari
// silent-playback bug, needs its own coverage against a hand-rolled Audio
// stub instead.
class FakeAudio {
  src = "";
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
  it("primes a single Audio element by playing then pausing it synchronously", () => {
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
