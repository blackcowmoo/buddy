import type { ClientMsg, ServerEvent } from "./protocol";

export type Status = "connecting" | "open" | "closed" | "error";

// Reconnect backoff: doubles each attempt, capped so a long outage still
// retries roughly once every 10s instead of falling further and further
// behind.
const initialReconnectDelayMs = 1000;
const maxReconnectDelayMs = 10000;

// BuddyClient wraps the single WebSocket to the Go server. Audio goes up as
// binary frames (one utterance each); everything else is JSON.
//
// Mobile backgrounding (switching apps, locking the screen, then coming
// back) reliably kills the underlying TCP connection without unmounting the
// page, so `ws.onclose` fires on its own with no user action. Without
// reconnect logic that just strands the learner: the socket is dead, but
// nothing tells them to reload, and anything they type after that silently
// never arrives (WebSocket.send is a no-op once the socket isn't OPEN).
// BuddyClient instead auto-reconnects to the same chat room (see
// `sessionID`, captured off the server's "ready" event so a reconnect after
// a brand-new room resumes it instead of minting another one) and queues
// any ClientMsg sent while disconnected, flushing the queue the moment the
// new socket opens — so a message typed mid-outage still goes out once the
// connection comes back, instead of vanishing.
export class BuddyClient {
  private ws: WebSocket | null = null;
  private sessionID: string | undefined;
  // True only once the caller calls close() — distinguishes "the learner
  // left this room" (stop reconnecting) from "the socket dropped on its
  // own" (reconnect), since both fire the same ws.onclose.
  private closedByCaller = false;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectDelayMs = initialReconnectDelayMs;
  // Messages sent while the socket isn't OPEN (mid-connect or mid-reconnect)
  // — flushed in order once a new socket opens.
  private pending: ClientMsg[] = [];

  constructor(
    private onEvent: (e: ServerEvent) => void,
    private onStatus: (s: Status) => void,
  ) {}

  /**
   * Opens the connection. Passing sessionID resumes that chat room (server
   * seeds its long-term memory and the caller is expected to have already
   * hydrated the visible transcript via fetchSessionDetail); omitting it
   * always starts a brand-new room — the server mints an ID and returns it
   * on the "ready" event.
   */
  connect(sessionID?: string) {
    this.closedByCaller = false;
    this.reconnectDelayMs = initialReconnectDelayMs;
    this.sessionID = sessionID;
    this.pending = [];
    this.open();
  }

  private open() {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // Derive the base path from the current page instead of hardcoding "/ws",
    // so this works whether the app is mounted at "/" or under a ROOT_PATH
    // prefix like "/pr/14" (see httpserver.withRootPath server-side).
    const base = location.pathname.endsWith("/") ? location.pathname : `${location.pathname}/`;
    const qs = this.sessionID ? `?session=${encodeURIComponent(this.sessionID)}` : "";
    const ws = new WebSocket(`${proto}://${location.host}${base}ws${qs}`);
    ws.binaryType = "arraybuffer";
    this.onStatus("connecting");

    ws.onopen = () => {
      this.reconnectDelayMs = initialReconnectDelayMs;
      this.onStatus("open");
      this.flushPending();
    };
    ws.onclose = () => {
      this.onStatus("closed");
      this.scheduleReconnect();
    };
    ws.onerror = () => this.onStatus("error");
    ws.onmessage = (ev) => {
      try {
        const parsed = JSON.parse(ev.data as string) as ServerEvent;
        // Remember the server-assigned ID so a later reconnect resumes this
        // room instead of minting a brand-new one — matters most right
        // after starting a new chat, before the caller has any ID to pass.
        if (parsed.type === "ready" && parsed.session) this.sessionID = parsed.session;
        this.onEvent(parsed);
      } catch (err) {
        console.error("bad server event", err);
      }
    };
    this.ws = ws;
  }

  private scheduleReconnect() {
    if (this.closedByCaller) return;
    this.reconnectTimer = setTimeout(() => this.open(), this.reconnectDelayMs);
    this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, maxReconnectDelayMs);
  }

  private flushPending() {
    const queued = this.pending;
    this.pending = [];
    for (const m of queued) this.rawSend(m);
  }

  /** Send one complete utterance (16 kHz mono s16le PCM). */
  sendAudio(pcm: Int16Array) {
    if (this.ws?.readyState === WebSocket.OPEN)
      this.ws.send(pcm.buffer as ArrayBuffer);
  }

  sendText(text: string) {
    this.send({ type: "text", text });
  }

  close() {
    this.closedByCaller = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.pending = [];
    this.ws?.close();
    this.ws = null;
  }

  // Queues m if the socket isn't open yet rather than dropping it — see the
  // class doc for why a disconnect must never silently swallow what the
  // learner just typed.
  private send(m: ClientMsg) {
    if (this.ws?.readyState === WebSocket.OPEN) this.rawSend(m);
    else this.pending.push(m);
  }

  private rawSend(m: ClientMsg) {
    this.ws?.send(JSON.stringify(m));
  }
}
