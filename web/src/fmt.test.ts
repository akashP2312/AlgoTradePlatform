import { describe, expect, it } from "vitest";
import { age, clock, inr, num } from "./fmt";

describe("fmt", () => {
  it("groups in lakh/crore", () => {
    expect(num(1234567.891, 2)).toBe("12,34,567.89");
    expect(num(null)).toBe("");
    expect(inr(54321)).toBe("₹54,321");
    expect(inr(250000)).toBe("₹2.50 L");
    expect(inr(-31500000)).toBe("₹-3.15 Cr");
  });
  it("formats ages", () => {
    expect(age(-1)).toBe("—");
    expect(age(850)).toBe("850ms");
    expect(age(4200)).toBe("4.2s");
    expect(age(185_000)).toBe("3m 05s");
    expect(age(7_800_000)).toBe("2h 10m");
  });
  it("shows IST", () => {
    expect(clock(Date.UTC(2026, 9, 21, 3, 45, 0))).toBe("09:15:00");
  });
});
