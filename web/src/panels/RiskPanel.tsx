// RISK: the book's scenario risk from risk.book (the risk engine), and the day's
// limits from risk.mon and risk.day (riskmon). P&L against spot at now, the next
// open and each expiry, with the breakevens read off the curves; gamma by strike;
// the intraday M2M against spot from the minute snapshots, with the peak and the
// loss limit; and the full scenario grid. Every rupee figure is the liquidation
// value (exit at the touch) unless it says mid. Read-only: nothing here places or
// changes an order.

import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { age, clock, inr, num } from "../fmt";
import { Chg, Msg, since } from "./common";

const BOOK = "risk.book";
const MON = "risk.mon";
const DAY = "risk.day";
/** risk.book is recomputed every few seconds; riskmon flags it stale at 30s. */
const BOOK_STALE_MS = 30_000;

type Cells = (number | null)[];
interface Greeks { d: number; dn: number; g: number; v: number; v30: number; t: number }
interface Grid { sig: number[]; rows: { dv: number; mid: Cells; liq: Cells }[]; path: { dv: number[]; mid: Cells; liq: Cells } }
interface Horizon { l: string; ts: number; mid: Cells; liq: Cells }
interface Risk extends Greeks {
  close: number;
  flags: { leg: string; why: string }[];
  be: { pct: number | null; sd: number | null } | null;
  sig: number;
  sigday: number;
  grid: Grid;
  time: Horizon[];
}
interface Leg extends Greeks { id: string; name: string; u: string; x: string; k: string; K: number; q: number; flags: string[] }
interface Und extends Risk { u: string; s: number; beta: number }
interface RiskBook {
  ts: number;
  session: boolean;
  ref: string;
  ref_px: number | null;
  beta: { src: string; n: number };
  steps: number[];
  vols: number[];
  legs: Leg[];
  unmod: { id: string }[];
  unds: Und[];
  book: Risk;
  pnl?: { m2m: number; pnl: number; bm2m: number; live: number; stale: number };
}
interface Rule { rule: string; lvl: "ok" | "warn" | "crit"; since?: number; msg?: string; v: number }
interface Mon {
  ts: number; session: boolean; has: boolean;
  m2m: number; pnl: number; bm2m: number; live: number; stale: number;
  peak: number | null; dd: number; maxdd: number;
  limit: number; lim_used: number | null; cap: number;
  worst: number | null; wmove: number; wpct: number;
  util: number | null; used: number; free: number;
  dn: number; band: number;
  book_age: number | null; mg_age: number | null;
  rules: Rule[];
}
interface Snap { ts: number; m2m: number; pnl: number; px: number | null }
interface Day { date: string; rows: Snap[] | null }

const RULE_NAMES: Record<string, string> = {
  risk_loss_limit: "loss limit", risk_worst_cell: "worst cell", risk_margin: "margin",
  risk_delta_band: "delta band", risk_drawdown: "drawdown", risk_stale: "inputs", risk_legs_flagged: "flags",
};
const lvlCls = (l?: string) => (l === "crit" ? "st-bad" : l === "warn" ? "st-warn" : "dim");
const HZ_COLOURS = ["var(--amber)", "var(--blue)", "var(--fg)", "var(--dim)"];

/** ₹ in thousands, for dense cells and axes. */
const k = (x: number | null | undefined) =>
  x == null ? "" : Math.abs(x) < 1000 ? num(Math.round(x), 0) : `${num(x / 1000, Math.abs(x) < 10_000 ? 1 : 0)}k`;

/** Where a curve crosses zero, as step positions, by linear interpolation. */
function zeros(xs: number[], ys: Cells): number[] {
  const out: number[] = [];
  for (let i = 1; i < xs.length; i++) {
    const a = ys[i - 1], b = ys[i];
    if (a == null || b == null) continue;
    if (a === 0 && i === 1) out.push(xs[0]);
    if ((a < 0 && b >= 0) || (a > 0 && b <= 0)) out.push(b === 0 ? xs[i] : xs[i - 1] + ((xs[i] - xs[i - 1]) * a) / (a - b));
  }
  return out;
}

function useWidth<T extends HTMLElement>(): [React.RefObject<T | null>, number] {
  const ref = useRef<T>(null);
  const [w, setW] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setW(Math.floor(e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

interface Line { ys: Cells; colour: string; dash?: string; label: string; width?: number }
interface Plot {
  xs: number[];
  lines: Line[];
  /** a second series on its own scale, drawn against the right axis */
  right?: Line;
  vlines?: { x: number; colour: string; label?: string; row?: number }[];
  hlines?: { y: number; colour: string; label: string }[];
  bars?: boolean;
  xfmt: (x: number) => string;
  yfmt: (y: number) => string;
  rfmt?: (y: number) => string;
  height?: number;
  xticks?: number[];
}

function range(vals: (number | null)[], pad = 0.06): [number, number] {
  const f = vals.filter((v): v is number => v != null && Number.isFinite(v));
  if (!f.length) return [0, 1];
  let lo = Math.min(...f), hi = Math.max(...f);
  if (lo === hi) { lo -= 1; hi += 1; }
  const p = (hi - lo) * pad;
  return [lo - p, hi + p];
}

/** A small SVG chart: lightweight-charts wants a time axis, these are against spot. */
function Chart(p: Plot) {
  const [ref, w] = useWidth<HTMLDivElement>();
  const h = p.height ?? 200;
  const T = p.vlines?.some((l) => l.row) ? 26 : 14;
  const L = 52, R = p.right ? 52 : 8, B = 18;
  const pw = Math.max(10, w - L - R), ph = h - T - B;
  const [x0, x1] = p.bars ? [p.xs[0] - 0.5 * gap(p.xs), p.xs[p.xs.length - 1] + 0.5 * gap(p.xs)] : [p.xs[0], p.xs[p.xs.length - 1]];
  const ys = p.lines.flatMap((l) => l.ys).concat(p.hlines?.map((l) => l.y) ?? [], p.bars ? [0] : []);
  const [y0, y1] = range(ys);
  const X = (x: number) => L + ((x - x0) / (x1 - x0 || 1)) * pw;
  const Y = (y: number) => T + (1 - (y - y0) / (y1 - y0)) * ph;
  const [r0, r1] = p.right ? range(p.right.ys) : [0, 1];
  const RY = (y: number) => T + (1 - (y - r0) / (r1 - r0)) * ph;
  const path = (l: Line, yf: (y: number) => number) => {
    let d = "", pen = false;
    l.ys.forEach((y, i) => {
      if (y == null) { pen = false; return; }
      d += `${pen ? "L" : "M"}${X(p.xs[i]).toFixed(1)},${yf(y).toFixed(1)}`;
      pen = true;
    });
    return d;
  };
  const yt = ticks(y0, y1, 5);
  const xt = p.xticks ?? p.xs;
  // Thin the x labels by pixel spacing: the spot steps bunch up around zero.
  const xl: number[] = [];
  for (const x of xt) if (!xl.length || X(x) - X(xl[xl.length - 1]) >= 48) xl.push(x);
  const bw = p.bars ? Math.max(2, (pw / (p.xs.length || 1)) * 0.7) : 0;
  return (
    <div ref={ref} style={{ width: "100%" }}>
      {w > 0 && (
        <svg width={w} height={h} style={{ display: "block", fontSize: 10, fontFamily: "var(--mono)" }}>
          {yt.map((y) => (
            <g key={`y${y}`}>
              <line x1={L} x2={L + pw} y1={Y(y)} y2={Y(y)} stroke="#161b22" />
              <text x={L - 4} y={Y(y) + 3} textAnchor="end" fill="var(--dim)">{p.yfmt(y)}</text>
            </g>
          ))}
          {p.right && p.rfmt && ticks(r0, r1, 5).map((y) => (
            <text key={`r${y}`} x={L + pw + 4} y={RY(y) + 3} fill="var(--dim)">{p.rfmt!(y)}</text>
          ))}
          {xl.map((x) => (
            <text key={`x${x}`} x={Math.min(L + pw - 18, Math.max(L + 18, X(x)))} y={h - 4} textAnchor="middle" fill="var(--dim)">{p.xfmt(x)}</text>
          ))}
          {y0 < 0 && y1 > 0 && <line x1={L} x2={L + pw} y1={Y(0)} y2={Y(0)} stroke="var(--line)" strokeWidth={1.5} />}
          {p.hlines?.map((l) => (
            <g key={l.label}>
              <line x1={L} x2={L + pw} y1={Y(l.y)} y2={Y(l.y)} stroke={l.colour} strokeDasharray="4 3" />
              <text x={L + pw - 2} y={Y(l.y) - 3} textAnchor="end" fill={l.colour}>{l.label}</text>
            </g>
          ))}
          {p.vlines?.map((l, i) => (
            <g key={`v${i}`}>
              <line x1={X(l.x)} x2={X(l.x)} y1={T} y2={T + ph} stroke={l.colour} strokeDasharray="2 3" />
              {l.label && <text x={X(l.x)} y={T - 3 - 12 * (p.vlines!.some((v) => v.row) ? 1 - (l.row ?? 0) : 0)} textAnchor="middle" fill={l.colour}>{l.label}</text>}
            </g>
          ))}
          {p.bars
            ? p.lines[0]?.ys.map((y, i) => y != null && (
                <rect key={i} x={X(p.xs[i]) - bw / 2} width={bw} y={Math.min(Y(y), Y(0))} height={Math.max(1, Math.abs(Y(y) - Y(0)))}
                  fill={y >= 0 ? "var(--green)" : "var(--red)"} opacity={0.8}><title>{`${p.xfmt(p.xs[i])}: ${p.yfmt(y)}`}</title></rect>
              ))
            : p.lines.map((l) => <path key={l.label} d={path(l, Y)} fill="none" stroke={l.colour} strokeWidth={l.width ?? 1.5} strokeDasharray={l.dash} />)}
          {p.right && <path d={path(p.right, RY)} fill="none" stroke={p.right.colour} strokeWidth={1} strokeDasharray={p.right.dash} />}
        </svg>
      )}
    </div>
  );
}

function gap(xs: number[]): number {
  let g = Infinity;
  for (let i = 1; i < xs.length; i++) g = Math.min(g, xs[i] - xs[i - 1]);
  return Number.isFinite(g) ? g : 1;
}

function ticks(lo: number, hi: number, n: number): number[] {
  const raw = (hi - lo) / n;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? raw;
  const out: number[] = [];
  for (let v = Math.ceil(lo / step) * step; v <= hi; v += step) out.push(Math.round(v / step) * step);
  return out;
}

function Legend({ items }: { items: { label: string; colour: string; dash?: boolean }[] }) {
  return (
    <span style={{ display: "inline-flex", gap: 10 }}>
      {items.map((i) => (
        <span key={i.label} style={{ color: i.colour }}>{i.dash ? "┄" : "━"} {i.label}</span>
      ))}
    </span>
  );
}

function Section({ title, extra, children }: { title: string; extra?: ReactNode; children: ReactNode }) {
  return (
    <section>
      <h3 style={{ display: "flex", gap: 12, alignItems: "baseline" }}><span>{title}</span>{extra && <span style={{ textTransform: "none", fontWeight: "normal", letterSpacing: 0 }}>{extra}</span>}</h3>
      {children}
    </section>
  );
}

/** A fill bar for a ratio, 1 = at the limit. */
function Bar({ v, warn }: { v: number | null; warn: number }) {
  if (v == null) return <span className="dim">—</span>;
  const c = v >= 1 ? "var(--red)" : v >= warn ? "var(--warn)" : "var(--green)";
  return (
    <span style={{ display: "inline-block", width: 80, height: 8, background: "var(--line)", verticalAlign: "middle", position: "relative" }}>
      <span style={{ position: "absolute", left: 0, top: 0, bottom: 0, width: `${Math.min(100, Math.max(0, 100 * v))}%`, background: c }} />
      <span style={{ position: "absolute", left: `${100 * warn}%`, top: -2, bottom: -2, width: 1, background: "var(--fg)" }} />
    </span>
  );
}

export function RiskPanel() {
  useSub([BOOK, MON, DAY]);
  const v = usePoll(() => ({
    rb: feed.store.get<RiskBook>(BOOK),
    mon: feed.store.get<Mon>(MON),
    day: feed.store.get<Day>(DAY),
  }), 2);
  const [und, setUnd] = useState<string>("");
  const { rb, mon, day } = v;
  if (!rb) return <Msg text="waiting for risk.book (is the risk engine running?)" />;

  const bookAge = since(rb.ts);
  const stale = bookAge > BOOK_STALE_MS;
  const ref = rb.ref_px ?? 0;
  const spot = (s: number) => ref * (1 + s / 100);
  const b = rb.book;
  const sel = rb.unds.find((u) => u.u === und);
  const r: Risk = sel ?? b;
  const label = sel ? sel.u : `book (${rb.ref} β-weighted)`;
  const bad = (mon?.rules ?? []).filter((x) => x.lvl !== "ok");
  const flags = b.flags.length + rb.unmod.length;

  // P&L vs spot: one curve per horizon; breakevens off the "now" and last curves.
  const hz = r.time;
  const now = hz[0];
  const last = hz[hz.length - 1];
  const beNow = now ? zeros(rb.steps, now.liq) : [];
  const beExp = last && last !== now ? zeros(rb.steps, last.liq) : [];

  // Gamma by strike, for one underlying (strikes don't share a scale across them).
  const gu = sel?.u ?? rb.ref;
  const byK = new Map<number, number>();
  for (const l of rb.legs) if (l.u === gu && l.K && (l.k === "CE" || l.k === "PE")) byK.set(l.K, (byK.get(l.K) ?? 0) + l.g);
  const Ks = [...byK.keys()].sort((a, b) => a - b);
  const uS = rb.unds.find((u) => u.u === gu)?.s;

  // Intraday: the minute snapshots, then the live value on the end.
  const snaps = day?.rows ?? [];
  const live = mon?.has && mon.session ? [{ ts: mon.ts, m2m: mon.m2m, px: rb.ref_px }] : [];
  const pts = [...snaps, ...live];
  const tx = pts.map((s) => s.ts);

  // Worst cell within the alert's move, to mark in the grid.
  const wmove = mon?.wmove ?? 5;
  const allRows: { l: string; dv?: string; mid: Cells; liq: Cells }[] = [
    ...r.grid.rows.map((x) => ({ l: `vol ${x.dv > 0 ? "+" : ""}${num(x.dv, 0)}`, mid: x.mid, liq: x.liq })),
    { l: "path", dv: r.grid.path.dv.map((d) => num(d, 1)).join(" "), mid: r.grid.path.mid, liq: r.grid.path.liq },
    ...hz.slice(1).map((h) => ({ l: h.l, mid: h.mid, liq: h.liq })),
  ];
  let worst: number | null = null;
  for (const row of allRows.slice(0, r.grid.rows.length + 1))
    row.liq.forEach((c, i) => { if (c != null && Math.abs(rb.steps[i]) <= wmove && (worst == null || c < worst)) worst = c; });

  return (
    <div className="gp">
      <div className="omon-head">
        <span className="name">RISK</span>
        {mon && <span className={mon.session ? "" : "dim"}>{mon.session ? "in session" : "closed"}</span>}
        <span><span className="dim">M2M </span><Chg d={mon?.has ? mon.m2m : rb.pnl?.m2m} dp={0} /></span>
        <span><span className="dim">P&L </span><Chg d={mon?.has ? mon.pnl : rb.pnl?.pnl} dp={0} /></span>
        {mon?.peak != null && <span><span className="dim">peak </span>{inr(mon.peak)}</span>}
        {mon && <span title="drop from today's peak M2M · the day's worst"><span className="dim">dd </span>{inr(mon.dd)}<span className="dim"> max {inr(mon.maxdd)}</span></span>}
        {mon && mon.limit > 0 && (
          <span title={`loss limit ₹${inr(mon.limit)} · tick at the warn level`}>
            <span className="dim">limit </span><Bar v={mon.lim_used} warn={ruleWarn(mon, "risk_loss_limit")} />{" "}
            {mon.lim_used != null && <span className={lvlCls(rule(mon, "risk_loss_limit")?.lvl)}>{num(100 * mon.lim_used, 0)}%</span>}
            <span className="dim"> of {inr(mon.limit)}</span>
          </span>
        )}
        {rb.pnl && rb.pnl.stale > 0 && <span className="st-warn" title="open legs left at the broker's price: no fresh tick">{rb.pnl.stale} legs unpriced</span>}
        <span className={stale ? "st-warn" : "dim"} title="risk.book age">{age(bookAge)}</span>
      </div>
      <div className="omon-head">
        <span className="name">GREEKS</span>
        <select value={und} onChange={(e) => setUnd(e.target.value)} title="the whole book β-weighted to the reference, or one underlying">
          <option value="">book</option>
          {rb.unds.map((u) => <option key={u.u} value={u.u}>{u.u}</option>)}
        </select>
        <span title="₹ per 1% move"><span className="dim">Δ </span><Chg d={r.d} dp={0} /></span>
        <span title={`delta in ${rb.ref} lots`}><span className="dim">Δ lots </span><Chg d={r.dn} dp={2} />{mon && !sel && <span className="dim"> / ±{num(mon.band, 1)}</span>}</span>
        <span title="₹ change in Δ (per 1%) per 1% move"><span className="dim">Γ </span><Chg d={r.g} dp={0} /></span>
        <span title="₹ per vol point"><span className="dim">V </span><Chg d={r.v} dp={0} /></span>
        <span title="₹ per vol point, 30-day-equivalent"><span className="dim">V30 </span><Chg d={r.v30} dp={0} /></span>
        <span title="₹ per day"><span className="dim">Θ </span><Chg d={r.t} dp={0} /></span>
        {r.be?.pct != null && <span title="nearest breakeven at expiry"><span className="dim">BE </span>±{num(r.be.pct, 2)}%<span className="dim"> {num(r.be.sd, 2)}σ</span></span>}
        <span title="one-day 1σ move"><span className="dim">σ day </span>{num(100 * r.sigday, 2)}%</span>
        {!sel && rb.beta.src === "default" && <span className="st-warn" title="β from defaults: index EOD history not backfilled">β default</span>}
      </div>
      {mon && (
        <div className="omon-head">
          <span className="name">LIMITS</span>
          <span title={`worst liquidation cell within ±${num(mon.wmove, 0)}%, vs ${num(mon.wpct, 1)}% of capital ₹${inr(mon.cap)}`}>
            <span className="dim">worst ±{num(mon.wmove, 0)}% </span>
            <span className={lvlCls(rule(mon, "risk_worst_cell")?.lvl)}>{mon.worst == null ? "—" : inr(mon.worst)}</span>
            {mon.worst != null && mon.cap > 0 && <span className="dim"> {num((-100 * mon.worst) / mon.cap, 1)}% / {num(mon.wpct, 1)}%</span>}
          </span>
          <span title={`used ₹${inr(mon.used)} · free ₹${inr(mon.free)}`}>
            <span className="dim">margin </span><Bar v={mon.util} warn={0.8} />{" "}
            <span className={lvlCls(rule(mon, "risk_margin")?.lvl)}>{mon.util == null ? "—" : `${num(100 * mon.util, 1)}%`}</span>
          </span>
          <span title="ages of the inputs"><span className="dim">book </span>{age(mon.book_age)}<span className="dim"> margins </span>{age(mon.mg_age)}</span>
          <span className={flags ? "st-warn" : "dim"} title={[...b.flags.map((f) => `${f.leg}: ${f.why}`), ...rb.unmod.map((u) => `${u.id}: not modelled`)].join("\n")}>
            {flags ? `${flags} flagged` : "no flags"}
          </span>
          {bad.length === 0
            ? <span className="st-ok">all clear</span>
            : bad.map((x) => <span key={x.rule} className={lvlCls(x.lvl)} title={x.msg}>{RULE_NAMES[x.rule] ?? x.rule}{x.since ? ` ${age(since(x.since))}` : ""}</span>)}
        </div>
      )}
      <div className="panel-body mon" style={{ flex: 1, minHeight: 0, overflow: "auto" }}>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(340px, 1fr))", gap: 12 }}>
          <Section title={`P&L vs spot · ${label}`} extra={<Legend items={hz.map((h, i) => ({ label: h.l, colour: HZ_COLOURS[i % HZ_COLOURS.length] }))} />}>
            <Chart
              xs={rb.steps}
              lines={hz.map((h, i) => ({ ys: h.liq, colour: HZ_COLOURS[i % HZ_COLOURS.length], label: h.l, width: i === 0 ? 2 : 1.25, dash: i === hz.length - 1 && i > 0 ? "5 3" : undefined }))}
              vlines={[
                { x: 0, colour: "var(--dim)", label: ref && ![...beNow, ...beExp].some((x) => Math.abs(x) < 0.75) ? num(ref, 0) : undefined },
                ...beNow.map((x) => ({ x, colour: HZ_COLOURS[0], label: num(spot(x), 0) })),
                ...beExp.map((x) => ({ x, colour: HZ_COLOURS[(hz.length - 1) % HZ_COLOURS.length], label: num(spot(x), 0), row: 1 })),
              ]}
              xfmt={(s) => (ref ? num(spot(s), 0) : `${s}%`)}
              yfmt={k}
              height={220}
            />
            <p className="dim" style={{ margin: "2px 0 0" }}>
              liquidation value along the vol path · breakevens now {beNow.map((x) => num(spot(x), 0)).join(", ") || "none"}
              {last && last !== now && <> · {last.l} {beExp.map((x) => num(spot(x), 0)).join(", ") || "none"}</>}
            </p>
          </Section>
          <Section title={`Gamma by strike · ${gu}`} extra={uS != null && <span className="dim">spot {num(uS, 0)}</span>}>
            {Ks.length === 0 ? (
              <p className="dim">no options in {gu}</p>
            ) : (
              <Chart
                xs={Ks}
                lines={[{ ys: Ks.map((K) => byK.get(K) ?? 0), colour: "var(--blue)", label: "Γ" }]}
                bars
                vlines={uS != null && uS >= Ks[0] - gap(Ks) && uS <= Ks[Ks.length - 1] + gap(Ks) ? [{ x: uS, colour: "var(--amber)", label: "spot" }] : []}
                xfmt={(x) => num(x, 0)}
                yfmt={k}
                height={220}
              />
            )}
          </Section>
          <Section
            title={`Intraday M2M vs ${rb.ref}`}
            extra={<Legend items={[{ label: "M2M", colour: "var(--amber)" }, { label: rb.ref, colour: "var(--dim)", dash: true }]} />}
          >
            {pts.length < 2 ? (
              <p className="dim">{mon?.session ? "the first minute snapshots are on their way" : `no snapshots for ${day?.date ?? "today"}: riskmon records them in session`}</p>
            ) : (
              <Chart
                xs={tx}
                xticks={tx.filter((t, i) => i === 0 || Math.floor(t / 1_800_000) !== Math.floor(tx[i - 1] / 1_800_000))}
                lines={[{ ys: pts.map((s) => s.m2m), colour: "var(--amber)", label: "M2M", width: 2 }]}
                right={{ ys: pts.map((s) => s.px ?? null), colour: "var(--dim)", label: rb.ref, dash: "3 3" }}
                hlines={[
                  ...(mon?.peak != null ? [{ y: mon.peak, colour: "var(--green)", label: `peak ${k(mon.peak)}` }] : []),
                  ...(mon && mon.limit > 0 && Math.min(...pts.map((s) => s.m2m)) < -0.5 * mon.limit ? [{ y: -mon.limit, colour: "var(--red)", label: `limit −${k(mon.limit)}` }] : []),
                ]}
                xfmt={(t) => clock(t).slice(0, 5)}
                yfmt={k}
                rfmt={(y) => num(y, 0)}
                height={200}
              />
            )}
          </Section>
        </div>
        <Section title={`Scenarios · ${label}`} extra={<span className="dim">liquidation ₹ · columns are spot moves, σ below · rows are vol shocks (pts), the vol path and later horizons · <span className="amber">■</span> worst within ±{num(wmove, 0)}%</span>}>
          <table className="grid">
            <thead>
              <tr>
                <th className="l"></th>
                {rb.steps.map((s) => <th key={s} title={ref ? `spot ${num(spot(s), 0)}` : undefined}>{s > 0 ? "+" : ""}{num(s, s % 1 ? 1 : 0)}%</th>)}
              </tr>
              <tr>
                <th className="l dim">σ</th>
                {r.grid.sig.map((s, i) => <th key={i} className="dim">{num(s, 1)}</th>)}
              </tr>
            </thead>
            <tbody>
              {allRows.map((row, ri) => (
                <tr key={row.l} className={ri === r.grid.rows.length || ri === r.grid.rows.length + 1 ? "sep" : ""}>
                  <td className="l dim" title={row.dv ? `vol shock by move: ${row.dv}` : undefined}>{row.l}</td>
                  {row.liq.map((c, i) => (
                    <td key={i} className={`n ${c == null ? "dim" : c > 0 ? "up" : c < 0 ? "dn" : "dim"}`}
                      style={c != null && c === worst && Math.abs(rb.steps[i]) <= wmove && ri <= r.grid.rows.length ? { outline: "1px solid var(--amber)", outlineOffset: -1 } : undefined}
                      title={`mid ${row.mid[i] == null ? "—" : inr(row.mid[i])}${ref ? ` · spot ${num(spot(rb.steps[i]), 0)}` : ""}`}>
                      {c == null ? "—" : k(c)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
          <p className="dim" style={{ margin: "4px 0 0" }}>
            {rb.legs.length} legs · {clock(rb.ts)} · β {rb.beta.src}{rb.beta.n ? ` (${rb.beta.n} days)` : ""}
          </p>
        </Section>
      </div>
    </div>
  );
}

function rule(m: Mon, name: string): Rule | undefined {
  return m.rules.find((r) => r.rule === name);
}

/** Where the loss-limit bar's warn tick goes: the rule's value is in units of
 * the warn threshold, lim_used in units of the limit. */
function ruleWarn(m: Mon, name: string): number {
  const r = rule(m, name);
  if (!r || !r.v || m.lim_used == null) return 0.75;
  return Math.min(1, Math.max(0, m.lim_used / r.v));
}
