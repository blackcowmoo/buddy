import { beforeEach, describe, expect, it, vi } from "vitest";
import { BuddyClient, type Status } from "./ws";
import type { ServerEvent } from "./protocol";

class MockWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  readyState = MockWebSocket.CONNECTING;
  binaryType = "";
  sent: unknown[] = [];
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;

  constructor(readonly url: string) {}

  send(data: unknown) {
    this.sent.push(data);
  }

  close() {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.();
  }

  open() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }
}

let lastSocket: MockWebSocket;

beforeEach(() => {
  vi.stubGlobal(
    "WebSocket",
    class extends MockWebSocket {
      constructor(url: string) {
        super(url);
        lastSocket = this;
      }
    },
  );
  vi.stubGlobal("location", { protocol: "https:", host: "buddy.example:8080" });
});

describe("BuddyClient", () => {
  it("connects to a wss URL derived from location when https", () => {
    const client = new BuddyClient(
      () => {},
      () => {},
    );
    client.connect();
    expect(lastSocket.url).toBe("wss://buddy.example:8080/ws");
  });

  it("uses ws (not wss) when the page is served over http", () => {
    vi.stubGlobal("location", { protocol: "http:", host: "buddy.example:8080" });
    const client = new BuddyClient(
      () => {},
      () => {},
    );
    client.connect();
    expect(lastSocket.url).toBe("ws://buddy.example:8080/ws");
  });

  it("reports status transitions", () => {
    const statuses: Status[] = [];
    const client = new BuddyClient(
      () => {},
      (s) => statuses.push(s),
    );
    client.connect();
    expect(statuses).toEqual(["connecting"]);
    lastSocket.open();
    expect(statuses).toEqual(["connecting", "open"]);
    lastSocket.close();
    expect(statuses).toEqual(["connecting", "open", "closed"]);
  });

  it("parses incoming JSON messages into ServerEvent", () => {
    const events: ServerEvent[] = [];
    const client = new BuddyClient(
      (e) => events.push(e),
      () => {},
    );
    client.connect();
    lastSocket.onmessage?.({ data: JSON.stringify({ type: "ready", turn: 1 }) });
    expect(events).toEqual([{ type: "ready", turn: 1 }]);
  });

  it("swallows malformed JSON instead of throwing", () => {
    const events: ServerEvent[] = [];
    const client = new BuddyClient(
      (e) => events.push(e),
      () => {},
    );
    client.connect();
    expect(() => lastSocket.onmessage?.({ data: "not json" })).not.toThrow();
    expect(events).toEqual([]);
  });

  it("sendText only writes to the socket once it is open", () => {
    const client = new BuddyClient(
      () => {},
      () => {},
    );
    client.connect();
    client.sendText("hello");
    expect(lastSocket.sent).toEqual([]);

    lastSocket.open();
    client.sendText("hello");
    expect(lastSocket.sent).toEqual([JSON.stringify({ type: "text", text: "hello" })]);
  });

  it("sendAudio writes the raw PCM buffer once open", () => {
    const client = new BuddyClient(
      () => {},
      () => {},
    );
    client.connect();
    lastSocket.open();
    const pcm = new Int16Array([1, 2, 3]);
    client.sendAudio(pcm);
    expect(lastSocket.sent).toEqual([pcm.buffer]);
  });

  it("reset sends a reset message", () => {
    const client = new BuddyClient(
      () => {},
      () => {},
    );
    client.connect();
    lastSocket.open();
    client.reset();
    expect(lastSocket.sent).toEqual([JSON.stringify({ type: "reset" })]);
  });

  it("close() tears down the socket", () => {
    const client = new BuddyClient(
      () => {},
      () => {},
    );
    client.connect();
    lastSocket.open();
    client.close();
    expect(lastSocket.readyState).toBe(MockWebSocket.CLOSED);
  });
});
