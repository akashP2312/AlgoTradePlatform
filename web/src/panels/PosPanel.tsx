// POS: the broker's positions, from book.positions (polled every 5s by the book
// service), repriced live from the terminal's own ticks, with the margins strip
// above. The check column reprices each row at the broker's own last price and
// compares with the broker's pnl and m2m: it must be zero, or the terminal's
// arithmetic (and every risk number built on it) is wrong. Double-click a row for
// its quote. Read-only: nothing here places or changes an order.

import { m2mAt, pnlAt, checkRow, sortBook } from "../data/book";
import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import type { Book, BookPos, Margins, Tick } from "../data/types";
import { run } from "../shell";
import { age, inr, num } from "../fmt";
import { Chg, lots, Msg, since, STALE_MS } from "./common";

const POS = "book.positions";
const MARGINS = "book.margins";
/** The book is polled every 5s; past this it is stale. */
const BOOK_STALE_MS = 15_000;

interface Live {
  r: BookPos;
  ltp: number; // the live tick when fresh, else the broker's
  live: boolean;
  pnl: number;
  m2m: number;
  chk: ReturnType<typeof checkRow>;
}

function quote(r: BookPos): string {
  const u = (r.u ?? "").replaceAll(" ", "_");
  if (r.k === "CE" || r.k === "PE") return `${u} Q ${r.K}${r.k} ${r.e}`;
  if (r.k === "FUT") return `${u} Q FUT`;
  return `${u || r.sym} Q`;
}

export function PosPanel() {
  const subs = usePoll(() => {
    const b = feed.store.get<Book>(POS);
    return (b?.net ?? []).flatMap((r) => (r.s && r.q !== 0 ? [r.s] : [])).sort().join(" ");
  }, 1);
  useSub([POS, MARGINS, ...(subs ? subs.split(" ") : [])]);

  const v = usePoll(() => {
    const b = feed.store.get<Book>(POS);
    const m = feed.store.get<Margins>(MARGINS);
    const rows: Live[] = sortBook(b?.net ?? []).map((r) => {
      const t = r.s ? feed.store.get<Tick>(r.s) : undefined;
      const a = since(t?.r);
      const live = !!t && a >= 0 && a <= STALE_MS && r.q !== 0;
      const ltp = live ? t!.p : r.p;
      return { r, ltp, live, pnl: pnlAt(r, ltp), m2m: m2mAt(r, ltp), chk: checkRow(r) };
    });
    return { b, m, rows };
  }, 4);

  const { b, m, rows } = v;
  if (!b) return <Msg text="waiting for book.positions (is the book service running and logged in?)" />;

  const bookAge = since(b.ts);
  const sum = (f: (w: Live) => number) => rows.reduce((s, w) => s + f(w), 0);
  const kitePnl = sum((w) => w.r.pnl);
  const livePnl = sum((w) => w.pnl);
  const liveM2m = sum((w) => w.m2m);
  const bad = rows.filter((w) => !w.chk.ok);
  const unknown = rows.filter((w) => !w.r.c && w.r.q !== 0);
  const open = rows.filter((w) => w.r.q !== 0).length;
  const util = m && m.net + m.used > 0 ? m.used / (m.net + m.used) : null;

  return (
    <div className="gp">
      <div className="omon-head">
        <span className="name">POSITIONS</span>
        <span><span className="dim">open </span>{open}<span className="dim"> / {rows.length}</span></span>
        <span><span className="dim">P&L </span><Chg d={livePnl} dp={0} /></span>
        <span><span className="dim">M2M </span><Chg d={liveM2m} dp={0} /></span>
        <span><span className="dim">broker </span><Chg d={kitePnl} dp={0} /></span>
        <span className={bad.length ? "st-bad" : "st-ok"} title="each row repriced at the broker's last price vs the broker's pnl and m2m">
          {bad.length ? `check: ${bad.length} off` : "check ok"}
        </span>
        {unknown.length > 0 && <span className="st-warn" title={unknown.map((w) => w.r.sym).join(", ")}>{unknown.length} not in refdata</span>}
        <span className={bookAge > BOOK_STALE_MS ? "st-warn" : "dim"}>{age(bookAge)}</span>
      </div>
      {m && (
        <div className="omon-head">
          <span className="name">MARGIN</span>
          {!m.on && <span className="st-warn">equity segment disabled</span>}
          <span><span className="dim">free </span>{inr(m.net)}</span>
          <span><span className="dim">used </span>{inr(m.used)}</span>
          {util != null && <span className={util > 0.8 ? "st-warn" : ""}><span className="dim">util </span>{num(100 * util, 1)}%</span>}
          <span><span className="dim">SPAN </span>{inr(m.span)}</span>
          <span><span className="dim">exposure </span>{inr(m.exp)}</span>
          {m.prem !== 0 && <span><span className="dim">premium </span>{inr(m.prem)}</span>}
          <span><span className="dim">cash </span>{inr(m.cash)}</span>
          <span><span className="dim">collateral </span>{inr(m.coll)}</span>
          {since(m.ts) > BOOK_STALE_MS && <span className="st-warn">{age(since(m.ts))}</span>}
        </div>
      )}
      <div className="panel-body mon">
        {rows.length === 0 ? (
          <p className="dim">no positions</p>
        ) : (
          <table className="grid">
            <thead>
              <tr>
                <th className="l">Contract</th><th className="l">Prod</th><th>Lots</th><th>Qty</th><th>Avg</th><th>Close</th>
                <th>LTP</th><th>P&L</th><th>M2M</th><th>Broker P&L</th><th>Broker LTP</th><th title="pnl and m2m repriced at the broker's LTP, minus the broker's">Check</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((w) => {
                const r = w.r;
                const closed = r.q === 0;
                const name = r.c ? (r.k === "FUT" ? `${r.u} FUT ${r.e?.slice(5)}` : r.K ? `${r.u} ${r.e?.slice(5)} ${num(r.K, 0)} ${r.k}` : r.u) : r.sym;
                return (
                  <tr key={`${r.x}:${r.sym}:${r.pr}`} className={closed ? "stale" : ""} onDoubleClick={() => r.c && run(quote(r), true)} title={`${r.x}:${r.sym}${r.c ? " · double-click for Q" : " · not in refdata"}`}>
                    <td className="l">{name}</td>
                    <td className="l dim">{r.pr}</td>
                    <td className="n">{closed ? "" : lots(r.q, r.lot ?? 0)}</td>
                    <td className={`n ${r.q > 0 ? "up" : r.q < 0 ? "dn" : "dim"}`}>{num(r.q, 0)}</td>
                    <td className="n">{closed ? "" : num(r.avg, 2)}</td>
                    <td className="n dim">{num(r.cl, 2)}</td>
                    <td className={`n ${w.live ? "" : "dim"}`} title={w.live ? "live tick" : "broker's last price (no fresh tick)"}>{num(w.ltp, 2)}</td>
                    <td className="n"><Chg d={w.pnl} dp={0} /></td>
                    <td className="n"><Chg d={w.m2m} dp={0} /></td>
                    <td className="n dim">{num(r.pnl, 0)}</td>
                    <td className="n dim">{num(r.p, 2)}</td>
                    <td className={`n ${w.chk.ok ? "dim" : "st-bad"}`} title={`pnl ${num(w.chk.pnl, 2)} · m2m ${num(w.chk.m2m, 2)}`}>
                      {w.chk.ok ? "0" : num(Math.abs(w.chk.pnl) >= Math.abs(w.chk.m2m) ? w.chk.pnl : w.chk.m2m, 2)}
                    </td>
                  </tr>
                );
              })}
              <tr className="sep">
                <td className="l dim" colSpan={7}>total</td>
                <td className="n"><Chg d={livePnl} dp={0} /></td>
                <td className="n"><Chg d={liveM2m} dp={0} /></td>
                <td className="n dim">{num(kitePnl, 0)}</td>
                <td className="n dim" colSpan={2}>live − broker <Chg d={livePnl - kitePnl} dp={0} /></td>
              </tr>
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
