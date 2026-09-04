import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
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

function connectedClient(onEvent: (e: ServerEvent) => void = () => {}, onStatus: (s: Status) => void = () => {}) {
  const client = new BuddyClient(onEvent, onStatus);
  client.connect();
  return client;
}

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
  vi.stubGlobal("location", { protocol: "https:", host: "buddy.example:8080", pathname: "/" });
});

afterEach(() => {
  // A test that simulates an unexpected close leaves a reconnect timer
  // scheduled; clear it so it can't fire during a later test and mutate
  // `lastSocket` out from under it.
  vi.useRealTimers();
  vi.clearAllTimers();
});

describe("BuddyClient", () => {
  it("connects to a wss URL derived from location when https", () => {
    connectedClient();
    expect(lastSocket.url).toBe("wss://buddy.example:8080/ws");
  });

  it("uses ws (not wss) when the page is served over http", () => {
    vi.stubGlobal("location", { protocol: "http:", host: "buddy.example:8080", pathname: "/" });
    connectedClient();
    expect(lastSocket.url).toBe("ws://buddy.example:8080/ws");
  });

  it("prefixes the WS URL with a ROOT_PATH mount point", () => {
    vi.stubGlobal("location", { protocol: "https:", host: "buddy.example:8080", pathname: "/pr/14/" });
    connectedClient();
    expect(lastSocket.url).toBe("wss://buddy.example:8080/pr/14/ws");
  });

  it("adds a trailing slash to pathname before appending ws", () => {
    vi.stubGlobal("location", { protocol: "https:", host: "buddy.example:8080", pathname: "/pr/14" });
    connectedClient();
    expect(lastSocket.url).toBe("wss://buddy.example:8080/pr/14/ws");
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
    client.close(); // cancel the reconnect this unexpected close scheduled
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

  it("queues sendText while disconnected and flushes once the socket opens", () => {
    const client = connectedClient();
    client.sendText("hello");
    expect(lastSocket.sent).toEqual([]);

    lastSocket.open();
    expect(lastSocket.sent).toEqual([JSON.stringify({ type: "text", text: "hello" })]);
  });

  it("sends immediately once already open", () => {
    const client = connectedClient();
    lastSocket.open();
    client.sendText("hello");
    expect(lastSocket.sent).toEqual([JSON.stringify({ type: "text", text: "hello" })]);
  });

  it("sendText tags a confirmed voice draft with source: voice", () => {
    const client = connectedClient();
    lastSocket.open();
    client.sendText("hello", "voice");
    expect(lastSocket.sent).toEqual([JSON.stringify({ type: "text", text: "hello", source: "voice" })]);
  });

  it("sendAudio writes the raw PCM buffer once open", () => {
    const client = connectedClient();
    lastSocket.open();
    const pcm = new Int16Array([1, 2, 3]);
    client.sendAudio(pcm);
    expect(Array.from(new Int16Array(lastSocket.sent[0] as ArrayBuffer))).toEqual([1, 2, 3]);
  });

  it("queues audio recorded during a reconnect instead of silently dropping it", () => {
    const client = connectedClient();
    const pcm = new Int16Array([4, 5, 6]);

    client.sendAudio(pcm);
    expect(lastSocket.sent).toEqual([]);

    lastSocket.open();
    expect(Array.from(new Int16Array(lastSocket.sent[0] as ArrayBuffer))).toEqual([4, 5, 6]);
  });

  it("sends only the samples in an Int16Array view, not its whole backing buffer", () => {
    const client = connectedClient();
    lastSocket.open();
    const backing = new Int16Array([99, 7, 8, 99]);

    client.sendAudio(backing.subarray(1, 3));

    expect(Array.from(new Int16Array(lastSocket.sent[0] as ArrayBuffer))).toEqual([7, 8]);
  });

  it("close() tears down the socket", () => {
    const client = connectedClient();
    lastSocket.open();
    client.close();
    expect(lastSocket.readyState).toBe(MockWebSocket.CLOSED);
  });

  it("automatically reconnects to the same session after an unexpected close", () => {
    vi.useFakeTimers();
    const client = connectedClient();
    lastSocket.open();
    lastSocket.onmessage?.({ data: JSON.stringify({ type: "ready", turn: 0, session: "room-42" }) });
    const firstSocket = lastSocket;

    firstSocket.close(); // e.g. the mobile OS backgrounded the tab and killed the TCP connection
    expect(lastSocket).toBe(firstSocket); // no new socket yet — reconnect is scheduled, not immediate

    vi.advanceTimersByTime(1000);
    expect(lastSocket).not.toBe(firstSocket);
    expect(lastSocket.url).toBe("wss://buddy.example:8080/ws?session=room-42");

    client.close();
    vi.useRealTimers();
  });

  it("retries a message sent while disconnected once the reconnect completes", () => {
    vi.useFakeTimers();
    const client = connectedClient();
    lastSocket.open();
    lastSocket.close(); // unexpected drop

    client.sendText("are you still there?");
    expect(lastSocket.sent).toEqual([]); // queued — the reconnect hasn't opened yet

    vi.advanceTimersByTime(1000);
    lastSocket.open();
    expect(lastSocket.sent).toEqual([JSON.stringify({ type: "text", text: "are you still there?" })]);

    client.close();
    vi.useRealTimers();
  });

  it("does not reconnect after an explicit close()", () => {
    vi.useFakeTimers();
    const client = connectedClient();
    lastSocket.open();
    const firstSocket = lastSocket;

    client.close(); // e.g. the learner navigated back to the room list
    vi.advanceTimersByTime(20000);
    expect(lastSocket).toBe(firstSocket); // still no new socket

    vi.useRealTimers();
  });

  it("resets reconnect backoff after a successful reconnect", () => {
    vi.useFakeTimers();
    const client = connectedClient();
    lastSocket.open();

    lastSocket.close();
    vi.advanceTimersByTime(1000); // first backoff: 1s
    lastSocket.open();

    lastSocket.close();
    vi.advanceTimersByTime(999);
    expect(lastSocket.readyState).toBe(MockWebSocket.CLOSED); // not yet — backoff restarted at 1s, not 2s

    vi.advanceTimersByTime(1);
    expect(lastSocket.readyState).toBe(MockWebSocket.CONNECTING);

    client.close();
    vi.useRealTimers();
  });
});
