import { describe, expect, it } from "vitest";
import { checkRow, m2mAt, pnlAt, sortBook } from "./book";
import type { BookPos } from "./types";

const base: BookPos = {
  sym: "", x: "NFO", t: 1, pr: "NRML", q: 0, oq: 0, m: 1, avg: 0, cl: 0, p: 0, pnl: 0, m2m: 0, ur: 0, rl: 0,
  bq: 0, bv: 0, sq: 0, sv: 0, dbq: 0, dbv: 0, dsq: 0, dsv: 0,
};

// A short call carried from yesterday: sold 150 at 120.5, closed at 110, now 100.25.
const short: BookPos = {
  ...base, sym: "NIFTY26OCT25400CE", u: "NIFTY", k: "CE", e: "2026-10-27", K: 25400,
  q: -150, oq: -150, avg: 120.5, cl: 110, p: 100.25, pnl: 3037.5, m2m: 1462.5, sq: 150, sv: 18075,
};

describe("book arithmetic", () => {
  it("gives back the broker's pnl and m2m at its own price", () => {
    expect(pnlAt(short, short.p)).toBeCloseTo(3037.5, 6);
    expect(m2mAt(short, short.p)).toBeCloseTo(1462.5, 6);
    expect(checkRow(short).ok).toBe(true);
  });

  it("reprices from a live tick", () => {
    expect(pnlAt(short, 105)).toBeCloseTo(18075 - 150 * 105, 6);
    expect(m2mAt(short, 105)).toBeCloseTo(-150 * 105 + 150 * 110, 6);
  });

  it("counts a day-only round trip in m2m", () => {
    const scalp = { ...base, q: 0, bq: 75, bv: 75 * 200, sq: 75, sv: 75 * 208, dbq: 75, dbv: 75 * 200, dsq: 75, dsv: 75 * 208, p: 190, pnl: 600, m2m: 600 };
    expect(checkRow(scalp)).toEqual({ pnl: 0, m2m: 0, ok: true });
  });

  it("flags a row the arithmetic does not explain", () => {
    const c = checkRow({ ...short, pnl: 3000 });
    expect(c.ok).toBe(false);
    expect(c.pnl).toBeCloseTo(37.5, 6);
  });

  it("sorts open rows by underlying, expiry, kind and strike, closed last", () => {
    const rows = [
      { ...short, sym: "closed", q: 0 },
      { ...short, sym: "pe", k: "PE", K: 24600 },
      { ...short, sym: "bnf", u: "BANKNIFTY", k: "PE", K: 55000, e: "2026-10-28" },
      { ...short, sym: "fut", k: "FUT", K: undefined, e: "2026-10-27" },
      short,
    ];
    expect(sortBook(rows).map((r) => r.sym)).toEqual(["bnf", "fut", "NIFTY26OCT25400CE", "pe", "closed"]);
  });
});
