import { describe, expect, it } from "vitest";
import { filterItems, Notes, parseView, viewArgs, type Feed, type Item, type Reason, type RiskState } from "./news";

const now = () => Date.now();
const risk = (state: RiskState["state"], reasons: Reason[] = [], extra: Partial<RiskState> = {}): RiskState =>
  ({ ts: now(), state, reasons, since: 1000, ...extra });
const rsn = (x: Partial<Reason>): Reason =>
  ({ level: "CAUTION", kind: "event", name: "x", until: now() + 60_000, msg: "m", ...x });
const item = (id: string, x: Partial<Item> = {}): Item =>
  ({ id, src: "et", trust: "media", region: "in", title: `t ${id}`, seen: now(), ts: now(), ...x });
const fd = (items: Item[]): Feed => ({ ts: now(), items, sources: [], rules: 1 });

describe("view args", () => {
  it("parses and writes back in a fixed order", () => {
    expect(parseView(["src", "80", "all"])).toEqual({ all: true, min: 80, src: true });
    expect(parseView(["999"]).min).toBe(100);
    expect(parseView(["junk", "1234"])).toEqual({ all: false, min: 0, src: false });
    expect(viewArgs(parseView(["SRC", "50", "ALL"]))).toEqual(["ALL", "50", "SRC"]);
    expect(viewArgs(parseView(undefined))).toEqual([]);
  });

  it("hides noise unless ALL, and items under the minimum", () => {
    const xs = [item("a", { impact: 90 }), item("b", { impact: 0, tags: ["noise"] }), item("c", { impact: 30 })];
    expect(filterItems(xs, parseView([])).map((i) => i.id)).toEqual(["a", "c"]);
    expect(filterItems(xs, parseView(["ALL"])).map((i) => i.id)).toEqual(["a", "b", "c"]);
    expect(filterItems(xs, parseView(["80"])).map((i) => i.id)).toEqual(["a"]);
  });
});

describe("Notes", () => {
  it("is quiet on the baseline and on a stale or missing state", () => {
    const n = new Notes();
    expect(n.risk(undefined)).toEqual([]);
    expect(n.risk(risk("CAUTION", [rsn({ event: "e1" })], { ts: now() - 120_000 }))).toEqual([]);
    expect(n.risk(risk("CAUTION", [rsn({ event: "e1" })]))).toEqual([]);
    expect(n.risk(risk("CAUTION", [rsn({ event: "e1" })]))).toEqual([]);
  });

  it("notes a state change once, and a reason added while the state holds", () => {
    const n = new Notes();
    n.risk(risk("NORMAL"));
    const up = n.risk(risk("NO_NEW_ENTRIES", [rsn({ level: "NO_NEW_ENTRIES", event: "rbi", msg: "RBI policy" }), rsn({ event: "cpi" })], { since: 5000 }));
    expect(up).toHaveLength(1);
    expect(up[0]).toMatchObject({ level: "NO_NEW_ENTRIES", title: "News risk: NO NEW ENTRIES" });
    expect(up[0].body).toContain("RBI policy (+1 more)");
    expect(n.risk(risk("NO_NEW_ENTRIES", [rsn({ event: "rbi" }), rsn({ event: "cpi" })], { since: 5000 }))).toEqual([]);

    const more = n.risk(risk("NO_NEW_ENTRIES", [rsn({ event: "rbi" }), rsn({ event: "cpi" }), rsn({ kind: "vix", name: "India VIX", msg: "VIX +16%" })], { since: 5000 }));
    expect(more).toEqual([expect.objectContaining({ title: "Also holding CAUTION: India VIX", body: "VIX +16%" })]);

    const down = n.risk(risk("NORMAL", [], { since: 9000 }));
    expect(down).toEqual([expect.objectContaining({ level: "NORMAL", title: "News risk back to NORMAL" })]);
  });

  it("announces a confirmed high-impact headline once, whether via the feed or the state", () => {
    const n = new Notes();
    n.risk(risk("CAUTION", [rsn({ event: "e1" })]));
    n.feed(fd([item("old", { impact: 90, confirmed: 1 })]));
    // old ones are the baseline; unconfirmed, backfill and low impact never note
    expect(n.feed(fd([
      item("old", { impact: 90, confirmed: 1 }),
      item("raw", { impact: 95 }),
      item("back", { impact: 95, confirmed: 1, backfill: true }),
      item("low", { impact: 60, confirmed: 1 }),
    ]))).toEqual([]);

    const hot = item("h1", { impact: 90, confirmed: 1, also: ["mint"], url: "https://example.test/h1" });
    expect(n.feed(fd([hot]))).toEqual([{ id: "item:h1", level: "CAUTION", url: hot.url, title: "Headline, impact 90: et +1", body: "t h1" }]);
    expect(n.feed(fd([hot]))).toEqual([]);
    // the same headline then holding CAUTION adds no second note
    expect(n.risk(risk("CAUTION", [rsn({ event: "e1" }), rsn({ kind: "headline", item: "h1", name: "h1" })]))).toEqual([]);

    // a headline reason first, then the feed: one note, from the state
    const got = n.risk(risk("CAUTION", [rsn({ event: "e1" }), rsn({ kind: "headline", item: "h2", name: "h2" })]));
    expect(got).toHaveLength(1);
    expect(n.feed(fd([item("h2", { impact: 90, confirmed: 1 })]))).toEqual([]);
  });

  it("keeps its seen set bounded", () => {
    const n = new Notes();
    n.feed(fd([]));
    for (let i = 0; i < 1200; i++) n.feed(fd([item(`i${i}`, { impact: 90, confirmed: 1 })]));
    // the oldest fell out, so it would note again; a recent one would not
    expect(n.feed(fd([item("i0", { impact: 90, confirmed: 1 })]))).toHaveLength(1);
    expect(n.feed(fd([item("i1199", { impact: 90, confirmed: 1 })]))).toEqual([]);
  });
});
