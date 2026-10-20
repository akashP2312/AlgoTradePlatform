// Q: one contract's quote. Last and change on the previous close, the touch and
// five levels of depth (quantities in lots), the day's range, volume, ATP and OI,
// and for an option its IV and greeks from the IV engine's row at that strike.

import { subjectOf, isOption } from "../data/contract";
import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { nearFuture, useRefData } from "../data/ref";
import type { IvSurface, Tick } from "../data/types";
import { age, clock, num } from "../fmt";
import { Chg, ivSubject, lots, Msg, since, STALE_MS, useResolved } from "./common";

export function QPanel({ sec, args }: { sec?: string; args?: string[] }) {
  const res = useResolved(sec, args);
  const r = useRefData();
  const ok = !("error" in res);
  const subj = ok ? subjectOf(res.c) : "";
  const opt = ok && isOption(res.kind);
  useSub(ok ? (opt ? [subj, ivSubject(res.und, res.x)] : [subj]) : []);
  const v = usePoll(() => {
    if (!ok) return null;
    const t = feed.store.get<Tick>(subj);
    const s = opt ? feed.store.get<IvSurface>(ivSubject(res.und, res.x)) : undefined;
    return { t, row: s?.rows.find((w) => w.k === res.k), s, a: since(t?.r) };
  }, 8);
  if (!ok) return <Msg text={res.error} />;
  const { t, row, s, a } = v!;
  const dp = res.dp;
  // An index or equity has no lot; its quantities read in its near future's lots.
  const lot = res.lot || nearFuture(res.und, r)?.lot || 0;
  const name = res.c.split("|").filter(Boolean).slice(1).join(" ");
  if (!t) return <Msg text={`${name}: waiting for the first tick…`} />;
  const stale = a > STALE_MS;
  const leg = opt ? (res.kind === "CE" ? row?.c : row?.p) : undefined;
  const d = t.d;

  return (
    <div className={`panel-body q ${stale ? "stale-all" : ""}`}>
      <div className="q-head">
        <span className="name">{name}</span>
        <span className={`last n ${stale ? "stale" : ""}`}>{num(t.p, dp)}</span>
        <Chg d={t.cl ? t.p - t.cl : null} base={t.cl} dp={dp} />
        <span className={stale ? "state st-warn" : "dim"}>{stale ? `STALE ${age(a)}` : age(a)}</span>
      </div>
      <div className="q-cols">
        <table className="kv">
          <tbody>
            {t.b != null && (
              <>
                <tr><th>Bid</th><td className="n">{num(t.b, dp)} <span className="dim">× {lots(t.bq, res.lot)}</span></td></tr>
                <tr><th>Ask</th><td className="n">{num(t.a, dp)} <span className="dim">× {lots(t.aq, res.lot)}</span></td></tr>
                <tr><th>Spread</th><td className="n">{t.a && t.b ? `${num(t.a - t.b, dp)} · ${num((200 * (t.a - t.b)) / (t.a + t.b), 2)}%` : "—"}</td></tr>
              </>
            )}
            <tr><th>Open</th><td className="n">{num(t.o, dp)}</td></tr>
            <tr><th>High</th><td className="n">{num(t.h, dp)}</td></tr>
            <tr><th>Low</th><td className="n">{num(t.lo, dp)}</td></tr>
            <tr><th>Prev close</th><td className="n">{num(t.cl, dp)}</td></tr>
          </tbody>
        </table>
        <table className="kv">
          <tbody>
            {t.v != null && <tr><th>Volume</th><td className="n">{lots(t.v, res.lot)} <span className="dim">{res.lot ? "lots" : ""}</span></td></tr>}
            {t.ap != null && <tr><th>ATP</th><td className="n">{num(t.ap, dp)}</td></tr>}
            {t.oi != null && (
              <tr><th>OI</th><td className="n">{lots(t.oi, lot)} <span className="dim">lots · {lots(t.ol, lot)}–{lots(t.oh, lot)}</span></td></tr>
            )}
            {t.tb != null && <tr><th>Total bid/ask</th><td className="n">{lots(t.tb, res.lot)} / {lots(t.ts, res.lot)}</td></tr>}
            {t.lt != null && <tr><th>Last trade</th><td className="n">{clock(t.lt)} <span className="dim">× {lots(t.q, res.lot)}</span></td></tr>}
            {t.e != null && <tr><th>Exch time</th><td className="n">{clock(t.e)}</td></tr>}
            <tr><th>Received</th><td className="n">{clock(t.r)}</td></tr>
            {res.lot > 0 && <tr><th>Lot · tick</th><td className="n">{res.lot} · {res.tick}</td></tr>}
          </tbody>
        </table>
        {opt && (
          <table className="kv">
            <tbody>
              <tr><th>IV</th><td className="n">{leg?.iv != null ? `${num(leg.iv * 100, 2)}%` : "—"} <span className="dim">fit {row?.fit != null ? num(row.fit * 100, 2) : "—"}</span></td></tr>
              <tr><th>Delta</th><td className="n">{num(leg?.delta, 3)}</td></tr>
              <tr><th>Gamma</th><td className="n">{num(leg?.gamma, 5)}</td></tr>
              <tr><th>Vega/lot</th><td className="n">{leg?.vega != null ? num(leg.vega * res.lot, 0) : ""} <span className="dim">₹ per vol pt</span></td></tr>
              <tr><th>Theta/lot</th><td className="n">{leg?.theta != null ? num(leg.theta * res.lot, 0) : ""} <span className="dim">₹ per day</span></td></tr>
              <tr><th>Forward</th><td className="n">{num(s?.f, 2)} <span className="dim">{s?.fsrc}</span></td></tr>
              <tr><th>DTE</th><td className="n">{num(s?.dte, 2)} <span className="dim">trading days</span></td></tr>
            </tbody>
          </table>
        )}
      </div>
      {d && (d.b.length > 0 || d.s.length > 0) && (
        <>
          <h3>Depth <span className="dim">{res.lot ? "(lots)" : ""}</span></h3>
          <table className="depth">
            <thead>
              <tr><th>Orders</th><th>Qty</th><th>Bid</th><th>Ask</th><th>Qty</th><th>Orders</th></tr>
            </thead>
            <tbody>
              {Array.from({ length: 5 }, (_, i) => {
                const b = d.b[i];
                const s = d.s[i];
                return (
                  <tr key={i}>
                    <td className="dim">{b?.n || ""}</td>
                    <td>{b?.q ? lots(b.q, res.lot) : ""}</td>
                    <td className="bid">{b?.q ? num(b.p, dp) : ""}</td>
                    <td className="ask">{s?.q ? num(s.p, dp) : ""}</td>
                    <td>{s?.q ? lots(s.q, res.lot) : ""}</td>
                    <td className="dim">{s?.n || ""}</td>
                  </tr>
                );
              })}
              <tr className="tot">
                <td /><td>{lots(d.b.reduce((x, l) => x + l.q, 0), res.lot)}</td><td /><td />
                <td>{lots(d.s.reduce((x, l) => x + l.q, 0), res.lot)}</td><td />
              </tr>
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}
