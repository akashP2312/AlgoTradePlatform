// The broker's P&L arithmetic, so the terminal can reprice a position from its own
// ticks and check itself against the broker. With the broker's last price these
// must give back its pnl and m2m to the paisa (the zero-shock check); with a live
// tick they give the P&L now.

import type { BookPos } from "./types";

/** pnl = sell value − buy value + qty·ltp·multiplier. */
export function pnlAt(r: BookPos, ltp: number): number {
  return r.sv - r.bv + r.q * ltp * (r.m || 1);
}

/** m2m = day sell value − day buy value + (qty·ltp − overnight qty·close)·multiplier. */
export function m2mAt(r: BookPos, ltp: number): number {
  return r.dsv - r.dbv + (r.q * ltp - r.oq * r.cl) * (r.m || 1);
}

/** Beyond this many rupees the recomputation and the broker disagree. */
export const CHECK_TOL = 1;

/** How far the recomputation at the broker's own price is from its pnl and m2m;
 * zero when the arithmetic (and so the terminal's repricing) is right. */
export function checkRow(r: BookPos): { pnl: number; m2m: number; ok: boolean } {
  const pnl = pnlAt(r, r.p) - r.pnl;
  const m2m = m2mAt(r, r.p) - r.m2m;
  return { pnl, m2m, ok: Math.abs(pnl) <= CHECK_TOL && Math.abs(m2m) <= CHECK_TOL };
}

/** Open rows first, by underlying, expiry, kind and strike; closed rows last. */
export function sortBook(rows: BookPos[]): BookPos[] {
  const kindRank = (k?: string) => ({ FUT: 0, CE: 1, PE: 2 } as Record<string, number>)[k ?? ""] ?? 3;
  return [...rows].sort((a, b) =>
    Number(a.q === 0) - Number(b.q === 0) ||
    (a.u ?? a.sym).localeCompare(b.u ?? b.sym) ||
    (a.e ?? "").localeCompare(b.e ?? "") ||
    kindRank(a.k) - kindRank(b.k) ||
    (a.K ?? 0) - (b.K ?? 0) ||
    a.sym.localeCompare(b.sym));
}
