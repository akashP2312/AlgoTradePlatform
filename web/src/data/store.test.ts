import { describe, expect, it } from "vitest";
import { Store, matches } from "./store";

describe("matches", () => {
  it.each([
    ["sys.>", "sys.ticker.feed", true],
    ["sys.>", "sys", false],
    ["sys.*.health", "sys.ivengine.health", true],
    ["sys.*.health", "sys.ticker.feed", false],
    ["tick.nse.nifty.>", "tick.nse.nifty._._.idx", true],
    ["tick.*", "tick.nse.nifty", false],
    ["analytics.nifty.term", "analytics.nifty.term", true],
  ])("%s ~ %s → %s", (p, s, want) => expect(matches(p, s)).toBe(want));
});

describe("Store", () => {
  it("keeps the latest value, bumps version, matches sorted", () => {
    const s = new Store();
    s.set("sys.b.health", 1, 10);
    s.set("sys.a.health", 2, 11);
    s.set("sys.a.health", 3, 12);
    expect(s.get("sys.a.health")).toBe(3);
    expect(s.version).toBe(3);
    expect(s.size).toBe(2);
    expect(s.match("sys.*.health").map(([k]) => k)).toEqual(["sys.a.health", "sys.b.health"]);
  });
});
