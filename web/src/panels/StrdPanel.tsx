// STRD: the ATM straddle through the day. From /api/straddle (the recorder's chain
// snapshots, refetched every minute): the rolling ATM straddle (amber), a fixed strike's
// straddle (blue; the strike typed, else today's last ATM), and the prior sessions'
// ATM straddle in grey, moved onto today's clock so the decay lines up by time of day.
// The last point follows the live IV surface. The forward sits in the pane below.
// Args: an expiry (X2, 2026-11-03), a strike (25000), sessions (3D; default 3).

import { useEffect, useRef, useState } from "react";
import {
  createChart, LineSeries, LineStyle,
  type IChartApi, type ISeriesApi, type UTCTimestamp,
} from "lightweight-charts";
import { feed } from "../data/feed";
import { useFrame, usePoll, useSub } from "../data/frame";
import { expiries, expiryArg, serverToday, useRefData } from "../data/ref";
import type { IvRow, IvSurface } from "../data/types";
import { age, num } from "../fmt";
import { Chg, ivSubject, Msg, since, STALE_MS } from "./common";

const IST = 19800; // the axis reads UTC, so points are shifted to show IST
const REFETCH_MS = 60_000;
const EXPIRY_BUTTONS = 6;
const PRIOR_GREYS = ["#8b949e", "#6e7681", "#545d68", "#484f58", "#3d444d"];

// [unix seconds, forward, ATM strike, ATM straddle, fixed-strike straddle|null]
type Pt = [number, number, number, number, number | null];

interface Strd {
  und: string;
  x: string;
  k: number | null;
  sessions: { d: string; pts: Pt[] }[];
}

interface Chart {
  chart: IChartApi;
  atm: ISeriesApi<"Line">;
  fixed: ISeriesApi<"Line">;
  fwd: ISeriesApi<"Line">;
  prior: ISeriesApi<"Line">[];
}

const css = (v: string) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
const DAY = 86400;
const dayStart = (d: string) => Date.parse(`${d}T00:00:00Z`) / 1000;

/** The straddle (call mid + put mid) at a strike, when both legs are quoted. */
function straddleAt(s: IvSurface, k: number): number | null {
  const w = s.rows.find((r) => r.k === k);
  return w?.c?.mid && w?.p?.mid ? w.c.mid + w.p.mid : null;
}

/** The strike nearest the forward with both legs quoted. */
function atmRow(s: IvSurface): IvRow | undefined {
  let best: IvRow | undefined;
  for (const w of s.rows) {
    if (!w.c?.mid || !w.p?.mid) continue;
    if (!best || Math.abs(w.k - s.f) < Math.abs(best.k - s.f)) best = w;
  }
  return best;
}

export function StrdPanel({ sec, args, setArgs }: { sec?: string; args?: string[]; setArgs: (a: string[]) => void }) {
  const r = useRefData();
  const und = sec ?? "";
  const xs = r ? expiries(und, r) : [];
  let x = xs[0] ?? "";
  let kArg: number | null = null;
  let nDays = 3;
  let err = "";
  for (const a of args ?? []) {
    const d = /^(\d+)D$/i.exec(a);
    const e = r ? expiryArg(und, a, r) : null;
    if (d) nDays = Math.min(10, Math.max(1, Number(d[1])));
    else if (e) x = xs.includes(e) ? e : x; // an unstreamed expiry (a stale saved arg) falls back to the nearest
    else if (/^\d+(\.\d+)?$/.test(a)) kArg = Number(a);
    else err = `don't understand "${a}": give an expiry (X2), a strike (25000) or sessions (3D)`;
  }
  const sSubj = und && x ? ivSubject(und, x) : "";
  useSub(sSubj ? [sSubj] : []);

  const box = useRef<HTMLDivElement>(null);
  const g = useRef<Chart | null>(null);
  const fitted = useRef("");
  const [data, setData] = useState<Strd | null>(null);
  const [note, setNote] = useState("loading…");
  const today = serverToday();
  // The chart div only exists once the reference data says there is a chain.
  const ready = !!r?.chains[und]?.length;

  // The chart lives as long as the panel shows one.
  useEffect(() => {
    if (!ready || !box.current) return;
    const chart = createChart(box.current, {
      autoSize: true,
      layout: { background: { color: css("--bg") }, textColor: css("--dim"), fontFamily: css("--mono"), fontSize: 11 },
      grid: { vertLines: { color: "#161b22" }, horzLines: { color: "#161b22" } },
      rightPriceScale: { borderColor: css("--line") },
      timeScale: { borderColor: css("--line"), timeVisible: true, secondsVisible: false, lockVisibleTimeRangeOnResize: true },
      crosshair: { mode: 0 },
    });
    const fwd = chart.addSeries(LineSeries, { color: css("--dim"), lineWidth: 1, title: "F" }, 1);
    const fixed = chart.addSeries(LineSeries, { color: css("--blue"), lineWidth: 2 });
    const atm = chart.addSeries(LineSeries, { color: css("--amber"), lineWidth: 2, title: "ATM" });
    const panes = chart.panes();
    panes[0].setStretchFactor(0.78);
    panes[1]?.setStretchFactor(0.22);
    g.current = { chart, atm, fixed, fwd, prior: [] };
    return () => {
      chart.remove();
      g.current = null;
      fitted.current = "";
    };
  }, [ready]);

  // History, refetched every minute.
  useEffect(() => {
    if (!und || !x || err) return;
    let dead = false;
    setData(null);
    setNote("loading…");
    const load = async () => {
      try {
        const q = `und=${encodeURIComponent(und)}&x=${x}&days=${nDays}${kArg != null ? `&k=${kArg}` : ""}`;
        const rsp = await fetch(`/api/straddle?${q}`);
        const body = await rsp.json();
        if (dead) return;
        if (!rsp.ok) return setNote(body.error ?? `straddle: HTTP ${rsp.status}`);
        setData(body);
        setNote(body.sessions.length ? "" : "no snapshots for this expiry yet");
      } catch {
        if (!dead) setNote("straddle: gateway unreachable");
      }
    };
    load();
    const id = setInterval(load, REFETCH_MS);
    return () => {
      dead = true;
      clearInterval(id);
    };
  }, [und, x, nDays, kArg, err]);

  // Draw the history whenever it changes.
  useEffect(() => {
    const c = g.current;
    if (!c || !data) return;
    const cur = data.sessions.find((s) => s.d === today);
    const t = (ts: number) => (ts + IST) as UTCTimestamp;
    const pts = cur?.pts ?? [];
    c.atm.setData(pts.map((p) => ({ time: t(p[0]), value: p[3] })));
    c.fixed.setData(pts.filter((p) => p[4] != null).map((p) => ({ time: t(p[0]), value: p[4]! })));
    c.fixed.applyOptions({ title: data.k != null ? `${data.k}` : "" });
    c.fwd.setData(pts.map((p) => ({ time: t(p[0]), value: p[1] })));
    // Prior sessions, newest first, each moved onto today's date.
    const prior = data.sessions.filter((s) => s.d < today).sort((a, b) => (a.d < b.d ? 1 : -1));
    while (c.prior.length > prior.length) c.chart.removeSeries(c.prior.pop()!);
    prior.forEach((s, i) => {
      if (!c.prior[i]) {
        c.prior[i] = c.chart.addSeries(LineSeries, {
          color: PRIOR_GREYS[Math.min(i, PRIOR_GREYS.length - 1)], lineWidth: 1, lineStyle: LineStyle.Dotted,
          lastValueVisible: false, priceLineVisible: false,
        });
      }
      const shift = Math.round((dayStart(today) - dayStart(s.d)) / DAY) * DAY;
      c.prior[i].applyOptions({ title: s.d.slice(5) });
      c.prior[i].setData(s.pts.map((p) => ({ time: t(p[0] + shift), value: p[3] })));
    });
    const key = `${und}|${x}|${nDays}|${kArg}`;
    if (fitted.current !== key && data.sessions.length) {
      fitted.current = key;
      c.chart.timeScale().fitContent();
    }
  }, [data, today, und, x, nDays, kArg, ready]);

  // The live point, from the surface, on the current minute.
  useFrame(() => {
    const c = g.current;
    const s = sSubj ? feed.store.get<IvSurface>(sSubj) : undefined;
    if (!c || !s?.rows.length || !data) return;
    const w = atmRow(s);
    if (!w) return;
    const lastTs = data.sessions.find((d) => d.d === today)?.pts.at(-1)?.[0] ?? 0;
    const at = Math.floor(s.ts / 60_000) * 60;
    if (at < lastTs) return; // never draw behind the stored history
    const time = (at + IST) as UTCTimestamp;
    c.atm.update({ time, value: w.c!.mid + w.p!.mid });
    c.fwd.update({ time, value: s.f });
    const fk = data.k != null ? straddleAt(s, data.k) : null;
    if (fk != null) c.fixed.update({ time, value: fk });
  }, 2);

  // The header's numbers.
  const v = usePoll(() => {
    const s = sSubj ? feed.store.get<IvSurface>(sSubj) : undefined;
    const w = s?.rows.length ? atmRow(s) : undefined;
    return { s, k: w?.k, str: w ? w.c!.mid + w.p!.mid : null, fk: s && data?.k != null ? straddleAt(s, data.k) : null };
  }, 2);

  if (!r) return <Msg text="loading reference data…" />;
  if (!ready) return <Msg text={`${und || "no security"}: no options`} />;

  const s = v.s;
  const cur = data?.sessions.find((d) => d.d === today)?.pts ?? [];
  const open = cur[0];
  // The previous session's ATM straddle at this time of day, for the decay comparison.
  const prev = data?.sessions.filter((d) => d.d < today).at(-1);
  let prevAt: number | null = null;
  if (prev && s) {
    const tod = (Math.floor(s.ts / 1000) + IST) % DAY;
    for (const p of prev.pts) if ((p[0] + IST) % DAY <= tod) prevAt = p[3];
  }
  const shown = xs.slice(0, EXPIRY_BUTTONS);
  if (x && !shown.includes(x)) shown.push(x);
  const keep = (args ?? []).filter((a) => !expiryArg(und, a, r));

  return (
    <div className="gp">
      <div className="omon-head">
        <span className="name">{und}</span>
        {shown.map((e) => (
          <button key={e} className={e === x ? "on" : ""} onClick={() => setArgs(e === xs[0] ? keep : [e, ...keep])}>{e.slice(5)}</button>
        ))}
        {err && <span className="st-warn">{err}</span>}
        {s && v.str != null ? (
          <>
            <span><span className="dim">ATM </span>{num(v.k, 0)}</span>
            <span>
              <span className="dim">straddle </span><span className="amber">{num(v.str, 2)}</span>{" "}
              {open && <Chg d={v.str - open[3]} base={open[3]} dp={2} />}
            </span>
            <span><span className="dim">BE </span>{num(v.k! - v.str, 0)} – {num(v.k! + v.str, 0)}</span>
            <span><span className="dim">move </span>±{num((100 * v.str) / s.f, 2)}%</span>
            {s.atm != null && <span><span className="dim">1σ </span>±{num(s.f * s.atm * Math.sqrt(s.tte), 0)}</span>}
            {prevAt != null && (
              <span><span className="dim">vs {prev!.d.slice(5)} same time </span><Chg d={v.str - prevAt} base={prevAt} dp={2} /></span>
            )}
            {data?.k != null && (
              <span><span className="dim">{data.k} </span><span className="blue">{num(v.fk, 2)}</span></span>
            )}
            <span><span className="dim">DTE </span>{num(s.dte, 2)}</span>
            <span className={since(s.ts) > STALE_MS ? "st-warn" : "dim"}>{age(since(s.ts))}</span>
          </>
        ) : (
          !err && <span className="dim">waiting for {sSubj}</span>
        )}
        {note && <span className="st-warn">{note}</span>}
      </div>
      <div className="gp-chart" ref={box} />
    </div>
  );
}
