// VOL: the EOD vol tables for an index, from /api/vol (refetched every few minutes;
// they change once a day, after the evening jobs). Realized vol by window and the HAR
// forecast of the next 21 days; the premium of iv30 and VIX over that forecast, with a
// year of history; the constant-maturity term structure and skew now, a week and a
// month ago; the move priced to each expiry against past moves; and the regime label
// with the readings behind it. Vols are annualized decimals, shown in vol points;
// pct is the share of the past year's days below today. A description, not a signal.

import { useEffect, useState } from "react";
import { num } from "../fmt";
import { Msg } from "./common";

const REFETCH_MS = 5 * 60_000;

type N = number | null;
interface Vol {
  und: string;
  rv: { d: string; win: number; cc: N; pk: N; gk: N; yz: N }[];
  har: { d: string; horizon: number; fc: N; lo: N; hi: N; rv21: N; vix: N }[];
  score: { d: string; model: string; n: number; qlike: N; mse: N; bias: N; hit: N }[];
  vrp: { d: string; iv30: N; vix: N; har_fc: N; rv21: N; iv_ante: N; vix_ante: N; iv_pct: N; vix_pct: N; n_iv: number; n_vix: number }[];
  iv: { d: string; iv30: N; iv_ante: N; iv_pct: N; n_iv: number }[];
  series: { d: string; iv30: N; vix: N; har_fc: N; rv21: N }[];
  term: { d: string; ago: number; tenor: number; atm_iv: N; rr25: N; bf25: N; atm_pct: N; rr25_pct: N; bf25_pct: N }[];
  events: { d: string; x: string; dte: number; atm_iv: N; implied_move: N; seg_vol: N; event_move: N; hist_move: N; hist_n: number; events: string | null }[];
  priced: { lo: number; hi: number; n: number; implied: N; realized: N; over: N }[];
  regime: { d: string; level_src: string | null; level_pct: N; term_slope: N; rv_ratio: N; premium: N; level: string | null; term: string | null; trend: string | null; prem: string | null; label: string; rule: string }[];
  labels: { d: string; label: string }[];
}

/** Vol points: 0.1532 as 15.32. */
const vp = (x: N | undefined, dp = 2) => (x == null ? "—" : num(100 * x, dp));
/** A signed vol-point difference. */
const svp = (x: N | undefined, dp = 2) => (x == null ? "—" : `${x >= 0 ? "+" : ""}${num(100 * x, dp)}`);
/** A 0..1 percentile as 0..100. */
const pc = (x: N | undefined) => (x == null ? "—" : num(100 * x, 0));

const LABEL_CLS: Record<string, string> = { stress: "dn", elevated: "amber", waking: "blue", calm: "up", normal: "dim" };
const LABEL_BG: Record<string, string> = { stress: "--red", elevated: "--amber", waking: "--blue", calm: "--green", normal: "--line" };
const AGO: Record<number, string> = { 0: "now", 5: "1w", 21: "1m" };

export function VolPanel({ sec }: { sec?: string }) {
  const und = sec ?? "";
  const [v, setV] = useState<Vol | null>(null);
  const [note, setNote] = useState("");

  useEffect(() => {
    if (!und) return;
    let live = true;
    const load = () =>
      fetch(`/api/vol?und=${encodeURIComponent(und)}`)
        .then(async (r) => {
          if (!r.ok) throw new Error(`${r.status} ${(await r.text()).trim()}`);
          return r.json() as Promise<Vol>;
        })
        .then((d) => { if (live) { setV(d); setNote(""); } })
        .catch((e) => { if (live) setNote(`/api/vol: ${e.message}`); });
    setV(null);
    load();
    const t = setInterval(load, REFETCH_MS);
    return () => { live = false; clearInterval(t); };
  }, [und]);

  if (!und) return <Msg text="VOL needs an index: NIFTY VOL" />;
  if (!v) return <Msg text={note || `loading the vol tables for ${und}…`} />;
  if (!v.rv.length && !v.vrp.length && !v.term.length) return <Msg text={`no EOD vol rows for ${und} yet (pmt-vol, pmt-ivhist)`} />;

  const g = v.regime[0];
  return (
    <div className="panel-body vol">
      <div className="q-head">
        <span className="name">{und} VOL</span>
        {g && <span className={LABEL_CLS[g.label] ?? ""} title={g.rule}>{g.label}</span>}
        <span className="dim">as of {v.har[0]?.d ?? v.vrp[0]?.d ?? v.rv[0]?.d}</span>
        {note && <span className="st-warn">{note}</span>}
      </div>
      <div className="q-cols">
        <Realized v={v} />
        <Premium v={v} />
        <Term v={v} />
      </div>
      <Chart v={v} />
      <div className="q-cols">
        <Events v={v} />
        <Regime v={v} />
      </div>
    </div>
  );
}

function Realized({ v }: { v: Vol }) {
  const h = v.har[0];
  return (
    <section>
      <h4>Realized and forecast</h4>
      <table className="grid">
        <thead><tr><th>days</th><th className="n" title="close to close">cc</th><th className="n" title="Parkinson">pk</th>
          <th className="n" title="Garman-Klass">gk</th><th className="n" title="Yang-Zhang">yz</th></tr></thead>
        <tbody>
          {v.rv.map((r) => (
            <tr key={r.win}><td>{r.win}</td><td className="n">{vp(r.cc)}</td><td className="n">{vp(r.pk)}</td>
              <td className="n">{vp(r.gk)}</td><td className="n">{vp(r.yz)}</td></tr>
          ))}
        </tbody>
      </table>
      {h && (
        <p>
          HAR, next {h.horizon} days: <b>{vp(h.fc)}</b> <span className="dim">({vp(h.lo)} to {vp(h.hi)})</span>
        </p>
      )}
      {v.score.length > 0 && (
        <table className="grid" title="out of sample, one forecast per day; lower QLIKE is better">
          <thead><tr><th>model</th><th className="n">n</th><th className="n">QLIKE</th><th className="n">bias</th></tr></thead>
          <tbody>
            {v.score.map((s) => (
              <tr key={s.model}><td>{s.model}</td><td className="n">{s.n}</td><td className="n">{num(s.qlike, 3)}</td>
                <td className="n">{svp(s.bias)}</td></tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Premium({ v }: { v: Vol }) {
  const p = v.vrp[0];
  const iv = v.iv[0];
  if (!p) return null;
  // iv30 can trail VIX by a session; show the latest one that exists, with its date.
  const ivRow = p.iv30 != null ? { d: p.d, iv30: p.iv30, iv_ante: p.iv_ante, iv_pct: p.iv_pct } : iv;
  return (
    <section>
      <h4>Premium over the forecast</h4>
      <table className="grid">
        <thead><tr><th></th><th className="n">level</th><th className="n">− HAR</th><th className="n">pct</th><th>as of</th></tr></thead>
        <tbody>
          {p.vix != null && (
            <tr><td>VIX</td><td className="n">{vp(p.vix)}</td><td className="n">{svp(p.vix_ante)}</td>
              <td className="n">{pc(p.vix_pct)}</td><td className="dim">{p.d}</td></tr>
          )}
          <tr><td>iv30</td><td className="n">{vp(ivRow?.iv30)}</td><td className="n">{svp(ivRow?.iv_ante)}</td>
            <td className="n">{pc(ivRow?.iv_pct)}</td><td className="dim">{ivRow?.d ?? "—"}</td></tr>
          <tr><td>HAR</td><td className="n">{vp(p.har_fc)}</td><td></td><td></td><td className="dim">{p.d}</td></tr>
          <tr><td>rv21</td><td className="n">{vp(p.rv21)}</td><td></td><td></td><td className="dim">trailing</td></tr>
        </tbody>
      </table>
    </section>
  );
}

function Term({ v }: { v: Vol }) {
  if (!v.term.length) return null;
  const tenors = [...new Set(v.term.map((t) => t.tenor))].sort((a, b) => a - b);
  const agos = [...new Set(v.term.map((t) => t.ago))].sort((a, b) => a - b);
  const at = (t: number, a: number) => v.term.find((r) => r.tenor === t && r.ago === a);
  return (
    <section>
      <h4>Term and skew</h4>
      <table className="grid">
        <thead>
          <tr><th>days</th>{agos.map((a) => <th key={a} className="n" title={at(tenors[0], a)?.d}>{AGO[a] ?? `${a}d`}</th>)}
            <th className="n">pct</th><th className="n" title="25-delta risk reversal">rr25</th><th className="n">pct</th>
            <th className="n" title="25-delta butterfly">bf25</th><th className="n">pct</th></tr>
        </thead>
        <tbody>
          {tenors.map((t) => {
            const n = at(t, 0);
            return (
              <tr key={t}>
                <td>{t}</td>
                {agos.map((a) => <td key={a} className="n">{vp(at(t, a)?.atm_iv)}</td>)}
                <td className="n dim">{pc(n?.atm_pct)}</td>
                <td className="n">{svp(n?.rr25)}</td><td className="n dim">{pc(n?.rr25_pct)}</td>
                <td className="n">{vp(n?.bf25)}</td><td className="n dim">{pc(n?.bf25_pct)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}

const W = 640, H = 120;
const LINES: { k: "iv30" | "vix" | "har_fc" | "rv21"; c: string; label: string }[] = [
  { k: "vix", c: "--amber", label: "VIX" },
  { k: "iv30", c: "--blue", label: "iv30" },
  { k: "har_fc", c: "--green", label: "HAR" },
  { k: "rv21", c: "--dim", label: "rv21" },
];

/** A year of iv30, VIX, the HAR forecast and trailing rv21, as plain SVG lines. */
function Chart({ v }: { v: Vol }) {
  const s = v.series;
  if (s.length < 2) return null;
  const lines = LINES.filter((l) => s.some((r) => r[l.k] != null));
  const vals = s.flatMap((r) => lines.map((l) => r[l.k])).filter((x): x is number => x != null);
  if (!vals.length) return null;
  const lo = Math.min(...vals), hi = Math.max(...vals);
  const x = (i: number) => (i / (s.length - 1)) * W;
  const y = (z: number) => H - ((z - lo) / (hi - lo || 1)) * H;
  const path = (k: (typeof LINES)[number]["k"]) => {
    let d = "", pen = false;
    s.forEach((r, i) => {
      const z = r[k];
      if (z == null) { pen = false; return; }
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(z).toFixed(1)}`;
      pen = true;
    });
    return d;
  };
  return (
    <section className="vol-chart">
      <h4>
        Past year{" "}
        {lines.map((l) => <span key={l.k} style={{ color: `var(${l.c})`, marginRight: 8 }}>{l.label}</span>)}
        <span className="dim">{vp(lo, 1)} to {vp(hi, 1)} · {s[0].d} to {s[s.length - 1].d}</span>
      </h4>
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" width="100%" height={H}>
        {lines.map((l) => <path key={l.k} d={path(l.k)} fill="none" stroke={`var(${l.c})`} strokeWidth={1.2} vectorEffect="non-scaling-stroke" />)}
      </svg>
    </section>
  );
}

function Events({ v }: { v: Vol }) {
  if (!v.events.length) return null;
  return (
    <section>
      <h4>Moves priced to expiry <span className="dim">{v.events[0].d}</span></h4>
      <table className="grid">
        <thead><tr><th>expiry</th><th className="n">dte</th><th className="n">iv</th><th className="n" title="atm_iv · √t · √(2/π), in %">implied</th>
          <th className="n" title="mean |move| over as many days, past two years">past</th><th className="n">ratio</th>
          <th className="n" title="forward vol of the segment since the previous expiry">seg vol</th>
          <th className="n" title="the segment's variance above the curve's typical rate, as a move">event</th><th>events</th></tr></thead>
        <tbody>
          {v.events.map((e) => (
            <tr key={e.x}>
              <td>{e.x}</td><td className="n">{num(e.dte, 0)}</td><td className="n">{vp(e.atm_iv)}</td>
              <td className="n">{vp(e.implied_move)}</td><td className="n">{vp(e.hist_move)}</td>
              <td className="n">{e.implied_move != null && e.hist_move ? num(e.implied_move / e.hist_move, 2) : "—"}</td>
              <td className="n">{vp(e.seg_vol)}</td><td className="n">{vp(e.event_move)}</td>
              <td className="dim">{e.events ?? ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {v.priced.length > 0 && (
        <table className="grid" title="every past fit whose expiry has passed">
          <thead><tr><th>priced vs realized</th><th className="n">n</th><th className="n">implied</th><th className="n">realized</th>
            <th className="n">ratio</th><th className="n" title="share of expiries whose move beat the implied move">beat</th></tr></thead>
          <tbody>
            {v.priced.map((b) => (
              <tr key={b.lo}>
                <td>{b.lo}–{b.hi} days</td><td className="n">{b.n}</td><td className="n">{vp(b.implied)}</td>
                <td className="n">{vp(b.realized)}</td>
                <td className="n">{b.implied != null && b.realized ? num(b.implied / b.realized, 2) : "—"}</td>
                <td className="n">{b.over == null ? "—" : `${num(100 * b.over, 0)}%`}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Regime({ v }: { v: Vol }) {
  const g = v.regime[0];
  if (!g) return null;
  return (
    <section>
      <h4>Regime <span className={LABEL_CLS[g.label] ?? ""}>{g.label}</span> <span className="dim">{g.d}</span></h4>
      <p className="dim">rule: {g.rule}</p>
      <table className="kv">
        <tbody>
          <tr><th>level</th><td>{g.level_src ?? "—"} pct {pc(g.level_pct)}</td><td className="dim">&nbsp;{g.level ?? "—"}</td></tr>
          <tr><th>term</th><td>63d − 21d {svp(g.term_slope)}</td><td className="dim">&nbsp;{g.term ?? "—"}</td></tr>
          <tr><th>trend</th><td>rv10 / rv63 {num(g.rv_ratio, 2) || "—"}</td><td className="dim">&nbsp;{g.trend ?? "—"}</td></tr>
          <tr><th>premium</th><td>vs HAR {svp(g.premium)}</td><td className="dim">&nbsp;{g.prem ?? "—"}</td></tr>
        </tbody>
      </table>
      {v.labels.length > 0 && (
        <div className="vol-strip" title={`the last ${v.labels.length} labelled days`}>
          {v.labels.map((l) => (
            <span key={l.d} title={`${l.d} ${l.label}`} style={{ background: `var(${LABEL_BG[l.label] ?? "--line"})` }} />
          ))}
        </div>
      )}
    </section>
  );
}
