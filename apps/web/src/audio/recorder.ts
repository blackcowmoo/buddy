// PCMRecorder captures mic audio as 16 kHz mono 16-bit PCM using an
// AudioWorklet. This is the simple, dependency-free "push to talk" path:
// start() on click, stop() returns the whole utterance.
//
// UPGRADE (hands-free / true realtime): swap this for Silero VAD via
// `@ricky0123/vad-web`, whose onSpeechEnd callback hands you a 16 kHz
// Float32Array per utterance. Feed that through floatTo16() and the rest of
// the app is unchanged.
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

    this.ctx = new AudioContext({ sampleRate: 16000 });
    await this.ctx.audioWorklet.addModule("/pcm-worklet.js");

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
    const total = this.chunks.reduce((n, c) => n + c.length, 0);
    const merged = new Float32Array(total);
    let off = 0;
    for (const c of this.chunks) {
      merged.set(c, off);
      off += c.length;
    }
    await this.cleanup();
    return floatTo16(merged);
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
