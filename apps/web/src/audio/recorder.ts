import type { NonRealTimeVAD } from "@ricky0123/vad-web";

// PCMRecorder captures mic audio as 16 kHz mono 16-bit PCM using an
// AudioWorklet. This is the simple, dependency-free "push to talk" path:
// start() on click, stop() returns the whole utterance, trimmed of
// leading/trailing/internal silence by trimSilence() below.
//
// UPGRADE (hands-free / true realtime): swap this for Silero VAD via
// `@ricky0123/vad-web`'s MicVAD, whose onSpeechEnd callback hands you a
// 16 kHz Float32Array per utterance. Feed that through floatTo16() and the
// rest of the app is unchanged.
const SAMPLE_RATE = 16000;

export class PCMRecorder {
  private ctx: AudioContext | null = null;
  private stream: MediaStream | null = null;
  private node: AudioWorkletNode | null = null;
  private chunks: Float32Array[] = [];
  private recording = false;

  get isRecording() {
    return this.recording;
  }

  async start(): Promise<void> {
    if (this.recording) return;

    this.ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
    // Relative (not "/pcm-worklet.js"): resolves against the current page
    // URL, so it still works when the app is mounted under a ROOT_PATH
    // prefix like "/pr/14" instead of "/".
    await this.ctx.audioWorklet.addModule("pcm-worklet.js");

    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: 1,
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });

    const src = this.ctx.createMediaStreamSource(this.stream);
    this.node = new AudioWorkletNode(this.ctx, "pcm-worklet");
    this.chunks = [];
    this.node.port.onmessage = (e: MessageEvent) => {
      this.chunks.push(e.data as Float32Array);
    };

    // Route through a muted gain so the worklet stays in the render graph
    // without echoing the mic to the speakers.
    const mute = this.ctx.createGain();
    mute.gain.value = 0;
    src.connect(this.node);
    this.node.connect(mute);
    mute.connect(this.ctx.destination);

    this.recording = true;
  }

  async stop(): Promise<Int16Array> {
    this.recording = false;
    const merged = concatFloat32(this.chunks);
    await this.cleanup();
    return floatTo16(await trimSilence(merged));
  }

  private async cleanup() {
    this.node?.disconnect();
    this.stream?.getTracks().forEach((t) => t.stop());
    if (this.ctx) await this.ctx.close();
    this.ctx = null;
    this.stream = null;
    this.node = null;
    this.chunks = [];
  }
}

export function floatTo16(f: Float32Array): Int16Array {
  const out = new Int16Array(f.length);
  for (let i = 0; i < f.length; i++) {
    const s = Math.max(-1, Math.min(1, f[i]));
    out[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
  }
  return out;
}

// Concatenate a list of Float32Array chunks into one contiguous buffer.
function concatFloat32(chunks: Float32Array[]): Float32Array {
  const out = new Float32Array(chunks.reduce((n, c) => n + c.length, 0));
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.length;
  }
  return out;
}

type VAD = Awaited<ReturnType<typeof NonRealTimeVAD.new>>;
let vadPromise: Promise<VAD> | null = null;

// Dynamic import: onnxruntime-web's runtime is multiple MB of JS, so pulling
// it in statically would bloat the app's main chunk for every visitor even
// though most never touch it. This way it's a separate chunk, fetched (and
// its ONNX model + wasm backend loaded, then cached here) only the first
// time a recording is actually stopped.
function getVAD(): Promise<VAD> {
  if (!vadPromise) {
    vadPromise = import("@ricky0123/vad-web").then(({ NonRealTimeVAD }) =>
      NonRealTimeVAD.new({
        // Relative, not "/silero_vad_legacy.onnx": resolves against the
        // current page URL like pcm-worklet.js does (see PCMRecorder.start),
        // so this still works when the app is mounted under a ROOT_PATH
        // prefix like "/pr/14" instead of "/".
        modelURL: "silero_vad_legacy.onnx",
        ortConfig: (ort) => {
          ort.env.wasm.wasmPaths = "./";
        },
      }),
    );
  }
  return vadPromise;
}

// Trims leading/trailing silence and long internal pauses out of a captured
// utterance with Silero VAD, so the payload sent to the server — and what
// Whisper has to transcribe — isn't padded with dead air that a push-to-talk
// button inevitably captures. Falls back to the untrimmed audio if VAD finds
// no speech at all or fails to load, so a flaky model fetch never silently
// drops a real utterance.
export async function trimSilence(audio: Float32Array): Promise<Float32Array> {
  if (audio.length === 0) return audio;

  let vad: VAD;
  try {
    vad = await getVAD();
  } catch (err) {
    console.error("vad: load failed, sending untrimmed audio:", err);
    vadPromise = null; // let the next utterance retry instead of failing forever
    return audio;
  }

  const segments: Float32Array[] = [];
  for await (const { audio: segment } of vad.run(audio, SAMPLE_RATE)) {
    segments.push(segment);
  }
  if (segments.length === 0) return audio;

  return concatFloat32(segments);
}
