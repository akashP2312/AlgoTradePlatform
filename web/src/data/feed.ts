// The gateway WS client. It reference-counts subscriptions so panels can subscribe
// and unsubscribe freely, resubscribes everything after a reconnect, and writes each
// conflated update into the store.
//
// It only reads: the gateway accepts sub and unsub and nothing else, and nothing
// typed in the browser is ever published.

import { Store } from "./store";

// The gateway allows these prefixes only (internal/gateway/subject.go).
const PREFIXES = ["tick.", "analytics.", "sys.", "book."];
// Subjects per sub/unsub frame, well under the gateway's 64 KiB read limit.
const CHUNK = 500;

export type ConnState = "connecting" | "open" | "closed";

export interface Status {
  state: ConnState;
  flushMs: number;
  since: number; // browser ms the state began
  frames: number; // update frames received since load
  lastFrame: number; // browser ms of the last update frame
  reconnects: number;
  lastError: string;
}

export class Feed {
  readonly store = new Store();
  readonly status: Status = {
    state: "closed", flushMs: 0, since: Date.now(), frames: 0, lastFrame: 0, reconnects: 0, lastError: "",
  };
  private refs = new Map<string, number>();
  private ws: WebSocket | null = null;
  private url = "";
  private backoff = 500;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private stopped = false;
  // Server clock minus browser clock, from the ticker's 1s feed status (see serverNow).
  private skews: number[] = [];
  private skewMs = 0;

  connect(url: string): void {
    this.url = url;
    this.stopped = false;
    this.open();
  }

  close(): void {
    this.stopped = true;
    if (this.timer) clearTimeout(this.timer);
    this.ws?.close();
  }

  /** Subscribes to subjects or patterns; the returned function releases them. */
  sub(subjects: string[]): () => void {
    const bad = subjects.filter((s) => !PREFIXES.some((p) => s.startsWith(p)));
    if (bad.length) throw new Error(`subject not allowed: ${bad.join(", ")}`);
    const added: string[] = [];
    for (const s of subjects) {
      const n = this.refs.get(s) ?? 0;
      this.refs.set(s, n + 1);
      if (n === 0) added.push(s);
    }
    this.send("sub", added);
    let done = false;
    return () => {
      if (done) return;
      done = true;
      const removed: string[] = [];
      for (const s of subjects) {
        const n = (this.refs.get(s) ?? 1) - 1;
        if (n <= 0) {
          this.refs.delete(s);
          removed.push(s);
        } else this.refs.set(s, n);
      }
      this.send("unsub", removed);
    };
  }

  get subscriptions(): number {
    return this.refs.size;
  }

  /**
   * The server's clock in ms. Payload timestamps (health `at`, feed ages) come from
   * the services' clock, which in a dry run is deliberately shifted, so ages are
   * measured against this rather than Date.now(). The skew is the largest
   * (server at − arrival) over the last 30 feed frames: delivery only ever delays a
   * frame, so the largest sample is the one closest to the true offset.
   */
  serverNow(): number {
    return Date.now() + this.skewMs;
  }

  private open(): void {
    if (this.stopped) return;
    this.setState("connecting");
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.url);
    } catch (e) {
      this.status.lastError = String(e);
      this.retry();
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      this.backoff = 500;
      this.setState("open");
      this.send("sub", [...this.refs.keys()]);
    };
    ws.onmessage = (ev) => this.onFrame(ev.data);
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.setState("closed");
      this.retry();
    };
    ws.onerror = () => {
      this.status.lastError = "socket error";
    };
  }

  private retry(): void {
    if (this.stopped) return;
    this.status.reconnects++;
    this.timer = setTimeout(() => this.open(), this.backoff);
    this.backoff = Math.min(this.backoff * 2, 5000);
  }

  private setState(s: ConnState): void {
    this.status.state = s;
    this.status.since = Date.now();
    this.store.version++;
  }

  private send(op: "sub" | "unsub", subjects: string[]): void {
    const ws = this.ws;
    if (!subjects.length || !ws || ws.readyState !== WebSocket.OPEN) return;
    for (let i = 0; i < subjects.length; i += CHUNK) {
      ws.send(JSON.stringify({ op, s: subjects.slice(i, i + CHUNK) }));
    }
  }

  private onFrame(data: unknown): void {
    if (typeof data !== "string") return;
    let f: { hello?: { flush_ms: number }; u?: Record<string, unknown>; err?: string };
    try {
      f = JSON.parse(data);
    } catch {
      return;
    }
    const now = Date.now();
    if (f.hello) this.status.flushMs = f.hello.flush_ms;
    if (f.err) this.status.lastError = f.err;
    if (f.u) {
      this.status.frames++;
      this.status.lastFrame = now;
      for (const s in f.u) {
        const v = f.u[s];
        this.store.set(s, v, now);
        if (s === "sys.ticker.feed") this.noteSkew(v, now);
      }
    }
    this.store.version++;
  }

  private noteSkew(v: unknown, now: number): void {
    const at = (v as { at?: number } | null)?.at;
    if (typeof at !== "number") return;
    this.skews.push(at - now);
    if (this.skews.length > 30) this.skews.shift();
    this.skewMs = Math.max(...this.skews);
  }
}

export const feed = new Feed();

/** The gateway's /ws on the page's own origin (the dev server proxies it). */
export function wsURL(loc: Location = window.location): string {
  return `${loc.protocol === "https:" ? "wss:" : "ws:"}//${loc.host}/ws`;
}
