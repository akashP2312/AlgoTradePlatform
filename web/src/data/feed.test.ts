import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Feed } from "./feed";

class FakeWS {
  static OPEN = 1;
  static last: FakeWS;
  readyState = 0;
  sent: { op: string; s: string[] }[] = [];
  onopen?: () => void;
  onmessage?: (e: { data: string }) => void;
  onclose?: () => void;
  onerror?: () => void;
  constructor(public url: string) {
    FakeWS.last = this;
  }
  send(d: string) {
    this.sent.push(JSON.parse(d));
  }
  close() {
    this.readyState = 3;
    this.onclose?.();
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", FakeWS);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("Feed", () => {
  it("reference-counts subscriptions", () => {
    const f = new Feed();
    f.connect("ws://x/ws");
    const ws = FakeWS.last;
    ws.open();
    const a = f.sub(["sys.>", "tick.nse.nifty.>"]);
    const b = f.sub(["sys.>"]);
    expect(ws.sent).toEqual([{ op: "sub", s: ["sys.>", "tick.nse.nifty.>"] }]);
    a();
    a(); // idempotent
    expect(ws.sent[1]).toEqual({ op: "unsub", s: ["tick.nse.nifty.>"] });
    b();
    expect(ws.sent[2]).toEqual({ op: "unsub", s: ["sys.>"] });
    expect(f.subscriptions).toBe(0);
  });

  it("refuses subjects the gateway would refuse", () => {
    expect(() => new Feed().sub(["order.update"])).toThrow(/not allowed/);
  });

  it("resubscribes after a reconnect and stores updates", () => {
    const f = new Feed();
    f.connect("ws://x/ws");
    f.sub(["sys.>"]);
    FakeWS.last.open();
    FakeWS.last.close();
    vi.advanceTimersByTime(500);
    const ws = FakeWS.last;
    ws.open();
    expect(ws.sent).toEqual([{ op: "sub", s: ["sys.>"] }]);
    ws.onmessage?.({ data: JSON.stringify({ t: 1, u: { "sys.ticker.health": { ok: true } } }) });
    expect(f.store.get("sys.ticker.health")).toEqual({ ok: true });
    expect(f.status.reconnects).toBe(1);
  });

  it("measures server skew from feed status", () => {
    const f = new Feed();
    f.connect("ws://x/ws");
    FakeWS.last.open();
    const at = Date.now() + 3_600_000;
    FakeWS.last.onmessage?.({ data: JSON.stringify({ u: { "sys.ticker.feed": { at } } }) });
    expect(Math.abs(f.serverNow() - at)).toBeLessThan(50);
  });
});
