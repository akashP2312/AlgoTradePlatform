// Contract reference data from the gateway's /api/ref: lot and tick sizes and the
// option expiries per underlying, fetched once (retrying until Postgres answers),
// and the resolution of what a user typed (NIFTY, NIFTY FUT2, NIFTY 25000CE X2)
// into a contract.

import { useSyncExternalStore } from "react";
import { feed } from "./feed";
import { isOption, keyString, type Kind } from "./contract";

export interface Inst {
  c: string;
  u: string;
  k: Kind;
  x?: string;
  lot: number;
  tick: number;
}

export interface Chain {
  x: string;
  lot: number;
  tick: number;
  n: number;
  /** The ticker streams it: it has ticks and an IV surface. */
  live?: boolean;
}

export interface Ref {
  insts: Inst[];
  chains: Record<string, Chain[]>;
  today: string;
}

let ref: Ref | null = null;
let started = false;
const listeners = new Set<() => void>();

export function setRef(r: Ref | null): void {
  ref = r;
  listeners.forEach((f) => f());
}

async function load(): Promise<void> {
  for (let wait = 1000; ; wait = Math.min(wait * 2, 15000)) {
    try {
      const r = await fetch("/api/ref");
      if (r.ok) return setRef(await r.json());
    } catch {
      /* gateway not up yet */
    }
    await new Promise((res) => setTimeout(res, wait));
  }
}

export function getRef(): Ref | null {
  if (!started && typeof window !== "undefined") {
    started = true;
    void load();
  }
  return ref;
}

export function useRefData(): Ref | null {
  return useSyncExternalStore(
    (f) => (listeners.add(f), () => listeners.delete(f)),
    getRef,
  );
}

/** Decimals to show for a tick size: 0.05 → 2, 0.0025 → 4, 1 → 0; 2 when unknown. */
export function decimals(tick: number | undefined): number {
  if (!tick) return 2;
  const s = String(tick);
  const i = s.indexOf(".");
  return i < 0 ? 0 : s.length - i - 1;
}

/** The server's date in IST, yyyy-mm-dd. */
export function serverToday(): string {
  const d = new Date(feed.serverNow() + 5.5 * 3600_000).toISOString().slice(0, 10);
  return ref?.today && ref.today > d ? ref.today : d;
}

/**
 * The underlying's option expiries from today on: the streamed ones when the
 * gateway marks any (only those have ticks and a surface), else all listed.
 */
export function expiries(und: string, r: Ref | null = ref): string[] {
  const t = serverToday();
  const cs = (r?.chains[und] ?? []).filter((c) => c.x >= t);
  const live = cs.filter((c) => c.live);
  return (live.length ? live : cs).map((c) => c.x);
}

export function securities(r: Ref | null = ref): string[] {
  return [...new Set((r?.insts ?? []).map((i) => i.u))].sort();
}

export interface Resolved {
  c: string; // contract key
  und: string;
  kind: Kind;
  x: string;
  k: number;
  lot: number;
  tick: number;
  dp: number;
}

const DATE = /^(\d{4})-?(\d{2})-?(\d{2})$/;
const OPT = /^(\d+(?:\.\d+)?)(CE|PE)$/;

/** An expiry token: yyyy-mm-dd, yyyymmdd, or Xn (the nth from today, X1 = nearest). */
export function expiryArg(und: string, a: string, r: Ref | null = ref): string | null {
  const u = a.toUpperCase();
  const m = DATE.exec(u);
  if (m) return `${m[1]}-${m[2]}-${m[3]}`;
  const x = /^X(\d)$/.exec(u);
  if (x) return expiries(und, r)[Number(x[1]) - 1] ?? null;
  return null;
}

/**
 * The contract a security and its args name. With no args: the index if there is
 * one, else the nearest future, else the equity. FUT / FUT1 / FUT2 pick a future;
 * 25000CE (or 25000 CE) an option on the nearest expiry unless an expiry arg is given.
 */
export function resolve(sec: string, args: string[] = [], r: Ref | null = ref): Resolved | { error: string } {
  if (!r) return { error: "loading reference data…" };
  const mine = r.insts.filter((i) => i.u === sec);
  if (!mine.length && !r.chains[sec]) return { error: `unknown security ${sec}` };
  const futs = mine.filter((i) => i.k === "FUT").sort((a, b) => (a.x! < b.x! ? -1 : 1)).filter((f) => f.x! >= serverToday());
  const toks = args.map((a) => a.toUpperCase());
  for (let i = 0; i + 1 < toks.length; i++) {
    if (/^\d+(\.\d+)?$/.test(toks[i]) && (toks[i + 1] === "CE" || toks[i + 1] === "PE")) toks.splice(i, 2, toks[i] + toks[i + 1]);
  }
  let want: "DEF" | "FUT" | "OPT" = "DEF";
  let futN = 1;
  let x = "";
  let k = 0;
  let kind: Kind = "IDX";
  for (const t of toks) {
    const f = /^FUT(\d)?$/.exec(t);
    const o = OPT.exec(t);
    if (f) [want, futN] = ["FUT", Number(f[1] ?? 1)];
    else if (o) [want, k, kind] = ["OPT", Number(o[1]), o[2] as Kind];
    else if (t !== "IDX") {
      const e = expiryArg(sec, t, r);
      if (!e) return { error: `don't understand "${t}"` };
      x = e;
    }
  }
  const pick = (i: Inst): Resolved => ({ c: i.c, und: i.u, kind: i.k, x: i.x ?? "", k: 0, lot: i.lot, tick: i.tick, dp: decimals(i.tick) });
  if (want === "OPT") {
    const ch = r.chains[sec];
    if (!ch?.length) return { error: `${sec} has no options` };
    x ||= expiries(sec, r)[0] ?? "";
    const c = ch.find((c) => c.x === x);
    if (!c) return { error: `${sec} has no ${x || "live"} expiry` };
    const exch = futs[0]?.c.split("|")[0] ?? "NFO";
    const key = keyString({ exch, name: sec, x, k, kind });
    return { c: key, und: sec, kind, x, k, lot: c.lot, tick: c.tick, dp: decimals(c.tick) };
  }
  if (want === "FUT" || !mine.some((i) => i.k === "IDX" || i.k === "EQ")) {
    const f = x ? futs.find((f) => f.x === x) : futs[futN - 1];
    if (!f) return { error: `${sec} has no such future` };
    return pick(f);
  }
  // A stock is listed on both NSE and BSE; the ticker streams the NSE line.
  const base = mine.find((i) => i.k === "IDX")
    ?? mine.find((i) => i.k === "EQ" && i.c.startsWith("NSE|"))
    ?? mine.find((i) => i.k === "EQ")!;
  return pick(base);
}

/** The lot and tick of an index or equity's tradable proxy, for OI in lots etc. */
export function nearFuture(und: string, r: Ref | null = ref): Inst | undefined {
  const t = serverToday();
  return r?.insts.filter((i) => i.u === und && i.k === "FUT" && i.x! >= t).sort((a, b) => (a.x! < b.x! ? -1 : 1))[0];
}

export { isOption };
