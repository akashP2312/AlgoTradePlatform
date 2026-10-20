import { describe, expect, it } from "vitest";
import { decimals, expiries, expiryArg, resolve, type Ref } from "./ref";

// Far-future expiries so "from today on" holds whatever the clock says.
const r: Ref = {
  today: "2099-01-01",
  insts: [
    { c: "NSE|NIFTY|||IDX", u: "NIFTY", k: "IDX", lot: 0, tick: 0 },
    { c: "NFO|NIFTY|2099-01-29||FUT", u: "NIFTY", k: "FUT", x: "2099-01-29", lot: 75, tick: 0.1 },
    { c: "NFO|NIFTY|2099-02-26||FUT", u: "NIFTY", k: "FUT", x: "2099-02-26", lot: 75, tick: 0.1 },
    { c: "MCX|CRUDEOIL|2099-01-20||FUT", u: "CRUDEOIL", k: "FUT", x: "2099-01-20", lot: 100, tick: 1 },
    { c: "NSE|INFY|||EQ", u: "INFY", k: "EQ", lot: 1, tick: 0.05 },
  ],
  chains: { NIFTY: [{ x: "2099-01-06", lot: 75, tick: 0.05, n: 3 }, { x: "2099-01-29", lot: 75, tick: 0.05, n: 3 }] },
};

const c = (sec: string, ...args: string[]) => {
  const x = resolve(sec, args, r);
  return "error" in x ? x.error : x.c;
};

describe("resolve", () => {
  it("defaults to the index, else the near future, else the equity", () => {
    expect(c("NIFTY")).toBe("NSE|NIFTY|||IDX");
    expect(c("CRUDEOIL")).toBe("MCX|CRUDEOIL|2099-01-20||FUT");
    expect(c("INFY")).toBe("NSE|INFY|||EQ");
  });
  it("picks futures", () => {
    expect(c("NIFTY", "FUT")).toBe("NFO|NIFTY|2099-01-29||FUT");
    expect(c("NIFTY", "FUT2")).toBe("NFO|NIFTY|2099-02-26||FUT");
    expect(c("NIFTY", "FUT3")).toMatch(/no such future/);
  });
  it("picks options on the nearest or a named expiry", () => {
    expect(c("NIFTY", "25000CE")).toBe("NFO|NIFTY|2099-01-06|25000|CE");
    expect(c("NIFTY", "25000", "pe", "X2")).toBe("NFO|NIFTY|2099-01-29|25000|PE");
    expect(c("NIFTY", "25000CE", "20990129")).toBe("NFO|NIFTY|2099-01-29|25000|CE");
    expect(c("NIFTY", "25000CE", "2099-03-01")).toMatch(/no 2099-03-01 expiry/);
    expect(c("INFY", "1500CE")).toMatch(/no options/);
  });
  it("rejects nonsense", () => {
    expect(c("ZZZ")).toMatch(/unknown/);
    expect(c("NIFTY", "BLAH")).toMatch(/don't understand/);
  });
});

describe("decimals", () => {
  it("follows the tick", () => {
    expect([0.05, 0.1, 0.0025, 1, 0.5, 0].map(decimals)).toEqual([2, 1, 4, 0, 1, 2]);
  });
});

describe("expiries", () => {
  const ch = (x: string, live?: boolean) => ({ x, lot: 75, tick: 0.05, n: 3, live });
  it("lists only the streamed chains when the gateway marks any", () => {
    const lr: Ref = { ...r, chains: { NIFTY: [ch("2099-01-06", true), ch("2099-01-13"), ch("2099-01-29", true)] } };
    expect(expiries("NIFTY", lr)).toEqual(["2099-01-06", "2099-01-29"]);
    expect(expiryArg("NIFTY", "X2", lr)).toBe("2099-01-29");
  });
  it("lists every chain when none is marked", () => {
    expect(expiries("NIFTY", r)).toEqual(["2099-01-06", "2099-01-29"]);
  });
});
