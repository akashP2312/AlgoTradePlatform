// GP: one contract's intraday minute candles. History comes from /api/bars (the
// harvested Kite bar where there is one, else the recorder's live bar); the last
// bar then follows the ticks. Volume and OI sit in panes below. For an index or
// future, dashed lines mark the previous close ± one trading day's expected move
// at the nearest expiry's ATM IV. GP 3D shows the last three sessions.

import { useEffect, useRef, useState } from "react";
import {
  CandlestickSeries, createChart, HistogramSeries, LineSeries, LineStyle,
  type AutoscaleInfo, type IChartApi, type IPriceLine, type ISeriesApi, type UTCTimestamp,
} from "lightweight-charts";
import { subjectOf, isOption } from "../data/contract";
import { feed } from "../data/feed";
import { useFrame, useSub } from "../data/frame";
import { expiries, useRefData } from "../data/ref";
import type { IvSurface, Tick } from "../data/types";
import { num } from "../fmt";
import { ivSubject, Msg, useResolved } from "./common";

const IST = 19800; // the axis reads UTC, so bars are shifted to show IST
const REFETCH_MS = 60_000;

type Bar = [number, number, number, number, number, number, number | null];

interface Series {
  chart: IChartApi;
  px: ISeriesApi<"Candlestick">;
  vol: ISeriesApi<"Histogram">;
  oi: ISeriesApi<"Line">;
  bands: IPriceLine[];
  // The band prices, so autoscale keeps them on screen.
  span: [number, number] | null;
}

const css = (v: string) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();

export function GpPanel({ sec, args }: { sec?: string; args?: string[] }) {
  const d = (args ?? []).map((a) => /^(\d+)D$/i.exec(a)).find(Boolean);
  const nDays = d ? Math.min(10, Math.max(1, Number(d[1]))) : 1;
  const res = useResolved(sec, (args ?? []).filter((a) => !/^\d+D$/i.test(a)));
  const r = useRefData();
  const ok = !("error" in res);
  const c = ok ? res.c : "";
  const subj = ok ? subjectOf(c) : "";
  // Bands need an IV: the underlying's nearest expiry, for an index or its future.
  const nx = ok && r && !isOption(res.kind) ? expiries(res.und, r)[0] : undefined;
  const ivSubj = ok && nx ? ivSubject(res.und, nx) : "";
  useSub([subj, ivSubj].filter(Boolean));

  const box = useRef<HTMLDivElement>(null);
  const s = useRef<Series | null>(null);
  const last = useRef<{ bar: Bar; v?: number } | null>(null);
  const [note, setNote] = useState("loading bars…");

  // The chart lives as long as the panel shows one; contracts swap its data.
  useEffect(() => {
    if (!ok || !box.current) return;
    const chart = createChart(box.current, {
      autoSize: true,
      layout: { background: { color: css("--bg") }, textColor: css("--dim"), fontFamily: css("--mono"), fontSize: 11 },
      grid: { vertLines: { color: "#161b22" }, horzLines: { color: "#161b22" } },
      rightPriceScale: { borderColor: css("--line") },
      timeScale: { borderColor: css("--line"), timeVisible: true, secondsVisible: false, lockVisibleTimeRangeOnResize: true },
      crosshair: { mode: 0 },
    });
    const up = css("--green");
    const dn = css("--red");
    const px = chart.addSeries(CandlestickSeries, {
      upColor: up, downColor: dn, wickUpColor: up, wickDownColor: dn, borderVisible: false,
      autoscaleInfoProvider: (base: () => AutoscaleInfo | null) => {
        const a = base();
        const b = s.current?.span;
        if (!a?.priceRange || !b) return a;
        return { ...a, priceRange: { minValue: Math.min(a.priceRange.minValue, b[1]), maxValue: Math.max(a.priceRange.maxValue, b[0]) } };
      },
    });
    const vol = chart.addSeries(HistogramSeries, { color: "#3a4352", priceFormat: { type: "volume" } }, 1);
    const oi = chart.addSeries(LineSeries, { color: css("--blue"), lineWidth: 1, priceFormat: { type: "volume" } }, 2);
    const panes = chart.panes();
    panes[0].setStretchFactor(0.7);
    panes[1]?.setStretchFactor(0.15);
    panes[2]?.setStretchFactor(0.15);
    s.current = { chart, px, vol, oi, bands: [], span: null };
    return () => {
      chart.remove();
      s.current = null;
    };
  }, [ok]);

  // History, refetched every minute so recorder bars replace the tick-built ones.
  useEffect(() => {
    if (!c) return;
    let dead = false;
    last.current = null;
    let first = true;
    const load = async () => {
      try {
        const rsp = await fetch(`/api/bars?c=${encodeURIComponent(c)}&days=${nDays}`);
        const body = await rsp.json();
        if (dead || !s.current) return;
        if (!rsp.ok) return setNote(body.error ?? `bars: HTTP ${rsp.status}`);
        const bars: Bar[] = body.bars;
        const t = (b: Bar) => (b[0] + IST) as UTCTimestamp;
        s.current.px.setData(bars.map((b) => ({ time: t(b), open: b[1], high: b[2], low: b[3], close: b[4] })));
        s.current.vol.setData(bars.map((b) => ({ time: t(b), value: b[5], color: b[4] >= b[1] ? "#1f4d36" : "#5a2526" })));
        s.current.oi.setData(bars.filter((b) => b[6] != null).map((b) => ({ time: t(b), value: b[6]! })));
        const prev = last.current;
        const end = bars[bars.length - 1];
        // Keep the tick-built bar if it is newer than the last stored one.
        last.current = prev && end && prev.bar[0] > end[0] ? prev : end ? { bar: [...end], v: prev?.v } : prev;
        if (first) s.current.chart.timeScale().fitContent();
        first = false;
        setNote(bars.length ? "" : "no bars yet: drawing from ticks");
      } catch {
        if (!dead) setNote("bars: gateway unreachable");
      }
    };
    load();
    const id = setInterval(load, REFETCH_MS);
    return () => {
      dead = true;
      clearInterval(id);
    };
  }, [c, nDays, ok]);

  // Ticks move the last bar, or open the next one when the minute turns.
  useFrame(() => {
    const g = s.current;
    if (!g || !subj) return;
    const t = feed.store.get<Tick>(subj);
    if (t?.p) {
      const at = Math.floor((t.e || t.r) / 60_000) * 60;
      const l = last.current;
      let bar: Bar;
      if (!l || at > l.bar[0]) bar = [at, t.p, t.p, t.p, t.p, 0, t.oi ?? null];
      else if (at === l.bar[0]) bar = [at, l.bar[1], Math.max(l.bar[2], t.p), Math.min(l.bar[3], t.p), t.p, l.bar[5], t.oi ?? l.bar[6]];
      else bar = l.bar; // a late tick for a closed minute
      if (l?.v != null && t.v != null && t.v > l.v && bar[0] === at) bar[5] += t.v - l.v;
      last.current = { bar, v: t.v ?? l?.v };
      const time = (bar[0] + IST) as UTCTimestamp;
      g.px.update({ time, open: bar[1], high: bar[2], low: bar[3], close: bar[4] });
      g.vol.update({ time, value: bar[5], color: bar[4] >= bar[1] ? "#1f4d36" : "#5a2526" });
      if (bar[6] != null) g.oi.update({ time, value: bar[6] });
    }
    const sf = ivSubj ? feed.store.get<IvSurface>(ivSubj) : undefined;
    const cl = t?.cl;
    if (sf?.atm && sf.dte > 0 && cl) {
      const m = cl * sf.atm * Math.sqrt(sf.tte / sf.dte);
      const lines: [number, number] = [cl + m, cl - m];
      g.span = lines;
      if (!g.bands.length) {
        g.bands = lines.map((price, i) => g.px.createPriceLine({
          price, color: css("--amber"), lineWidth: 1, lineStyle: LineStyle.Dashed, axisLabelVisible: true, title: i ? "−1d σ" : "+1d σ",
        }));
      } else g.bands.forEach((b, i) => b.applyOptions({ price: lines[i] }));
    }
  }, 4);

  if (!ok) return <Msg text={res.error} />;
  const name = c.split("|").filter(Boolean).slice(1).join(" ");
  return (
    <div className="gp">
      <div className="gp-head">
        <span className="name">{name}</span>
        <span className="dim">1m · {nDays === 1 ? "today" : `${nDays} sessions`}{ivSubj ? ` · bands ±1d at ${nx} ATM IV` : ""}</span>
        {note && <span className="st-warn">{note}</span>}
        {res.dp != null && <span className="dim">tick {num(res.tick, res.dp)}</span>}
      </div>
      <div className="gp-chart" ref={box} />
    </div>
  );
}
