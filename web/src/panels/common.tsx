// Pieces the market screens share: resolving a panel's security, the analytics
// subjects, ages against the server clock, and signed changes.

import { undTok } from "../data/contract";
import { feed } from "../data/feed";
import { resolve, useRefData, type Resolved } from "../data/ref";
import { num } from "../fmt";

/** Ticks older than this are drawn grey: the fakekite and live feeds tick every second or so. */
export const STALE_MS = 10_000;

export function useResolved(sec: string | undefined, args: string[] | undefined): Resolved | { error: string } {
  const r = useRefData();
  if (!sec) return { error: "no security" };
  return resolve(sec, args ?? [], r);
}

/** yyyy-mm-dd → yyyymmdd, as subjects carry expiries. */
export const compact = (x: string) => x.replaceAll("-", "");

export const ivSubject = (und: string, x: string) => `analytics.${undTok(und)}.iv.${compact(x)}`;
export const termSubject = (und: string) => `analytics.${undTok(und)}.term`;

/** ms since a server-clock timestamp; -1 when there is none. */
export function since(ts: number | undefined): number {
  return ts ? Math.max(0, feed.serverNow() - ts) : -1;
}

export function Msg({ text }: { text: string }) {
  return <div className="panel-body"><p className="dim">{text}</p></div>;
}

/** A signed change, coloured by direction, with an optional percentage of base. */
export function Chg({ d, base, dp = 2 }: { d: number | null | undefined; base?: number; dp?: number }) {
  if (d == null || !Number.isFinite(d)) return <span className="dim">—</span>;
  const cls = d > 0 ? "up" : d < 0 ? "dn" : "dim";
  const pct = base ? ` ${d >= 0 ? "+" : ""}${num((100 * d) / base, 2)}%` : "";
  return <span className={cls}>{d >= 0 ? "+" : ""}{num(d, dp)}{pct}</span>;
}

/** A quantity in lots when the contract has a lot size, else in units. */
export function lots(q: number | undefined, lot: number): string {
  if (q == null) return "";
  return lot > 0 ? num(q / lot, q % lot ? 1 : 0) : num(q, 0);
}

/** The standard normal CDF (Abramowitz–Stegun 7.1.26, |error| < 1.5e-7). */
export function ncdf(x: number): number {
  const t = 1 / (1 + 0.3275911 * Math.abs(x) / Math.SQRT2);
  const y = 1 - ((((1.061405429 * t - 1.453152027) * t + 1.421413741) * t - 0.284496736) * t + 0.254829592) * t * Math.exp(-(x * x) / 2);
  return x >= 0 ? (1 + y) / 2 : (1 - y) / 2;
}
