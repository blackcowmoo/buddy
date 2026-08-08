// The Audio Session API (https://github.com/w3c/audio-session) isn't in
// lib.dom.d.ts yet — Safari is currently the only implementer. Declared
// once here rather than per call site.
declare global {
  interface Navigator {
    audioSession?: { type: "auto" | "playback" | "transient" | "transient-solo" | "ambient" | "play-and-record" };
  }
}

// Requests the "ambient" audio session type (Safari-only; a no-op
// elsewhere) so TTS read-aloud mixes with whatever the learner is already
// playing (podcast, music) instead of pausing it — the alternative,
// "playback", ignores the hardware ring/silent switch but is exclusive and
// would stop the learner's music. Trade-off: like any other ambient sound,
// playback stays silent while the ring/silent switch is on. Call
// synchronously inside the click handler that also starts playback — see
// ArticleQuiz.tsx's/App.tsx's handleRead/playMessage.
export function requestAmbientAudioSession(): void {
  const session = navigator.audioSession;
  if (session) session.type = "ambient";
}
