// MON: the market at a glance. The indices against their near futures (basis), VIX,
// GIFT NIFTY's premium to NIFTY, USDINR and crude, each index's ATM straddle and IV
// on its nearest expiry, then the heavyweights. Extra securities typed as args
// (MON TCS INFY) are added at the bottom. Double-click a row for its quote.

import { subjectOf } from "../data/contract";
import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { expiries, resolve, useRefData, type Ref, type Resolved } from "../data/ref";
import type { IvSurface, Tick } from "../data/types";
import { security } from "../cmd/registry";
import { run } from "../shell";
import { age, num } from "../fmt";
import { Chg, ivSubject, Msg, since, STALE_MS } from "./common";

const INDICES = ["NIFTY", "BANKNIFTY"];
const MACRO = ["INDIAVIX", "GIFT NIFTY", "USDINR", "CRUDEOIL"];
const HEAVY = ["RELIANCE", "HDFCBANK", "ICICIBANK", "INFY", "TCS"];

interface Row {
  label: string;
  sec: string;
  res: Resolved;
  // Against another row's last: basis to the index, GIFT's premium to NIFTY.
  vs?: { label: string; c: string };
}

function rows(r: Ref, extra: string[]): Row[] {
  const out: Row[] = [];
  const add = (sec: string, args: string[] = [], label = sec, vs?: Row["vs"]) => {
    const res = resolve(sec, args, r);
    if (!("error" in res)) out.push({ label, sec, res, vs });
    return "error" in res ? undefined : res;
  };
  for (const u of INDICES) {
    const idx = add(u);
    add(u, ["FUT"], `${u} FUT1`, idx && { label: "basis", c: idx.c });
  }
  const nifty = out.find((w) => w.sec === "NIFTY" && w.res.kind === "IDX");
  for (const u of MACRO) add(u, [], u, u === "GIFT NIFTY" && nifty ? { label: "prem", c: nifty.res.c } : undefined);
  for (const u of [...HEAVY, ...extra]) if (!out.some((w) => w.sec === u)) add(u);
  return out;
}

const atmLeg = (s: IvSurface) => {
  let best = s.rows[0];
  for (const w of s.rows) if (Math.abs(w.k - s.f) < Math.abs(best.k - s.f)) best = w;
  return best;
};

export function MonPanel({ args }: { args?: string[] }) {
  const r = useRefData();
  const extra = (args ?? []).map(security);
  const list = r ? rows(r, extra) : [];
  const opts = r ? INDICES.map((u) => ({ u, x: expiries(u, r)[0] })).filter((o) => o.x) : [];
  useSub([...list.map((w) => subjectOf(w.res.c)), ...opts.map((o) => ivSubject(o.u, o.x))]);
  const v = usePoll(() => ({
    t: new Map(list.map((w) => [w.res.c, feed.store.get<Tick>(subjectOf(w.res.c))])),
    s: opts.map((o) => ({ ...o, s: feed.store.get<IvSurface>(ivSubject(o.u, o.x)) })),
  }), 4);
  if (!r) return <Msg text="loading reference data…" />;
  const missing = extra.filter((e) => !list.some((w) => w.sec === e));

  return (
    <div className="panel-body mon">
      <table className="grid">
        <thead>
          <tr>
            <th className="l">Security</th><th>Last</th><th>Chg</th><th>High</th><th>Low</th><th>vs</th><th>Age</th>
          </tr>
        </thead>
        <tbody>
          {list.map((w, i) => {
            const t = v.t.get(w.res.c);
            const a = since(t?.r);
            const stale = a < 0 || a > STALE_MS;
            const dp = w.res.dp;
            const o = w.vs && v.t.get(w.vs.c);
            const gap = INDICES.includes(w.sec) && w.res.kind === "IDX" && i > 0 ? "sep" : MACRO[0] === w.sec || HEAVY[0] === w.sec ? "sep" : "";
            return (
              <tr key={w.res.c} className={`${gap} ${stale ? "stale" : ""}`} onDoubleClick={() => run(`${w.sec.replaceAll(" ", "_")} Q${w.res.kind === "FUT" ? " FUT" : ""}`, true)} title={`${w.res.c} · double-click for Q`}>
                <td className="l">{w.label}</td>
                <td className="n">{num(t?.p, dp)}</td>
                <td className="n">{t && <Chg d={t.cl ? t.p - t.cl : null} base={t.cl} dp={dp} />}</td>
                <td className="n dim">{num(t?.h, dp)}</td>
                <td className="n dim">{num(t?.lo, dp)}</td>
                <td className="n">{t && o ? <><span className="dim">{w.vs!.label} </span><Chg d={t.p - o.p} dp={dp} /></> : ""}</td>
                <td className={`n ${stale ? "st-warn" : "dim"}`}>{age(a)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {missing.length > 0 && <p className="dim">not found: {missing.join(", ")}</p>}
      <h3>Nearest expiry</h3>
      <table className="grid">
        <thead>
          <tr>
            <th className="l">Index</th><th className="l">Expiry</th><th>DTE</th><th>Forward</th><th>ATM</th><th>Straddle</th><th>Move</th><th>ATM IV</th><th>RR25</th><th>BF25</th>
          </tr>
        </thead>
        <tbody>
          {v.s.map(({ u, x, s }) => {
            const w = s?.rows.length ? atmLeg(s) : undefined;
            const str = w?.c && w?.p ? w.c.mid + w.p.mid : null;
            return (
              <tr key={u} onDoubleClick={() => run(`${u} OMON`, true)} title="double-click for OMON">
                <td className="l">{u}</td>
                <td className="l">{x}</td>
                <td className="n">{num(s?.dte, 2)}</td>
                <td className="n">{num(s?.f, 2)}</td>
                <td className="n">{num(w?.k, 0)}</td>
                <td className="n">{num(str, 2)}</td>
                <td className="n">{str && s ? `±${num((100 * str) / s.f, 2)}%` : ""}</td>
                <td className="n">{s?.atm != null ? `${num(s.atm * 100, 2)}%` : ""}</td>
                <td className="n">{s?.rr25 != null ? num(s.rr25 * 100, 2) : ""}</td>
                <td className="n">{s?.bf25 != null ? num(s.bf25 * 100, 2) : ""}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
