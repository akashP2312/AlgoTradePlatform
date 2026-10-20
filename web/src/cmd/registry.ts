// The function mnemonics and the command grammar: `<SECURITY> <FUNCTION> [args]`.
// A function that takes no security is typed alone (SYS, W); a security typed alone
// re-points the focused panel's function at it.

export type SecRule = "none" | "optional" | "required";

export interface Fn {
  code: string;
  title: string;
  sec: SecRule;
  built: boolean; // false: a placeholder until its screen lands
}

export const FNS: Fn[] = [
  { code: "SYS", title: "System: feed state, tick ages, service health", sec: "none", built: true },
  { code: "Q", title: "Quote: bid, ask, last, depth · NIFTY Q, NIFTY Q FUT, NIFTY Q 25000CE X2", sec: "required", built: true },
  { code: "MON", title: "Monitor: indices, basis, VIX, GIFT, FX, crude, heavyweights · MON, MON TCS INFY", sec: "none", built: true },
  { code: "GP", title: "Graph price: 1m candles, volume, OI, ±1d σ bands (GP 3D for 3 sessions)", sec: "required", built: true },
  { code: "OMON", title: "Option monitor: chain with IV, greeks, OI and ΔOI (OMON X2 for the next expiry)", sec: "required", built: true },
  { code: "STRD", title: "Straddle: ATM straddle through the day vs prior sessions, breakevens, implied move (STRD 25000 3D)", sec: "required", built: true },
  { code: "POS", title: "Positions: the broker book repriced live, margins, and the zero-shock check against the broker P&L", sec: "none", built: true },
  { code: "RISK", title: "Risk: scenario grid, P&L vs spot, gamma by strike, intraday M2M, loss limit and alerts", sec: "none", built: true },
  { code: "BOT", title: "Bot: the algo-trading bot's ledger, daily-run heartbeat, positions, proposed orders, trades (read-only)", sec: "none", built: true },
  { code: "W", title: "Workspaces: W, W <name>, W SAVE <name>, W DEL <name>", sec: "none", built: true },
  { code: "HELP", title: "Keys and functions", sec: "none", built: true },
];

const BY_CODE = new Map(FNS.map((f) => [f.code, f]));

export function fn(code: string): Fn | undefined {
  return BY_CODE.get(code.toUpperCase());
}

// Short forms for the configured underlyings; anything else is taken as typed.
const ALIASES: Record<string, string> = {
  NF: "NIFTY", BNF: "BANKNIFTY", FIN: "FINNIFTY", VIX: "INDIAVIX", GIFT: "GIFT NIFTY", CRUDE: "CRUDEOIL",
};

/** A typed security token: an alias, or the name with _ standing for a space
 * (GIFT_NIFTY), since tokens are split on whitespace. */
export function security(s: string): string {
  const u = s.toUpperCase();
  return ALIASES[u] ?? u.replaceAll("_", " ");
}

export interface Parsed {
  sec?: string;
  fn?: string;
  args: string[];
  error?: string;
}

export function parse(input: string): Parsed {
  const t = input.trim().split(/\s+/).filter(Boolean);
  if (!t.length) return { args: [], error: "empty command" };
  let out: Parsed;
  if (fn(t[0])) out = { fn: t[0].toUpperCase(), args: t.slice(1) };
  else if (t.length > 1 && fn(t[1])) out = { sec: security(t[0]), fn: t[1].toUpperCase(), args: t.slice(2) };
  else if (t.length === 1) return { sec: security(t[0]), args: [] };
  else return { args: t, error: `no function in "${input.trim()}"` };
  const f = fn(out.fn!)!;
  if (f.sec === "none" && out.sec) out.error = `${f.code} takes no security`;
  return out;
}

export interface Suggestion {
  text: string; // the full command line it completes to
  label: string;
  title: string;
}

/**
 * Completions for what is typed so far. The token being typed is matched as a
 * function when it can stand in that position (first, or second after a security),
 * and as a security otherwise: prefix matches first, then substrings of the code,
 * then words of the title.
 */
export function suggest(input: string, securities: string[], limit = 8): Suggestion[] {
  const lead = input.replace(/^\s+/, "");
  const t = lead.split(/\s+/);
  const cur = (t[t.length - 1] ?? "").toUpperCase();
  const before = t.slice(0, -1);
  if (before.length > 1 || (before.length === 1 && fn(before[0]))) return []; // in args
  const head = before.length ? security(before[0]) + " " : "";
  const out: { s: Suggestion; score: number }[] = [];
  for (const f of FNS) {
    if (head && f.sec === "none") continue;
    const score = f.code.startsWith(cur) ? 0 : f.code.includes(cur) ? 1 : cur && f.title.toUpperCase().includes(cur) ? 2 : -1;
    if (score >= 0) out.push({ s: { text: head + f.code + " ", label: f.code, title: f.title }, score });
  }
  if (!before.length && cur) {
    for (const s of securities) {
      if (s.startsWith(security(cur)) || s.startsWith(cur)) out.push({ s: { text: s + " ", label: s, title: "security" }, score: 0.5 });
    }
  }
  return out.sort((a, b) => a.score - b.score).slice(0, limit).map((x) => x.s);
}
