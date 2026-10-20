// Contract keys and their tick subjects, mirroring internal/contract: a key is
// EXCH|NAME|yyyy-mm-dd|strike|KIND and its subject is tick. + Key.Subject().

export type Kind = "IDX" | "EQ" | "FUT" | "CE" | "PE";

export interface Key {
  exch: string;
  name: string;
  x: string; // expiry yyyy-mm-dd, "" for none
  k: number; // strike, 0 unless an option
  kind: Kind;
}

export const isOption = (k: Kind) => k === "CE" || k === "PE";

export function parseKey(s: string): Key | null {
  const p = s.split("|");
  if (p.length !== 5 || !p[0] || !p[1] || !["IDX", "EQ", "FUT", "CE", "PE"].includes(p[4])) return null;
  const kind = p[4] as Kind;
  return { exch: p[0], name: p[1], x: p[2], k: isOption(kind) ? Number(p[3]) : 0, kind };
}

export function keyString(k: Key): string {
  return [k.exch, k.name, k.x, isOption(k.kind) ? String(k.k) : "", k.kind].join("|");
}

function tok(s: string): string {
  return s ? s.toLowerCase().replace(/[^a-z0-9-]/g, "_") : "_";
}

/** The tick subject of a key, e.g. tick.nfo.nifty.20261027.25000.ce. */
export function subjectOf(k: Key | string): string {
  const key = typeof k === "string" ? parseKey(k) : k;
  if (!key) throw new Error(`bad contract key ${k}`);
  const x = key.x ? key.x.replaceAll("-", "") : "_";
  const strike = isOption(key.kind) ? String(key.k).replace(".", "p") : "_";
  return ["tick", tok(key.exch), tok(key.name), x, tok(strike), tok(key.kind)].join(".");
}

/** The subject token of an underlying, as analytics subjects use it. */
export const undTok = tok;
