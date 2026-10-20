// Payload shapes on the bus, as the ticker and the IV engine publish them.

export interface Level {
  p: number;
  q: number;
  n: number;
}

/** tick.<subject>: a conflated quote. Times are ms on the services' clock. */
export interface Tick {
  c: string;
  m: "f" | "q" | "l"; // full, quote, ltp
  r: number; // received
  e?: number; // exchange timestamp
  lt?: number; // last trade time
  p: number; // last price
  q?: number; // last quantity
  v?: number; // volume
  ap?: number; // average traded price
  oi?: number;
  oh?: number; // day OI high
  ol?: number; // day OI low
  b?: number;
  a?: number;
  bq?: number;
  aq?: number;
  tb?: number; // total buy quantity
  ts?: number; // total sell quantity
  o?: number;
  h?: number;
  lo?: number;
  cl?: number; // previous close
  d?: { b: Level[]; s: Level[] };
}

export interface Leg {
  b: number;
  a: number;
  mid: number;
  spr: number; // spread / mid
  iv: number | null;
  age: number;
  delta: number | null;
  gamma: number | null;
  vega: number | null; // per vol point, per unit
  theta: number | null; // per trading day, per unit
}

export interface IvRow {
  k: number;
  kx: number; // log-moneyness
  c?: Leg | null;
  p?: Leg | null;
  fit: number | null;
}

/** analytics.<und>.iv.<yyyymmdd>: one expiry's smile. */
export interface IvSurface {
  ts: number;
  u: string;
  x: string;
  tte: number; // years, trading-time
  dte: number; // trading days
  cal: number;
  df: number;
  r: number;
  f: number; // forward
  fsrc: string;
  s: number; // spot
  fut: number | null;
  atm: number | null;
  rr25: number | null;
  bf25: number | null;
  n: number;
  fit: { a: number; b: number; rho: number; m: number; s: number; rmse: number; ok: boolean; arb: string[] } | null;
  age: { med: number; max: number };
  rows: IvRow[];
}

export interface TermRow {
  x: string;
  dte: number;
  tte: number;
  f: number;
  atm: number | null;
  rr25: number | null;
  bf25: number | null;
  ok: boolean;
  calarb: boolean;
}

export interface Term {
  ts: number;
  u: string;
  rows: TermRow[];
}

/** One broker position row on book.positions, joined to its contract when refdata knows it. */
export interface BookPos {
  sym: string;
  x: string;
  t: number;
  pr: string; // NRML | MIS | CNC
  c?: string;
  s?: string; // tick subject
  u?: string;
  k?: string; // CE | PE | FUT | EQ
  e?: string; // expiry yyyy-mm-dd
  K?: number;
  lot?: number;
  q: number;
  oq: number; // overnight quantity
  m: number; // multiplier
  avg: number;
  cl: number; // previous close
  p: number; // the broker's last price
  pnl: number;
  m2m: number;
  ur: number;
  rl: number;
  bq: number;
  bv: number;
  sq: number;
  sv: number;
  dbq: number;
  dbv: number;
  dsq: number;
  dsv: number;
}

/** book.positions: the broker's net and day positions, polled every few seconds. */
export interface Book {
  ts: number;
  net: BookPos[];
  day: BookPos[];
}

/** book.margins: the equity segment of the broker's margins. */
export interface Margins {
  ts: number;
  on: boolean;
  net: number; // free to trade with
  cash: number;
  open: number;
  live: number;
  coll: number;
  used: number; // debits
  span: number;
  exp: number;
  prem: number;
  m2mr: number;
  m2mu: number;
}
