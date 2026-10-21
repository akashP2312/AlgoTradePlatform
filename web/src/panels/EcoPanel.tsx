// ECO: the scheduled event calendar newsmon works from (config/news/events.yaml,
// plus F&O expiries derived from the listed instruments), and the risk state it
// gives right now. VERIFY marks a date or time not confirmed against the official
// source; TBC a date not yet announced. Untimed events open no window.

import { feed } from "../data/feed";
import { usePoll, useSub } from "../data/frame";
import { countdown, LEVEL_TEXT, levelCls, reasonKey, stale, SUBJ_EVENTS, SUBJ_RISK, type Calendar, type EventRow, type RiskState } from "../data/news";
import { age, clock } from "../fmt";
import { Msg, since } from "./common";

const IMPACT_CLS: Record<EventRow["impact"], string> = { HIGH: "dn", MEDIUM: "", LOW: "dim" };

function when(e: EventRow): [string, string] {
  if (e.tbc) return ["TBC", ""];
  if (!e.at) return [e.date ?? "", "time n/a"];
  const d = new Date(e.at + 5.5 * 3600_000).toISOString();
  return [d.slice(0, 10), `${d.slice(11, 16)}`];
}

export function EcoPanel() {
  useSub([SUBJ_RISK, SUBJ_EVENTS]);
  const v = usePoll(() => ({
    r: feed.store.get<RiskState>(SUBJ_RISK),
    c: feed.store.get<Calendar>(SUBJ_EVENTS),
    now: feed.serverNow(),
  }), 1);
  const { r, c, now } = v;
  if (!c) return <Msg text={`waiting for ${SUBJ_EVENTS} (is newsmon running?)`} />;
  const active = new Set((r && !stale(r) ? r.reasons : []).flatMap((x) => (x.event ? [x.event] : [])));

  return (
    <div className="gp">
      <div className="omon-head">
        <span className="name">ECO</span>
        {stale(r) ? <span className="state st-quiet">RISK ? (newsmon quiet)</span>
          : <span className={`state ${levelCls(r!.state)}`}>RISK {LEVEL_TEXT[r!.state]}</span>}
        {r && !stale(r) && r.expires_at ? <span className="dim">changes in {countdown(r.expires_at - now)} at {clock(r.expires_at)}</span> : null}
        {r?.next_high && <span className="dim">next HIGH: {r.next_high.name} in {countdown(r.next_high.at - now)}</span>}
        {c.err && <span className="st-bad" title={c.err}>event file not loaded, previous events kept</span>}
        <span className="dim" title="when the event file was last read">{c.loaded ? `file read ${age(since(c.loaded))} ago` : "file never read"}</span>
      </div>
      {r && !stale(r) && r.reasons.length > 0 && (
        <div className="omon-head">
          {r.reasons.map((x) => <span key={reasonKey(x)} className={levelCls(x.level)} title={`until ${clock(x.until)}`}>{x.msg}</span>)}
        </div>
      )}
      <div className="panel-body mon">
        <p className="dim">A filter on new entries, never a trade signal. Times IST. VERIFY: not yet checked against the official source. Edit config/news/events.yaml; newsmon reloads it within seconds.</p>
        <table className="grid">
          <thead><tr>
            <th className="l">Date</th><th className="l">IST</th><th className="l">Impact</th><th className="l">Event</th>
            <th className="l">Ctry</th><th className="l">Category</th><th className="l">Affects</th><th className="l">Note</th>
          </tr></thead>
          <tbody>
            {c.events.map((e) => {
              const [d, t] = when(e);
              const past = e.at != null && e.at < now;
              return (
                <tr key={e.id} className={past && !active.has(e.id) ? "stale" : undefined}>
                  <td className="l" style={{ whiteSpace: "nowrap" }}>{d}</td>
                  <td className="l">{t}</td>
                  <td className={`l ${IMPACT_CLS[e.impact]}`}>{e.impact}</td>
                  <td className={`l ${active.has(e.id) ? "st-warn" : ""}`}>
                    {e.source ? <a href={e.source} target="_blank" rel="noreferrer" style={{ color: "inherit" }}>{e.name}</a> : e.name}
                    {e.verify && <span className="eco-badge" style={{ color: "var(--warn)" }} title="date/time not confirmed against the official source">VERIFY</span>}
                    {e.tbc && <span className="eco-badge dim" title="date not announced; opens no window">TBC</span>}
                    {e.derived && <span className="eco-badge dim" title="derived from the listed instruments">AUTO</span>}
                  </td>
                  <td className="l dim">{e.country}</td>
                  <td className="l dim">{e.category}</td>
                  <td className="l dim">{e.affects.join(" ")}</td>
                  <td className="l dim detail">{e.note}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}
