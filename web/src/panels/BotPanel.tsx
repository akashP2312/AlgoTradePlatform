// BOT: the algo-trading bot as botmon reads it from the bot's own SQLite ledger
// (bot.<name>.state). The heartbeat says whether the daily run landed for the
// trading day it should have; the rest is the ledger's book as of that run. In
// paper mode the ledger is the only book, so there is nothing to reconcile and the
// recon reads "not compared". Read-only: nothing here, or in botmon, places an order.

import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { age, clock, inr, num } from "../fmt";
import { Chg, Msg, since } from "./common";

const SUBJ = "bot.rotation.state";
/** botmon publishes every 15s; past this the service is down. */
const STALE_MS = 45_000;

interface Rule { rule: string; lvl: "ok" | "warn" | "crit"; since?: number; msg?: string }
interface Pos {
  sym: string; engine: string; qty: number; entry_date: string; entry: number; stop: number; high: number; init_risk: number;
  mark?: number; pnl_pct?: number; value?: number; pnl?: number; to_stop?: number;
}
interface Order { id: number; created: string; sym: string; side: string; qty: number; stop?: number; engine: string; reason: string; status: string }
interface Trade {
  sym: string; engine: string; qty: number; entry_date: string; entry: number; exit_date: string; exit: number;
  pnl: number; r?: number; reason: string; days: number;
}
interface ReconItem { sym: string; ledger: number; broker: number; note: "pending" | "mismatch" | "extra" }
interface State {
  ts: number; name: string; mode: string; read_at?: number; err?: string; as_of?: string; expected: string; behind: number;
  cash?: number; equity?: number; month_start?: number; month_pct?: number;
  positions: Pos[]; pending: Order[]; trades: Trade[]; curve: { d: string; eq: number; cash: number; n: number }[];
  realised: number; n_trades: number;
  recon: { status: "not_compared" | "waiting" | "ok" | "mismatch"; at?: number; msg?: string; items?: ReconItem[] };
  rules: Rule[];
}

const lvlCls = (l?: string) => (l === "crit" ? "st-bad" : l === "warn" ? "st-warn" : "st-ok");
const RECON: Record<State["recon"]["status"], [string, string]> = {
  not_compared: ["not compared", "dim"],
  waiting: ["waiting for the broker", "dim"],
  ok: ["matches the broker", "st-ok"],
  mismatch: ["differs from the broker", "st-warn"],
};

export function BotPanel() {
  useSub([SUBJ]);
  const s = usePoll(() => feed.store.get<State>(SUBJ), 2);
  if (!s) return <Msg text={`waiting for ${SUBJ} (is botmon running, with bot.dir set?)`} />;

  const pubAge = since(s.ts);
  const hb = s.rules.find((r) => r.rule === "bot_heartbeat");
  const bad = s.rules.filter((r) => r.lvl !== "ok");
  const [rtext, rcls] = RECON[s.recon.status] ?? [s.recon.status, "dim"];
  const open = s.positions.reduce((v, p) => v + (p.value ?? 0), 0);

  return (
    <div className="gp">
      <div className="omon-head">
        <span className="name">BOT {s.name.toUpperCase()}</span>
        <span className={s.mode === "live" ? "st-warn" : "dim"}>{s.mode}</span>
        <span className={lvlCls(hb?.lvl)} title={hb?.msg}>
          {s.as_of ? `ran for ${s.as_of}` : "never ran"}
          {s.behind > 0 && ` · ${s.behind} day${s.behind > 1 ? "s" : ""} behind`}
        </span>
        <span className="dim" title="the trading day whose run should have landed by now">due {s.expected}</span>
        <span className={rcls} title={s.recon.msg}>recon: {rtext}</span>
        {s.err && <span className="st-bad" title={s.err}>ledger: {s.err.length > 40 ? `${s.err.slice(0, 40)}…` : s.err}</span>}
        <span className="dim" title="last ledger read">{s.read_at ? `read ${age(since(s.read_at))}` : "never read"}</span>
        <span className={pubAge > STALE_MS ? "st-warn" : "dim"}>{age(pubAge)}</span>
      </div>
      <div className="omon-head">
        <span className="name">EQUITY</span>
        <span>{s.equity == null ? "—" : inr(s.equity)}</span>
        {s.month_pct != null && <span><span className="dim">month </span><Chg d={s.month_pct} />%</span>}
        <span><span className="dim">cash </span>{s.cash == null ? "—" : inr(s.cash)}</span>
        <span><span className="dim">open </span>{inr(open)}</span>
        <span><span className="dim">realised </span><Chg d={s.realised} dp={0} /><span className="dim"> over {s.n_trades} trades</span></span>
      </div>
      {bad.length > 0 && (
        <div className="omon-head">
          {bad.map((r) => (
            <span key={r.rule} className={lvlCls(r.lvl)} title={r.since ? `since ${clock(r.since)}` : undefined}>{r.msg ?? r.rule}</span>
          ))}
        </div>
      )}
      <div className="panel-body mon">
        <Curve pts={s.curve} />
        <h4 className="dim">Positions, marked at the last run</h4>
        {s.positions.length === 0 ? <p className="dim">flat</p> : (
          <table className="grid">
            <thead><tr>
              <th className="l">Symbol</th><th className="l">Engine</th><th>Qty</th><th className="l">Since</th><th>Entry</th>
              <th>Mark</th><th>P&L %</th><th>P&L</th><th>Value</th><th>Stop</th><th title="% the mark sits above the stop">To stop</th>
            </tr></thead>
            <tbody>
              {s.positions.map((p) => (
                <tr key={p.sym}>
                  <td className="l">{p.sym}</td><td className="l dim">{p.engine}</td><td className="n">{num(p.qty, 0)}</td>
                  <td className="l dim">{p.entry_date}</td><td className="n">{num(p.entry, 2)}</td>
                  <td className="n">{p.mark == null ? "—" : num(p.mark, 2)}</td>
                  <td className="n"><Chg d={p.pnl_pct} /></td><td className="n"><Chg d={p.pnl} dp={0} /></td>
                  <td className="n">{p.value == null ? "—" : inr(p.value)}</td><td className="n dim">{num(p.stop, 2)}</td>
                  <td className={`n ${p.to_stop != null && p.to_stop < 3 ? "st-warn" : "dim"}`}>{p.to_stop == null ? "—" : `${num(p.to_stop, 1)}%`}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {s.pending.length > 0 && (<>
          <h4 className="dim">Orders the bot has proposed (it places them; the terminal does not)</h4>
          <table className="grid">
            <thead><tr><th className="l">Created</th><th className="l">Side</th><th className="l">Symbol</th><th>Qty</th><th>Stop</th><th className="l">Reason</th><th className="l">Status</th></tr></thead>
            <tbody>
              {s.pending.map((o) => (
                <tr key={o.id}>
                  <td className="l dim">{o.created}</td><td className={`l ${o.side === "BUY" ? "up" : "dn"}`}>{o.side}</td><td className="l">{o.sym}</td>
                  <td className="n">{num(o.qty, 0)}</td><td className="n dim">{o.stop == null ? "" : num(o.stop, 2)}</td>
                  <td className="l dim">{o.reason}</td><td className="l">{o.status}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>)}
        {(s.recon.items?.length ?? 0) > 0 && (<>
          <h4 className="dim">Ledger vs broker</h4>
          <table className="grid">
            <thead><tr><th className="l">Symbol</th><th>Ledger</th><th>Broker</th><th className="l">Note</th></tr></thead>
            <tbody>
              {s.recon.items!.map((it) => (
                <tr key={it.sym}>
                  <td className="l">{it.sym}</td><td className="n">{num(it.ledger, 0)}</td><td className="n">{num(it.broker, 0)}</td>
                  <td className={`l ${it.note === "mismatch" ? "st-warn" : "dim"}`}>
                    {it.note === "pending" ? "order open" : it.note === "extra" ? "broker holds more (not the bot's)" : "broker holds less"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>)}
        <h4 className="dim">Recent trades</h4>
        {s.trades.length === 0 ? <p className="dim">none</p> : (
          <table className="grid">
            <thead><tr>
              <th className="l">Symbol</th><th>Qty</th><th className="l">In</th><th>Entry</th><th className="l">Out</th><th>Exit</th>
              <th>P&L</th><th>R</th><th>Days</th><th className="l">Reason</th>
            </tr></thead>
            <tbody>
              {s.trades.map((t, i) => (
                <tr key={`${t.sym}${t.exit_date}${i}`}>
                  <td className="l">{t.sym}</td><td className="n">{num(t.qty, 0)}</td><td className="l dim">{t.entry_date}</td>
                  <td className="n">{num(t.entry, 2)}</td><td className="l dim">{t.exit_date}</td><td className="n">{num(t.exit, 2)}</td>
                  <td className="n"><Chg d={t.pnl} dp={0} /></td><td className="n">{t.r == null ? "—" : num(t.r, 2)}</td>
                  <td className="n dim">{t.days}</td><td className="l dim">{t.reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

/** The ledger's last 30 equity points, one per run. */
function Curve({ pts }: { pts: State["curve"] }) {
  if (pts.length < 2) return null;
  const W = 600, H = 70, P = 4;
  const ys = pts.map((p) => p.eq);
  const lo = Math.min(...ys), hi = Math.max(...ys), span = hi - lo || 1;
  const X = (i: number) => P + ((W - 2 * P) * i) / (pts.length - 1);
  const Y = (y: number) => H - P - ((H - 2 * P) * (y - lo)) / span;
  const d = pts.map((p, i) => `${i ? "L" : "M"}${X(i).toFixed(1)},${Y(p.eq).toFixed(1)}`).join("");
  const up = ys[ys.length - 1] >= ys[0];
  return (
    <div title={`${pts[0].d} ${inr(ys[0])} → ${pts[pts.length - 1].d} ${inr(ys[ys.length - 1])}`}>
      <div className="dim" style={{ fontSize: 11 }}>
        equity, {pts.length} runs · {pts[0].d} – {pts[pts.length - 1].d} · low {inr(lo)} · high {inr(hi)}
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" style={{ width: "100%", height: H, display: "block" }}>
        <path d={d} fill="none" stroke={up ? "var(--green)" : "var(--red)"} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
      </svg>
    </div>
  );
}
