import { describe, expect, it } from "vitest";
import { parse, suggest } from "./registry";

describe("parse", () => {
  it("reads <SECURITY> <FUNCTION> [args]", () => {
    expect(parse("nifty omon 20261027")).toEqual({ sec: "NIFTY", fn: "OMON", args: ["20261027"] });
    expect(parse("bnf strd")).toEqual({ sec: "BANKNIFTY", fn: "STRD", args: [] });
    expect(parse("SYS")).toEqual({ fn: "SYS", args: [] });
    expect(parse("w save intraday")).toEqual({ fn: "W", args: ["save", "intraday"] });
  });
  it("takes a lone security as re-pointing", () => {
    expect(parse("banknifty")).toEqual({ sec: "BANKNIFTY", args: [] });
  });
  it("reports errors", () => {
    expect(parse("  ").error).toBeTruthy();
    expect(parse("nifty foo bar").error).toMatch(/no function/);
    expect(parse("nifty sys").error).toMatch(/takes no security/);
  });
});

describe("suggest", () => {
  it("prefix before substring, securities offered first-position", () => {
    const s = suggest("o", ["NIFTY"]).map((x) => x.label);
    expect(s[0]).toBe("OMON");
    expect(suggest("ni", ["NIFTY", "BANKNIFTY"]).map((x) => x.label)).toContain("NIFTY");
  });
  it("after a security, offers only functions that take one", () => {
    const s = suggest("nifty ", []).map((x) => x.label);
    expect(s).toContain("OMON");
    expect(s).not.toContain("SYS");
    expect(suggest("nifty st", [])[0].text).toBe("NIFTY STRD ");
  });
  it("offers nothing inside args", () => {
    expect(suggest("nifty omon 2026", [])).toEqual([]);
  });
});
