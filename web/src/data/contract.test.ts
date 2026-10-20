import { describe, expect, it } from "vitest";
import { keyString, parseKey, subjectOf } from "./contract";

describe("subjectOf", () => {
  it("matches the Go subjects", () => {
    expect(subjectOf("NFO|NIFTY|2026-10-27|25000|CE")).toBe("tick.nfo.nifty.20261027.25000.ce");
    expect(subjectOf("NFO|NIFTY|2026-10-27|25012.5|PE")).toBe("tick.nfo.nifty.20261027.25012p5.pe");
    expect(subjectOf("NSEIX|GIFT NIFTY|2026-10-27||FUT")).toBe("tick.nseix.gift_nifty.20261027._.fut");
    expect(subjectOf("NSE|NIFTY|||IDX")).toBe("tick.nse.nifty._._.idx");
    expect(subjectOf("NSE|M&M|||EQ")).toBe("tick.nse.m_m._._.eq");
  });
  it("round-trips keys", () => {
    for (const s of ["NFO|NIFTY|2026-10-27|25000|CE", "NSE|RELIANCE|||EQ", "MCX|CRUDEOIL|2026-10-27||FUT"]) {
      expect(keyString(parseKey(s)!)).toBe(s);
    }
    expect(parseKey("NFO|NIFTY|2026-10-27||XX")).toBeNull();
  });
});
