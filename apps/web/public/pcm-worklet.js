// AudioWorklet that forwards mono Float32 mic frames to the main thread.
// The AudioContext runs at 16 kHz, so no resampling is needed here — the main
// thread just converts these to 16-bit PCM before sending to the server.
class PCMWorklet extends AudioWorkletProcessor {
  process(inputs) {
    const input = inputs[0];
    if (input && input[0]) {
      // Copy: the underlying buffer is reused across render quanta.
      this.port.postMessage(input[0].slice(0));
    }
    return true; // keep the processor alive
  }
}

registerProcessor("pcm-worklet", PCMWorklet);
