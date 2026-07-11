import type { ClientMsg, ServerEvent } from "./protocol";

export type Status = "connecting" | "open" | "closed" | "error";

// BuddyClient wraps the single WebSocket to the Go server. Audio goes up as
// binary frames (one utterance each); everything else is JSON.
export class BuddyClient {
  private ws: WebSocket | null = null;

  constructor(
    private onEvent: (e: ServerEvent) => void,
    private onStatus: (s: Status) => void,
  ) {}

  connect() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // Derive the base path from the current page instead of hardcoding "/ws",
    // so this works whether the app is mounted at "/" or under a ROOT_PATH
    // prefix like "/pr/14" (see httpserver.withRootPath server-side).
    const base = location.pathname.endsWith("/") ? location.pathname : `${location.pathname}/`;
    const ws = new WebSocket(`${proto}://${location.host}${base}ws`);
    ws.binaryType = "arraybuffer";
    this.onStatus("connecting");

    ws.onopen = () => this.onStatus("open");
    ws.onclose = () => this.onStatus("closed");
    ws.onerror = () => this.onStatus("error");
    ws.onmessage = (ev) => {
      try {
        this.onEvent(JSON.parse(ev.data as string) as ServerEvent);
      } catch (err) {
        console.error("bad server event", err);
      }
    };
    this.ws = ws;
  }

  /** Send one complete utterance (16 kHz mono s16le PCM). */
  sendAudio(pcm: Int16Array) {
    if (this.ws?.readyState === WebSocket.OPEN)
      this.ws.send(pcm.buffer as ArrayBuffer);
  }

  sendText(text: string) {
    this.send({ type: "text", text });
  }

  reset() {
    this.send({ type: "reset" });
  }

  close() {
    this.ws?.close();
    this.ws = null;
  }

  private send(m: ClientMsg) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
  }
}
