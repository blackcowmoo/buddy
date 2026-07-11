import { KokoroTTS } from "kokoro-js";

// kokoro-82M runs fully in the browser (WebGPU, WASM fallback). The model is
// ~80–300 MB depending on dtype, so it is lazy-loaded on first use.
const MODEL_ID = "onnx-community/Kokoro-82M-v1.0-ONNX";

// A few known-good voices. See the kokoro-js model card for the full list
// (af_* American female, am_* American male, bf_*/bm_* British, etc.).
export type KokoroVoice = "af_heart" | "af_bella" | "af_sarah" | "am_adam" | "am_echo";

export class KokoroSpeaker {
  private ttsPromise: Promise<KokoroTTS> | null = null;
  private queue: Promise<void> = Promise.resolve();
  voice: KokoroVoice = "af_heart";

  get loaded() {
    return this.ttsPromise !== null;
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
      this.ttsPromise = KokoroTTS.from_pretrained(MODEL_ID, opts as never);
    }
    return this.ttsPromise;
  }

  /**
   * Speak text at the given speed (1 = native speed). This is passed straight
   * through to the model's own `speed` generation parameter rather than
   * resampling the finished waveform, so slow speech stays natural instead
   * of sounding stretched. Calls are queued so replies never overlap.
   */
  speak(text: string, speed = 1): Promise<void> {
    this.queue = this.queue
      .then(() => this.synth(text, speed))
      .catch((err) => console.error("tts:", err));
    return this.queue;
  }

  private async synth(text: string, speed: number) {
    const tts = await this.load();
    const audio = await tts.generate(text, { voice: this.voice, speed });
    const url = URL.createObjectURL(audio.toBlob());
    try {
      await play(url);
    } finally {
      URL.revokeObjectURL(url);
    }
  }
}

function play(url: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const el = new Audio(url);
    el.onended = () => resolve();
    el.onerror = () => reject(new Error("audio playback failed"));
    void el.play().catch(reject);
  });
}
